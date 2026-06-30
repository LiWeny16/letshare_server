package handler

import (
	"bytes"
	"encoding/json"
	"letshare-server/internal/model"
	"letshare-server/internal/service"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

// wsWriteBufferPool 共享写缓冲池，50个连接共用而非各自占 64KB
var wsWriteBufferPool = &sync.Pool{
	New: func() interface{} {
		return make([]byte, 65536)
	},
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		// CORS检查在中间件中处理，这里允许所有来源
		return true
	},
	// 64KB 读缓冲 — 匹配文件传输分块大小, 减少 syscall
	ReadBufferSize: 65536,
	// 写缓冲使用共享池，避免每连接固定分配 64KB
	WriteBufferPool: wsWriteBufferPool,
}

type WebSocketHandler struct {
	wsService           *service.WebSocketService
	authService         *service.AuthService
	fileTransferService *service.FileTransferService
}

func NewWebSocketHandler(wsService *service.WebSocketService, authService *service.AuthService, fileTransferService *service.FileTransferService) *WebSocketHandler {
	handler := &WebSocketHandler{
		wsService:           wsService,
		authService:         authService,
		fileTransferService: fileTransferService,
	}

	// 注册客户端断开回调：当 WebSocket 客户端断开时，清理其作为发送方的文件传输会话
	wsService.SetOnClientDisconnect(func(clientID string) {
		fileTransferService.HandleClientDisconnect(clientID)
	})

	return handler
}

// HandleWebSocket 处理WebSocket连接
func (h *WebSocketHandler) HandleWebSocket(c *gin.Context) {
	// 从查询参数获取token和用户ID
	token := c.Query("token")
	userIdParam := c.Query("userId") // 新增：从查询参数获取用户ID

	if token == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "缺少认证token"})
		return
	}

	// 验证AuthToken
	if err := h.authService.ValidateAuthToken(token); err != nil {
		logrus.WithError(err).Error("AuthToken验证失败")
		c.JSON(http.StatusUnauthorized, gin.H{"error": "token验证失败: " + err.Error()})
		return
	}

	// 升级为WebSocket连接
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		logrus.WithError(err).Error("WebSocket升级失败")
		return
	}

	// 创建客户端
	clientID := uuid.New().String()
	// 使用传递的用户ID，如果没有则使用clientID
	userID := userIdParam
	if userID == "" {
		userID = clientID // 回退到clientID
	}

	client := model.NewClient(clientID, userID, conn)
	client.Metadata["authenticated"] = true

	// 添加到服务
	h.wsService.AddClient(client)

	logrus.WithFields(logrus.Fields{
		"client_id": clientID,
		"user_id":   userID,
	}).Info("WebSocket客户端已连接")

	// 使用defer确保资源清理，即使发生panic也能执行
	defer func() {
		// 恢复panic，防止整个服务崩溃
		if r := recover(); r != nil {
			logrus.WithFields(logrus.Fields{
				"client_id": clientID,
				"panic":     r,
			}).Error("WebSocket处理发生panic")
		}

		// 确保连接被关闭
		if conn != nil {
			conn.Close()
		}

		// 从服务中移除客户端（这会清理所有相关资源）
		h.wsService.RemoveClient(clientID)

		logrus.WithField("client_id", clientID).Info("WebSocket连接已清理")
	}()

	// 设置连接参数
	conn.SetReadLimit(512 * 1024)                          // 512KB
	conn.SetReadDeadline(time.Now().Add(60 * time.Second)) // 60s 移动网络容忍度
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		client.LastPing = time.Now()
		return nil
	})

	// 启动ping定时器
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// 启动消息处理goroutine
	done := make(chan struct{}, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logrus.WithFields(logrus.Fields{
					"client_id": clientID,
					"panic":     r,
				}).Error("消息处理goroutine发生panic")
			}
			close(done)
		}()
		h.handleMessages(client, conn)
	}()

	// 保持连接和定期ping
	for {
		select {
		case <-done:
			// 消息处理goroutine结束，退出主循环
			return
		case <-ticker.C:
			// 检查连接是否仍然有效
			if conn == nil {
				return
			}
			// 使用defer recover防止panic
			func() {
				defer func() {
					if r := recover(); r != nil {
						logrus.WithFields(logrus.Fields{
							"client_id": clientID,
							"panic":     r,
						}).Warn("发送ping时发生panic")
					}
				}()

				client.ConnMutex.Lock()
				defer client.ConnMutex.Unlock()

				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					// 只在非预期错误时记录（忽略正常关闭）
					if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
						logrus.WithField("client_id", clientID).Debug("连接已关闭，停止发送ping")
					}
					// 触发清理，不记录错误
					done <- struct{}{}
				}
			}()
		}
	}
}

// handleMessages 处理客户端消息(同时支持JSON和二进制消息)
func (h *WebSocketHandler) handleMessages(client *model.Client, conn *websocket.Conn) {
	for {
		messageType, messageData, err := conn.ReadMessage()
		if err != nil {
			// 区分正常关闭和异常关闭
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseNormalClosure,
				websocket.CloseGoingAway,
				websocket.CloseAbnormalClosure,
				websocket.CloseNoStatusReceived) {
				logrus.WithField("client_id", client.ID).WithError(err).Warn("WebSocket异常断开")
			} else {
				logrus.WithField("client_id", client.ID).Debug("WebSocket正常关闭")
			}
			break
		}

		// 更新最后活跃时间
		client.LastPing = time.Now()
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		// 根据消息类型处理
		switch messageType {
		case websocket.TextMessage:
			// JSON消息
			var message model.WebSocketMessage
			if err := json.Unmarshal(messageData, &message); err != nil {
				logrus.WithField("client_id", client.ID).WithError(err).Error("JSON解析失败")
				continue
			}
			h.processMessage(client, &message)

		case websocket.BinaryMessage:
			// 二进制消息(文件传输)
			h.processBinaryMessage(client, messageData)

		default:
			logrus.WithFields(logrus.Fields{
				"client_id":    client.ID,
				"message_type": messageType,
			}).Warn("不支持的消息类型")
		}
	}
}

// processMessage 处理具体消息
func (h *WebSocketHandler) processMessage(client *model.Client, message *model.WebSocketMessage) {
	logrus.WithFields(logrus.Fields{
		"client_id": client.ID,
		"type":      message.Type,
		"channel":   message.Channel,
		"event":     message.Event,
	}).Debug("收到客户端消息")

	switch message.Type {
	case model.MessageTypeSubscribe:
		h.handleSubscribe(client, message)
	case model.MessageTypeUnsubscribe:
		h.handleUnsubscribe(client, message)
	case model.MessageTypePublish:
		h.handlePublish(client, message)
	// 文件传输相关消息
	case model.MessageTypeFileTransferRequest:
		h.handleFileTransferRequest(client, message)
	case model.MessageTypeFileTransferAccept:
		h.handleFileTransferAccept(client, message)
	case model.MessageTypeFileTransferReject:
		h.handleFileTransferReject(client, message)
	case model.MessageTypeFileTransferStart:
		h.handleFileTransferStart(client, message)
	case model.MessageTypeFileTransferEnd:
		h.handleFileTransferEnd(client, message)
	case model.MessageTypeFileTransferComplete:
		h.handleFileTransferComplete(client, message)
	case model.MessageTypeFileTransferResend:
		h.handleFileTransferResend(client, message)
	case model.MessageTypeFileTransferCancel:
		h.handleFileTransferCancel(client, message)
	default:
		h.sendError(client, 400, "不支持的消息类型: "+message.Type)
	}
}

// handleSubscribe 处理订阅消息
func (h *WebSocketHandler) handleSubscribe(client *model.Client, message *model.WebSocketMessage) {
	if message.Channel == "" {
		h.sendError(client, 400, "缺少频道名称")
		return
	}

	// 如果没有指定事件，则只订阅房间
	event := message.Event

	if err := h.wsService.SubscribeToRoom(client.ID, message.Channel, event); err != nil {
		h.sendError(client, 400, err.Error())
		return
	}

	// 发送订阅确认
	h.sendMessage(client, model.NewWebSocketMessage(
		"subscribed",
		message.Channel,
		event,
		map[string]interface{}{
			"status": "subscribed",
			"room":   message.Channel,
			"event":  event,
		},
	))
}

// handleUnsubscribe 处理取消订阅消息
func (h *WebSocketHandler) handleUnsubscribe(client *model.Client, message *model.WebSocketMessage) {
	if message.Channel == "" {
		h.sendError(client, 400, "缺少频道名称")
		return
	}

	event := message.Event
	if event == "" {
		event = "signal:all"
	}

	if err := h.wsService.UnsubscribeFromRoom(client.ID, message.Channel, event); err != nil {
		h.sendError(client, 400, err.Error())
		return
	}

	// 发送取消订阅确认
	h.sendMessage(client, model.NewWebSocketMessage(
		"unsubscribed",
		message.Channel,
		event,
		map[string]interface{}{
			"status": "unsubscribed",
			"room":   message.Channel,
		},
	))
}

// handlePublish 处理发布消息
func (h *WebSocketHandler) handlePublish(client *model.Client, message *model.WebSocketMessage) {
	if message.Channel == "" {
		h.sendError(client, 400, "缺少频道名称")
		return
	}

	event := message.Event
	if event == "" {
		event = "signal:all"
	}

	// 验证消息数据
	if message.Data == nil {
		h.sendError(client, 400, "缺少消息数据")
		return
	}

	// 验证数据格式
	var data map[string]interface{}
	if err := json.Unmarshal(message.Data, &data); err != nil {
		h.sendError(client, 400, "消息数据格式错误")
		return
	}

	// 确保包含必要的字段（from字段）
	if _, exists := data["from"]; !exists {
		data["from"] = client.UserID
		if newData, err := json.Marshal(data); err == nil {
			message.Data = newData
		}
	}

	if err := h.wsService.PublishToRoom(client.ID, message.Channel, event, message.Data); err != nil {
		h.sendError(client, 400, err.Error())
		return
	}
}

// sendMessage 发送消息给客户端
func (h *WebSocketHandler) sendMessage(client *model.Client, message *model.WebSocketMessage) {
	conn, ok := client.Connection.(*websocket.Conn)
	if !ok {
		return
	}

	client.ConnMutex.Lock()
	defer client.ConnMutex.Unlock()

	if err := conn.WriteJSON(message); err != nil {
		logrus.WithFields(logrus.Fields{
			"client_id": client.ID,
			"error":     err.Error(),
		}).Error("发送消息失败")
	}
}

// sendError 发送错误消息（通用类型，用于订阅/发布等非文件传输场景）
func (h *WebSocketHandler) sendError(client *model.Client, code int, message string) {
	logrus.WithFields(logrus.Fields{
		"client_id": client.ID,
		"code":      code,
		"message":   message,
	}).Warn("发送错误消息")

	errorMsg := model.NewErrorMessage(code, message)
	h.sendMessage(client, errorMsg)
}

// sendFileTransferError 发送文件传输错误消息（使用"file:transfer:error"类型，客户端可识别）
// transferID 可选：如果已知 session 的 transfer_id 则填入，否则传空字符串
func (h *WebSocketHandler) sendFileTransferError(client *model.Client, code int, message string, transferID string) {
	logrus.WithFields(logrus.Fields{
		"client_id": client.ID,
		"code":      code,
		"message":   message,
	}).Warn("发送文件传输错误消息")

	payload := map[string]interface{}{
		"code":    code,
		"message": message,
	}
	if transferID != "" {
		payload["transfer_id"] = transferID
		payload["error"] = message
	}
	data, _ := json.Marshal(payload)

	errorMsg := &model.WebSocketMessage{
		Type:      model.MessageTypeFileTransferError,
		Channel:   "",
		Event:     "",
		Data:      data,
		Timestamp: time.Now().UnixMilli(),
	}
	h.sendMessage(client, errorMsg)
}

// processBinaryMessage 处理二进制消息(文件数据块)
func (h *WebSocketHandler) processBinaryMessage(client *model.Client, data []byte) {
	// 二进制消息格式: 前面是JSON元数据(固定长度或分隔符),后面是实际数据
	// 这里我们假设客户端先发送JSON元数据,再发送二进制数据
	// 因此在收到二进制消息时,我们需要先解析元数据

	// 简化处理:假设前256字节是JSON元数据头,剩余是数据
	if len(data) < 256 {
		h.sendFileTransferError(client, 400, "二进制消息格式错误", "")
		return
	}

	// 解析元数据（前256字节，去除padding的0）
	var chunkMeta model.FileTransferChunk
	metaBytes := bytes.TrimRight(data[:256], "\x00")
	if err := json.Unmarshal(metaBytes, &chunkMeta); err != nil {
		logrus.WithField("client_id", client.ID).WithError(err).Error("解析文件块元数据失败")
		h.sendFileTransferError(client, 400, "文件块元数据格式错误", "")
		return
	}

	// 提取实际数据(跳过元数据部分)，但转发时保留完整帧，接收端可直接按 transfer_id 定位会话
	chunkData := data[256:]
	h.handleFileChunk(client, &chunkMeta, chunkData, data)
}

// handleFileChunk 处理文件数据块
func (h *WebSocketHandler) handleFileChunk(client *model.Client, chunkMeta *model.FileTransferChunk, chunkData []byte, framedData []byte) {
	logrus.WithFields(logrus.Fields{
		"client_id":   client.ID,
		"transfer_id": chunkMeta.TransferID,
		"chunk_index": chunkMeta.ChunkIndex,
		"chunk_size":  len(chunkData),
	}).Debug("收到文件数据块")

	// 服务器中继转发逻辑
	// 验证会话
	session, err := h.fileTransferService.GetSession(chunkMeta.TransferID)
	if err != nil {
		h.sendFileTransferError(client, 404, "传输会话不存在", "")
		return
	}

	// 验证发送者
	if session.FromUserID != client.UserID {
		h.sendFileTransferError(client, 403, "无权发送此传输的数据", "")
		return
	}

	// 验证会话状态 — 允许 transferring 和 resending（重传）
	if session.Status == "completed" {
		logrus.WithFields(logrus.Fields{
			"transfer_id": chunkMeta.TransferID,
			"chunk_index": chunkMeta.ChunkIndex,
		}).Debug("ignore late file chunk for completed transfer")
		return
	}
	if session.Status != "completed" && session.Status != "transferring" && session.Status != "resending" {
		h.sendFileTransferError(client, 400, "传输会话状态错误: "+session.Status, "")
		return
	}

	// 转发数据块给接收者(零拷贝)
	if err := h.fileTransferService.ForwardChunkToReceiver(chunkMeta.TransferID, framedData, len(chunkData), chunkMeta); err != nil {
		logrus.WithError(err).Error("转发文件数据块失败")
		h.sendFileTransferError(client, 500, "转发失败: "+err.Error(), "")

		// 通知双方传输错误
		h.notifyTransferError(session, "数据转发失败")
		return
	}
}

// handleFileTransferRequest 处理文件传输请求
func (h *WebSocketHandler) handleFileTransferRequest(client *model.Client, message *model.WebSocketMessage) {
	var request model.FileTransferRequest
	if err := json.Unmarshal(message.Data, &request); err != nil {
		h.sendFileTransferError(client, 400, "请求数据格式错误", "")
		return
	}

	// 设置发送者信息
	request.FromUserID = client.UserID

	// 创建传输会话
	session, err := h.fileTransferService.CreateTransferSession(&request)
	if err != nil {
		h.sendFileTransferError(client, 400, err.Error(), "")
		return
	}

	// 更新客户端ID
	h.fileTransferService.UpdateSessionClients(session.TransferID, client.ID, "")

	// 清除密码，不转发给接收端
	request.AdminPass = ""

	// 转发请求给接收者
	requestMsg := model.NewWebSocketMessage(
		model.MessageTypeFileTransferRequest,
		request.RoomName,
		"",
		request,
	)

	if err := h.fileTransferService.SendMessageToUser(request.ToUserID, request.RoomName, requestMsg); err != nil {
		h.sendFileTransferError(client, 404, "找不到接收者", "")
		h.fileTransferService.RemoveSession(session.TransferID)
		return
	}

	logrus.WithFields(logrus.Fields{
		"transfer_id": session.TransferID,
		"from":        request.FromUserID,
		"to":          request.ToUserID,
		"file":        request.FileName,
	}).Info("文件传输请求已发送")
}

// handleFileTransferAccept 处理接受文件传输
func (h *WebSocketHandler) handleFileTransferAccept(client *model.Client, message *model.WebSocketMessage) {
	var data map[string]interface{}
	if err := json.Unmarshal(message.Data, &data); err != nil {
		h.sendFileTransferError(client, 400, "数据格式错误", "")
		return
	}

	transferID, ok := data["transfer_id"].(string)
	if !ok {
		h.sendFileTransferError(client, 400, "缺少transfer_id", "")
		return
	}

	session, err := h.fileTransferService.GetSession(transferID)
	if err != nil {
		h.sendFileTransferError(client, 404, "传输会话不存在", "")
		return
	}

	// 验证接收者
	if session.ToUserID != client.UserID {
		h.sendFileTransferError(client, 403, "无权接受此传输", "")
		return
	}

	// 验证状态转换合法性
	if session.Status != "pending" {
		h.sendFileTransferError(client, 400, "传输会话状态错误，无法接受: "+session.Status, "")
		return
	}

	// 更新会话状态
	h.fileTransferService.UpdateSessionStatus(transferID, "pending", "accepted")
	h.fileTransferService.UpdateSessionClients(transferID, session.FromClientID, client.ID)

	// 通知发送者
	acceptMsg := model.NewWebSocketMessage(
		model.MessageTypeFileTransferAccept,
		session.RoomName,
		"",
		map[string]interface{}{
			"transfer_id": transferID,
		},
	)

	h.fileTransferService.SendMessageToUser(session.FromUserID, session.RoomName, acceptMsg)

	logrus.WithField("transfer_id", transferID).Info("文件传输已接受")
}

// handleFileTransferReject 处理拒绝文件传输
func (h *WebSocketHandler) handleFileTransferReject(client *model.Client, message *model.WebSocketMessage) {
	var data map[string]interface{}
	if err := json.Unmarshal(message.Data, &data); err != nil {
		h.sendFileTransferError(client, 400, "数据格式错误", "")
		return
	}

	transferID, ok := data["transfer_id"].(string)
	if !ok {
		h.sendFileTransferError(client, 400, "缺少transfer_id", "")
		return
	}

	session, err := h.fileTransferService.GetSession(transferID)
	if err != nil {
		h.sendFileTransferError(client, 404, "传输会话不存在", "")
		return
	}

	// 验证状态转换合法性
	if session.Status != "pending" {
		h.sendFileTransferError(client, 400, "传输会话状态错误，无法拒绝: "+session.Status, "")
		return
	}

	// 更新会话状态
	h.fileTransferService.UpdateSessionStatus(transferID, "pending", "rejected")

	// 通知发送者
	rejectMsg := model.NewWebSocketMessage(
		model.MessageTypeFileTransferReject,
		session.RoomName,
		"",
		map[string]interface{}{
			"transfer_id": transferID,
			"reason":      data["reason"],
		},
	)

	h.fileTransferService.SendMessageToUser(session.FromUserID, session.RoomName, rejectMsg)

	// 清理会话
	h.fileTransferService.RemoveSession(transferID)

	logrus.WithField("transfer_id", transferID).Info("文件传输已拒绝")
}

// handleFileTransferStart 处理开始文件传输
func (h *WebSocketHandler) handleFileTransferStart(client *model.Client, message *model.WebSocketMessage) {
	var data map[string]interface{}
	if err := json.Unmarshal(message.Data, &data); err != nil {
		h.sendFileTransferError(client, 400, "数据格式错误", "")
		return
	}

	transferID, ok := data["transfer_id"].(string)
	if !ok {
		h.sendFileTransferError(client, 400, "缺少transfer_id", "")
		return
	}

	session, err := h.fileTransferService.GetSession(transferID)
	if err != nil {
		h.sendFileTransferError(client, 404, "传输会话不存在", "")
		return
	}

	// 验证发送者
	if session.FromUserID != client.UserID {
		h.sendFileTransferError(client, 403, "无权开始此传输", "")
		return
	}

	// 验证状态转换合法性
	if session.Status != "accepted" && session.Status != "resending" {
		h.sendFileTransferError(client, 400, "传输会话状态错误，无法开始传输: "+session.Status, "")
		return
	}

	// 更新会话状态
	h.fileTransferService.UpdateSessionStatus(transferID, session.Status, "transferring")

	// 通知接收者
	startMsg := model.NewWebSocketMessage(
		model.MessageTypeFileTransferStart,
		session.RoomName,
		"",
		map[string]interface{}{
			"transfer_id":  transferID,
			"file_name":    session.FileName,
			"file_size":    session.FileSize,
			"total_chunks": session.TotalChunks,
		},
	)

	h.fileTransferService.SendMessageToUser(session.ToUserID, session.RoomName, startMsg)

	logrus.WithField("transfer_id", transferID).Info("文件传输已开始")
}

// handleFileTransferEnd 处理文件传输完成
func (h *WebSocketHandler) handleFileTransferEnd(client *model.Client, message *model.WebSocketMessage) {
	var data map[string]interface{}
	if err := json.Unmarshal(message.Data, &data); err != nil {
		h.sendFileTransferError(client, 400, "数据格式错误", "")
		return
	}

	transferID, ok := data["transfer_id"].(string)
	if !ok {
		h.sendFileTransferError(client, 400, "缺少transfer_id", "")
		return
	}

	session, err := h.fileTransferService.GetSession(transferID)
	if err != nil {
		h.sendFileTransferError(client, 404, "传输会话不存在", "")
		return
	}

	// 验证发送者。END 只表示发送端已经发完字节，不能代表接收端已经组装成功。
	if session.FromUserID != client.UserID {
		h.sendFileTransferError(client, 403, "无权结束此传输", "")
		return
	}

	// 验证状态转换合法性
	if session.Status != "completed" && session.Status != "transferring" && session.Status != "resending" {
		h.sendFileTransferError(client, 400, "传输会话状态错误，无法结束传输: "+session.Status, "")
		return
	}

	if session.Status == "completed" {
		logrus.WithField("transfer_id", transferID).Debug("ignore late transfer end for completed transfer")
		return
	}

	h.fileTransferService.UpdateSessionStatus(transferID, session.Status, "ending")

	// 只通知接收者进入收尾校验；发送者必须等待接收方 COMPLETE。
	endMsg := model.NewWebSocketMessage(
		model.MessageTypeFileTransferEnd,
		session.RoomName,
		"",
		map[string]interface{}{
			"transfer_id": transferID,
			"file_name":   session.FileName,
			"file_size":   session.FileSize,
		},
	)

	h.fileTransferService.SendMessageToUser(session.ToUserID, session.RoomName, endMsg)

	logrus.WithFields(logrus.Fields{
		"transfer_id": transferID,
		"file_name":   session.FileName,
		"file_size":   session.FileSize,
		"duration":    time.Since(session.StartTime).String(),
	}).Info("文件传输发送端已结束，等待接收端确认")
}

// handleFileTransferComplete 处理接收方完成确认
func (h *WebSocketHandler) handleFileTransferComplete(client *model.Client, message *model.WebSocketMessage) {
	var data map[string]interface{}
	if err := json.Unmarshal(message.Data, &data); err != nil {
		h.sendFileTransferError(client, 400, "数据格式错误", "")
		return
	}

	transferID, ok := data["transfer_id"].(string)
	if !ok {
		h.sendFileTransferError(client, 400, "缺少transfer_id", "")
		return
	}

	session, err := h.fileTransferService.GetSession(transferID)
	if err != nil {
		h.sendFileTransferError(client, 404, "传输会话不存在", "")
		return
	}

	if session.ToUserID != client.UserID {
		h.sendFileTransferError(client, 403, "无权确认此传输", "")
		return
	}

	// 验证状态转换合法性 — 接收方可能在重传完成、发送端 END 还没到达时就组装完毕
	if session.Status == "completed" {
		logrus.WithField("transfer_id", transferID).Debug("ignore duplicate transfer complete")
		return
	}
	if session.Status != "ending" && session.Status != "resending" {
		h.sendFileTransferError(client, 400, "传输会话状态错误，无法确认完成: "+session.Status, "")
		return
	}

	if err := h.fileTransferService.UpdateSessionStatus(transferID, session.Status, "completed"); err != nil {
		h.sendFileTransferError(client, 409, "transfer session status changed: "+err.Error(), "")
		return
	}

	completeMsg := model.NewWebSocketMessage(
		model.MessageTypeFileTransferComplete,
		session.RoomName,
		"",
		map[string]interface{}{
			"transfer_id": transferID,
			"file_name":   session.FileName,
			"file_size":   session.FileSize,
		},
	)

	h.fileTransferService.SendMessageToUser(session.FromUserID, session.RoomName, completeMsg)

	// 延迟清理会话
	go func() {
		time.Sleep(30 * time.Second)
		h.fileTransferService.RemoveSession(transferID)
	}()

	logrus.WithFields(logrus.Fields{
		"transfer_id": transferID,
		"file_name":   session.FileName,
		"file_size":   session.FileSize,
		"duration":    time.Since(session.StartTime).String(),
	}).Info("文件传输已由接收端确认完成")
}

// handleFileTransferResend 处理接收方请求重传缺失分片
func (h *WebSocketHandler) handleFileTransferResend(client *model.Client, message *model.WebSocketMessage) {
	var data map[string]interface{}
	if err := json.Unmarshal(message.Data, &data); err != nil {
		h.sendFileTransferError(client, 400, "数据格式错误", "")
		return
	}

	transferID, ok := data["transfer_id"].(string)
	if !ok || transferID == "" {
		h.sendFileTransferError(client, 400, "缺少transfer_id", "")
		return
	}

	session, err := h.fileTransferService.GetSession(transferID)
	if err != nil {
		h.sendFileTransferError(client, 404, "传输会话不存在", "")
		return
	}

	if session.ToUserID != client.UserID {
		h.sendFileTransferError(client, 403, "无权请求此传输重传", "")
		return
	}

	// 验证状态转换合法性 — 允许从 transferring 或 ending 进入 resending 恢复模式
	if session.Status == "completed" {
		logrus.WithField("transfer_id", transferID).Debug("ignore late resend request for completed transfer")
		return
	}
	if session.Status != "transferring" && session.Status != "ending" {
		h.sendFileTransferError(client, 400, "传输会话状态错误，无法请求重传: "+session.Status, "")
		return
	}

	// 重传请求到达后进入 resending 恢复模式，等待发送端确认后回到 transferring。
	// 使用 session.Status 作为 expectedStatus，兼容从 transferring 或 ending 状态进入
	h.fileTransferService.UpdateSessionStatus(transferID, session.Status, "resending")

	resendMsg := model.NewWebSocketMessage(
		model.MessageTypeFileTransferResend,
		session.RoomName,
		"",
		data,
	)

	if err := h.fileTransferService.SendMessageToUser(session.FromUserID, session.RoomName, resendMsg); err != nil {
		h.fileTransferService.UpdateSessionStatus(transferID, "", "error")
		go func() {
			time.Sleep(5 * time.Second)
			h.fileTransferService.RemoveSession(transferID)
		}()
		h.sendFileTransferError(client, 404, "找不到发送者", "")
		return
	}

	logrus.WithFields(logrus.Fields{
		"transfer_id": transferID,
		"from":        session.ToUserID,
		"to":          session.FromUserID,
	}).Info("已转发文件分片重传请求")
}

// handleFileTransferCancel 处理取消文件传输
func (h *WebSocketHandler) handleFileTransferCancel(client *model.Client, message *model.WebSocketMessage) {
	var data map[string]interface{}
	if err := json.Unmarshal(message.Data, &data); err != nil {
		h.sendFileTransferError(client, 400, "数据格式错误", "")
		return
	}

	transferID, ok := data["transfer_id"].(string)
	if !ok {
		h.sendFileTransferError(client, 400, "缺少transfer_id", "")
		return
	}

	session, err := h.fileTransferService.GetSession(transferID)
	if err != nil {
		h.sendFileTransferError(client, 404, "传输会话不存在", "")
		return
	}

	// 验证状态转换合法性 — 终端状态不可取消
	if session.Status == "completed" || session.Status == "cancelled" || session.Status == "error" || session.Status == "rejected" {
		h.sendFileTransferError(client, 400, "传输会话已结束，无法取消: "+session.Status, "")
		return
	}

	// 更新会话状态
	h.fileTransferService.UpdateSessionStatus(transferID, session.Status, "cancelled")

	// 通知对方
	cancelMsg := model.NewWebSocketMessage(
		model.MessageTypeFileTransferCancel,
		session.RoomName,
		"",
		map[string]interface{}{
			"transfer_id": transferID,
			"reason":      data["reason"],
		},
	)

	// 通知发送者和接收者
	if client.UserID == session.FromUserID {
		h.fileTransferService.SendMessageToUser(session.ToUserID, session.RoomName, cancelMsg)
	} else {
		h.fileTransferService.SendMessageToUser(session.FromUserID, session.RoomName, cancelMsg)
	}

	// 清理会话
	h.fileTransferService.RemoveSession(transferID)

	logrus.WithField("transfer_id", transferID).Info("文件传输已取消")
}

// notifyTransferError 通知传输错误
func (h *WebSocketHandler) notifyTransferError(session *model.FileTransferSession, errMsg string) {
	h.fileTransferService.UpdateSessionStatus(session.TransferID, "", "error")

	errorMsg := model.NewWebSocketMessage(
		model.MessageTypeFileTransferError,
		session.RoomName,
		"",
		map[string]interface{}{
			"transfer_id": session.TransferID,
			"error":       errMsg,
		},
	)

	h.fileTransferService.SendMessageToUser(session.FromUserID, session.RoomName, errorMsg)
	h.fileTransferService.SendMessageToUser(session.ToUserID, session.RoomName, errorMsg)

	// 延迟清理会话，给客户端时间处理错误消息
	go func() {
		time.Sleep(5 * time.Second)
		h.fileTransferService.RemoveSession(session.TransferID)
	}()
}
