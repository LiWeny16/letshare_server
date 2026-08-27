package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"letshare-server/internal/model"
	"letshare-server/internal/service"
	"math"
	"net"
	"net/http"
	"strings"
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

const websocketReadTimeout = 120 * time.Second

const maxRelayResendChunkIndexes = 256

// ErrorRateLimiter 限制向客户端发送错误消息的频率
// 防止移动网络重连风暴时大量错误消息淹没客户端
type ErrorRateLimiter struct {
	mu      sync.Mutex
	clients map[string]*clientErrorRecord
}

type clientErrorRecord struct {
	timestamps   []time.Time
	blockedUntil time.Time
}

// NewErrorRateLimiter 创建错误频率限制器
func NewErrorRateLimiter() *ErrorRateLimiter {
	return &ErrorRateLimiter{
		clients: make(map[string]*clientErrorRecord),
	}
}

// Allow 检查是否允许向指定客户端发送错误消息
// 5秒内超过3条错误则屏蔽10秒，避免重连风暴
func (r *ErrorRateLimiter) Allow(clientID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	record, exists := r.clients[clientID]
	if !exists {
		r.clients[clientID] = &clientErrorRecord{
			timestamps: []time.Time{now},
		}
		return true
	}

	// 检查是否处于屏蔽期
	if !record.blockedUntil.IsZero() && now.Before(record.blockedUntil) {
		return false
	}

	// 清理5秒前的过期时间戳
	cutoff := now.Add(-5 * time.Second)
	valid := record.timestamps[:0]
	for _, t := range record.timestamps {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	record.timestamps = append(valid, now)

	// 5秒内超过3条错误，屏蔽10秒
	if len(record.timestamps) > 3 {
		record.blockedUntil = now.Add(10 * time.Second)
		record.timestamps = nil // 重置计数
		logrus.WithField("client_id", clientID).Warn("错误消息频率过高，已屏蔽10秒")
		return false
	}

	return true
}

type WebSocketHandler struct {
	wsService           *service.WebSocketService
	authService         *service.AuthService
	fileTransferService *service.FileTransferService
	jwtService          *service.JWTService
	errorRateLimiter    *ErrorRateLimiter
}

func NewWebSocketHandler(wsService *service.WebSocketService, authService *service.AuthService, fileTransferService *service.FileTransferService, jwtService *service.JWTService) *WebSocketHandler {
	handler := &WebSocketHandler{
		wsService:           wsService,
		authService:         authService,
		fileTransferService: fileTransferService,
		jwtService:          jwtService,
		errorRateLimiter:    NewErrorRateLimiter(),
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

	// 验证可选 PRO token（JWT），设置 isPro 元数据
	if proToken := c.Query("pro_token"); proToken != "" {
		if claims, err := h.jwtService.ValidateToken(proToken); err == nil && claims.IsPro {
			// 验证 subject 绑定：JWT 的 sub 必须匹配当前 userID
			if claims.Subject == "" || claims.Subject != userID {
				logrus.WithFields(logrus.Fields{
					"client_id":     clientID,
					"user_id":       userID,
					"token_subject": claims.Subject,
				}).Warn("PRO token subject 不匹配，拒绝授予 PRO 状态")
			} else {
				client.Metadata["isPro"] = true
				logrus.WithFields(logrus.Fields{
					"client_id": clientID,
					"user_id":   userID,
				}).Debug("PRO token 验证成功")
			}
		} else if err != nil {
			logrus.WithFields(logrus.Fields{
				"client_id": clientID,
				"error":     err.Error(),
			}).Debug("PRO token 验证失败")
		}
	}

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
	conn.SetReadLimit(512 * 1024)                              // 512KB
	conn.SetReadDeadline(time.Now().Add(websocketReadTimeout)) // Chrome 后台节流需宽容
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(websocketReadTimeout))
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
			// Signal the owner loop without risking a second close/send.
			select {
			case done <- struct{}{}:
			default:
			}
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
					if !isConnClosedError(err) {
						logrus.WithField("client_id", clientID).WithError(err).Warn("发送ping失败")
					}
					select {
					case done <- struct{}{}:
					default:
					}
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
		conn.SetReadDeadline(time.Now().Add(websocketReadTimeout))

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
	case model.MessageTypeFileTransferAck:
		h.handleFileTransferAck(client, message)
	case model.MessageTypeFileTransferResend:
		h.handleFileTransferResend(client, message)
	case model.MessageTypeFileTransferResumeQuery:
		h.handleFileTransferResumeQuery(client, message)
	case model.MessageTypeFileTransferCancel:
		h.handleFileTransferCancel(client, message)
	case model.MessageTypeFileTransferProgress:
		// 进度消息仅由服务端向客户端推送，客户端不应主动发送
		// 移动网络不稳定时客户端可能回显进度消息，静默跳过避免产生错误
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
		if isConnClosedError(err) {
			logrus.WithField("client_id", client.ID).Debug("连接已关闭，跳过发送消息")
		} else {
			logrus.WithFields(logrus.Fields{
				"client_id": client.ID,
				"error":     err.Error(),
			}).Error("发送消息失败")
		}
	}
}

// sendError 发送错误消息（通用类型，用于订阅/发布等非文件传输场景）
func (h *WebSocketHandler) sendError(client *model.Client, code int, message string) {
	// 频率限制：防止重连风暴时大量错误淹没客户端
	if h.errorRateLimiter != nil && !h.errorRateLimiter.Allow(client.ID) {
		return
	}

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
	// 频率限制：防止重连风暴时大量错误淹没客户端
	if h.errorRateLimiter != nil && !h.errorRateLimiter.Allow(client.ID) {
		return
	}

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

// mediaFrameHeaderSize 媒体帧固定头大小（与客户端 callSignaling.ts 的 MEDIA_FRAME_HEADER_SIZE 一致）
const mediaFrameHeaderSize = 24

// mediaFrameCallIDBytes 媒体帧头中 callId 占用字节数
const mediaFrameCallIDBytes = 16

// mediaFrameMaxPayload 单帧 payload 上限（64KB，防止异常大包占用内存/带宽）
const mediaFrameMaxPayload = 64 * 1024

// isMediaFrame 判断二进制帧是否为通话媒体帧（"medi" 魔数开头）
func isMediaFrame(data []byte) bool {
	return len(data) >= mediaFrameHeaderSize+1 && data[0] == 'm' && data[1] == 'e' && data[2] == 'd' && data[3] == 'i'
}

// processBinaryMessage 处理二进制消息(文件数据块 / 通话媒体帧)
func (h *WebSocketHandler) processBinaryMessage(client *model.Client, data []byte) {
	// 媒体帧（通话公网兜底轨道）：24字节固定头 [callId 16B | seq 2B | track 1B | padding 5B] + 裸 payload。
	// 盲转发：不解析 payload，按头内 callId 定向转发。
	if isMediaFrame(data) {
		h.handleMediaFrame(client, data)
		return
	}

	// 文件传输二进制：前256字节是JSON元数据头,剩余是数据
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

// handleMediaFrame 处理通话媒体帧（公网兜底轨道）。
// 帧头 layout（24B，与客户端 encodeMediaFrame 一致）：
//
//	[0..4)   "medi" 魔数
//	[4..20)  callId (ascii, \0 填充)
//	[20..22) seq (uint16 BE)
//	[22)     track: 0=audio 1=video 2=data
//	[23)     padding
//
// 转发语义：盲转发。服务器不解析 payload，不做会话管理、不做重传。
// 目标用户定位：媒体帧是 1对1 通话，发送方所在房间 + 头内 callId 关联的对端。
// 由于服务器不维护通话会话表，这里采用"房间内除发送方外的全部用户"广播，
// 由客户端按 callId 过滤丢弃非本通话帧（与信令 publish 广播同语义，零会话状态）。
func (h *WebSocketHandler) handleMediaFrame(client *model.Client, frame []byte) {
	if len(frame) < mediaFrameHeaderSize {
		return
	}
	payloadLen := len(frame) - mediaFrameHeaderSize
	if payloadLen > mediaFrameMaxPayload {
		logrus.WithFields(logrus.Fields{
			"client_id": client.ID,
			"payload":   payloadLen,
		}).Warn("媒体帧 payload 超过上限，丢弃")
		return
	}

	// 发送方必须已加入某个房间才能定位对端
	var roomName string
	for r := range client.Rooms {
		roomName = r
		break
	}
	if roomName == "" {
		return
	}

	// callId 仅用于日志，不解析 payload
	callID := mediaFrameCallID(frame)
	seq := mediaFrameSeq(frame)
	track := frame[18]

	// 转发给房间内除发送方外的全部用户（客户端按 callId 过滤）
	forwarded := 0
	for _, target := range h.wsService.GetRoomClients(roomName) {
		if target.ID == client.ID {
			continue
		}
		if err := h.wsService.WriteBinaryToClient(target, frame); err != nil {
			logrus.WithFields(logrus.Fields{
				"operation":    "media.forward",
				"call_id":      callID,
				"seq":          seq,
				"track":        track,
				"target":       target.UserID,
				"room":         roomName,
				"error":        err.Error(),
			}).Debug("媒体帧转发失败")
			continue
		}
		forwarded++
	}

	logrus.WithFields(logrus.Fields{
		"operation": "media.forward",
		"call_id":   callID,
		"seq":       seq,
		"track":     track,
		"payload":   payloadLen,
		"room":      roomName,
		"forwarded": forwarded,
	}).Debug("媒体帧已转发")
}

// mediaFrameCallID 从媒体帧头提取 callId（offset 4, ascii, 遇 \0 截断）
func mediaFrameCallID(frame []byte) string {
	if len(frame) < 4+mediaFrameCallIDBytes {
		return ""
	}
	for i := 0; i < mediaFrameCallIDBytes; i++ {
		if frame[4+i] == 0 {
			return string(frame[4 : 4+i])
		}
	}
	return string(frame[4 : 4+mediaFrameCallIDBytes])
}

// mediaFrameSeq 从媒体帧头提取 seq (uint16 BE, offset 20)
func mediaFrameSeq(frame []byte) uint16 {
	if len(frame) < 22 {
		return 0
	}
	return uint16(frame[20])<<8 | uint16(frame[21])
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
		// CRITICAL: 必须带上 transfer_id，否则客户端收到无 transfer_id 的错误消息
		// 会触发 "transfer id is required" 连锁错误，导致所有活跃传输被终止
		h.sendFileTransferError(client, 404, "传输会话不存在", chunkMeta.TransferID)
		return
	}

	// 验证发送者
	if session.FromUserID != client.UserID {
		h.sendFileTransferError(client, 403, "无权发送此传输的数据", chunkMeta.TransferID)
		return
	}

	// 验证会话状态 — 允许 transferring、resending 和 interrupted（等待接收端恢复）
	if session.Status == "completed" {
		logrus.WithFields(logrus.Fields{
			"transfer_id": chunkMeta.TransferID,
			"chunk_index": chunkMeta.ChunkIndex,
		}).Debug("ignore late file chunk for completed transfer")
		return
	}
	if session.Status != "completed" && session.Status != "transferring" && session.Status != "resending" && session.Status != "interrupted" {
		// CRITICAL: 大文件传输中如果接收方断开/会话超时，后续chunk会走到这里
		// 必须带上 transfer_id，否则触发客户端连锁错误
		h.sendFileTransferError(client, 400, "传输会话状态错误: "+session.Status, chunkMeta.TransferID)
		return
	}

	// 转发数据块给接收者(零拷贝)
	if err := h.fileTransferService.ForwardChunkToReceiver(chunkMeta.TransferID, framedData, len(chunkData), chunkMeta); err != nil {
		logrus.WithFields(logrus.Fields{
			"operation":   "relay.forward_chunk",
			"transfer_id": chunkMeta.TransferID,
			"chunk_index": chunkMeta.ChunkIndex,
			"chunk_size":  len(chunkData),
			"client_id":   client.ID,
			"user_id":     client.UserID,
			"room":        session.RoomName,
		}).WithError(err).Error("转发文件数据块失败")
		if service.IsRelayReceiverUnavailable(err) {
			if state, stateErr := h.fileTransferService.GetResumeState(chunkMeta.TransferID, client.UserID); stateErr == nil {
				h.sendMessage(client, model.NewWebSocketMessage(
					model.MessageTypeFileTransferResumeState,
					state.RoomName,
					"",
					state,
				))
			}
			return
		}

		h.sendFileTransferError(client, 500, "转发失败: "+err.Error(), chunkMeta.TransferID)

		// 通知双方传输错误
		h.notifyTransferError(session, "数据转发失败")
		return
	}
}

func (h *WebSocketHandler) handleFileTransferResumeQuery(client *model.Client, message *model.WebSocketMessage) {
	var query model.FileTransferResumeQuery
	if err := json.Unmarshal(message.Data, &query); err != nil {
		h.sendFileTransferError(client, 400, "resume query data format error", "")
		return
	}
	if query.TransferID == "" {
		h.sendFileTransferError(client, 400, "missing transfer_id", "")
		return
	}

	state, err := h.fileTransferService.GetResumeState(query.TransferID, client.UserID)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"operation":   "relay.resume_state.query",
			"transfer_id": query.TransferID,
			"client_id":   client.ID,
			"user_id":     client.UserID,
			"error":       err.Error(),
		}).Warn("relay resume state query failed")
		h.sendFileTransferError(client, 404, err.Error(), query.TransferID)
		return
	}

	h.sendMessage(client, model.NewWebSocketMessage(
		model.MessageTypeFileTransferResumeState,
		state.RoomName,
		"",
		state,
	))
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

	// 从客户端元数据读取 PRO 状态（WebSocket 握手时由 JWT 验证设置）
	isPro, _ := client.Metadata["isPro"].(bool)

	// 创建传输会话
	session, err := h.fileTransferService.CreateTransferSession(&request, isPro)
	if err != nil {
		h.sendFileTransferError(client, 400, err.Error(), request.TransferID)
		return
	}

	// 更新客户端ID
	h.fileTransferService.UpdateSessionClients(session.TransferID, client.ID, "")

	// IsPro 标记不需要转发给接收端，服务端校验后不再传递

	// 转发请求给接收者
	requestMsg := model.NewWebSocketMessage(
		model.MessageTypeFileTransferRequest,
		request.RoomName,
		"",
		request,
	)

	if err := h.fileTransferService.SendMessageToUser(request.ToUserID, request.RoomName, requestMsg); err != nil {
		h.sendFileTransferError(client, 404, "找不到接收者", session.TransferID)
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

	// 防御性检查：data为空对象时可能是客户端重连导致的异常消息，跳过处理
	if len(data) == 0 {
		logrus.WithField("client_id", client.ID).Debug("ACCEPT消息data字段为空，跳过处理")
		return
	}

	transferID, ok := data["transfer_id"].(string)
	if !ok {
		h.sendFileTransferError(client, 400, "缺少transfer_id", "")
		return
	}

	session, err := h.fileTransferService.GetSession(transferID)
	if err != nil {
		h.sendFileTransferError(client, 404, "传输会话不存在", transferID)
		return
	}

	// 验证接收者
	if session.ToUserID != client.UserID {
		h.sendFileTransferError(client, 403, "无权接受此传输", transferID)
		return
	}

	// 验证状态转换合法性
	if session.Status != "pending" {
		h.sendFileTransferError(client, 400, "传输会话状态错误，无法接受: "+session.Status, transferID)
		return
	}

	// 更新会话状态
	h.fileTransferService.UpdateSessionStatus(transferID, "pending", "accepted")
	h.fileTransferService.UpdateSessionClients(transferID, session.FromClientID, client.ID)

	// 通知发送者
	acceptData := map[string]interface{}{
		"transfer_id": transferID,
	}
	if flowControl, ok := data["flow_control"].(string); ok && flowControl != "" {
		acceptData["flow_control"] = flowControl
	}
	if ackEveryChunks, ok := data["ack_every_chunks"].(float64); ok && ackEveryChunks > 0 {
		acceptData["ack_every_chunks"] = int(ackEveryChunks)
	}
	if ackWindowChunks, ok := data["ack_window_chunks"].(float64); ok && ackWindowChunks > 0 {
		acceptData["ack_window_chunks"] = int(ackWindowChunks)
	}

	acceptMsg := model.NewWebSocketMessage(
		model.MessageTypeFileTransferAccept,
		session.RoomName,
		"",
		acceptData,
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

	// 防御性检查：data为空对象时可能是客户端重连导致的异常消息，跳过处理
	if len(data) == 0 {
		logrus.WithField("client_id", client.ID).Debug("REJECT消息data字段为空，跳过处理")
		return
	}

	transferID, ok := data["transfer_id"].(string)
	if !ok {
		h.sendFileTransferError(client, 400, "缺少transfer_id", "")
		return
	}

	session, err := h.fileTransferService.GetSession(transferID)
	if err != nil {
		h.sendFileTransferError(client, 404, "传输会话不存在", transferID)
		return
	}

	// 验证状态转换合法性
	if session.Status != "pending" {
		h.sendFileTransferError(client, 400, "传输会话状态错误，无法拒绝: "+session.Status, transferID)
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

	// 防御性检查：data为空对象时可能是客户端重连导致的异常消息，跳过处理
	if len(data) == 0 {
		logrus.WithField("client_id", client.ID).Debug("START消息data字段为空，跳过处理")
		return
	}

	transferID, ok := data["transfer_id"].(string)
	if !ok {
		h.sendFileTransferError(client, 400, "缺少transfer_id", "")
		return
	}

	session, err := h.fileTransferService.GetSession(transferID)
	if err != nil {
		h.sendFileTransferError(client, 404, "传输会话不存在", transferID)
		return
	}

	// 验证发送者
	if session.FromUserID != client.UserID {
		h.sendFileTransferError(client, 403, "无权开始此传输", transferID)
		return
	}

	// 验证状态转换合法性
	if session.Status != "accepted" && session.Status != "resending" {
		h.sendFileTransferError(client, 400, "传输会话状态错误，无法开始传输: "+session.Status, transferID)
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

	// 防御性检查：data为空对象时可能是客户端重连导致的异常消息，跳过处理
	if len(data) == 0 {
		logrus.WithField("client_id", client.ID).Debug("END消息data字段为空，跳过处理")
		return
	}

	transferID, ok := data["transfer_id"].(string)
	if !ok || transferID == "" {
		// 记录data内容帮助排查移动端重连导致transfer_id丢失的问题
		logrus.WithFields(logrus.Fields{
			"client_id": client.ID,
			"data_keys": getMapKeys(data),
		}).Warn("END消息缺少transfer_id，可能是客户端重连导致data字段不完整")
		h.sendFileTransferError(client, 400, "缺少transfer_id", "")
		return
	}

	session, err := h.fileTransferService.GetSession(transferID)
	if err != nil {
		h.sendFileTransferError(client, 404, "传输会话不存在", transferID)
		return
	}

	// 验证发送者。END 只表示发送端已经发完字节，不能代表接收端已经组装成功。
	if session.FromUserID != client.UserID {
		h.sendFileTransferError(client, 403, "无权结束此传输", transferID)
		return
	}

	// 验证状态转换合法性
	if session.Status != "completed" && session.Status != "transferring" && session.Status != "resending" && session.Status != "interrupted" {
		h.sendFileTransferError(client, 400, "传输会话状态错误，无法结束传输: "+session.Status, transferID)
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
		// data字段损坏，静默跳过——移动网络不稳定时常见，不应因无法解析而产生额外错误
		logrus.WithField("client_id", client.ID).WithError(err).Debug("COMPLETE消息data解析失败，静默跳过")
		return
	}

	// 防御性检查：data为空对象时可能是客户端重连导致的异常消息，跳过处理
	if len(data) == 0 {
		logrus.WithField("client_id", client.ID).Debug("COMPLETE消息data字段为空，跳过处理")
		return
	}

	transferID, ok := data["transfer_id"].(string)
	if !ok || transferID == "" {
		// 记录data内容帮助排查移动端重连导致transfer_id丢失的问题
		// COMPLETE是通知性消息，缺少transfer_id时静默跳过，避免产生级联错误
		logrus.WithFields(logrus.Fields{
			"client_id": client.ID,
			"data_keys": getMapKeys(data),
		}).Debug("COMPLETE消息缺少transfer_id，静默跳过——可能是客户端重连或会话已清理")
		return
	}

	session, err := h.fileTransferService.GetSession(transferID)
	if err != nil {
		h.sendFileTransferError(client, 404, "传输会话不存在", transferID)
		return
	}

	if session.ToUserID != client.UserID {
		h.sendFileTransferError(client, 403, "无权确认此传输", transferID)
		return
	}

	// 验证状态转换合法性 — 接收方可能在发送端 END 到达前就已经组装完毕
	if session.Status == "completed" {
		logrus.WithField("transfer_id", transferID).Debug("ignore duplicate transfer complete")
		return
	}
	if session.Status != "transferring" && session.Status != "ending" && session.Status != "resending" && session.Status != "interrupted" {
		h.sendFileTransferError(client, 400, "传输会话状态错误，无法确认完成: "+session.Status, transferID)
		return
	}

	if err := h.fileTransferService.UpdateSessionStatus(transferID, session.Status, "completed"); err != nil {
		h.sendFileTransferError(client, 409, "transfer session status changed: "+err.Error(), transferID)
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

// handleFileTransferAck forwards receiver-side chunk processing acknowledgements to the sender.
func (h *WebSocketHandler) handleFileTransferAck(client *model.Client, message *model.WebSocketMessage) {
	var data map[string]interface{}
	if err := json.Unmarshal(message.Data, &data); err != nil {
		logrus.WithField("client_id", client.ID).WithError(err).Debug("ACK message data parse failed")
		return
	}

	transferID, ok := data["transfer_id"].(string)
	if !ok || transferID == "" {
		logrus.WithField("client_id", client.ID).Debug("ACK message missing transfer_id")
		return
	}

	session, err := h.fileTransferService.GetSession(transferID)
	if err != nil {
		h.sendFileTransferError(client, 404, "传输会话不存在", transferID)
		return
	}

	if session.ToUserID != client.UserID {
		h.sendFileTransferError(client, 403, "无权确认此传输", transferID)
		return
	}

	if session.Status != "transferring" && session.Status != "resending" && session.Status != "ending" && session.Status != "interrupted" {
		logrus.WithFields(logrus.Fields{
			"transfer_id": transferID,
			"status":      session.Status,
		}).Debug("ignore ACK for inactive transfer session")
		return
	}

	ackMsg := model.NewWebSocketMessage(
		model.MessageTypeFileTransferAck,
		session.RoomName,
		"",
		data,
	)

	if err := h.fileTransferService.SendMessageToUser(session.FromUserID, session.RoomName, ackMsg); err != nil {
		logrus.WithError(err).WithField("transfer_id", transferID).Warn("forward transfer ACK failed")
		return
	}
}

// handleFileTransferResend 处理接收方请求重传缺失分片
func (h *WebSocketHandler) handleFileTransferResend(client *model.Client, message *model.WebSocketMessage) {
	var data map[string]interface{}
	if err := json.Unmarshal(message.Data, &data); err != nil {
		h.sendFileTransferError(client, 400, "数据格式错误", "")
		return
	}

	// 防御性检查：data为空对象时可能是客户端重连导致的异常消息，跳过处理
	if len(data) == 0 {
		logrus.WithField("client_id", client.ID).Debug("RESEND消息data字段为空，跳过处理")
		return
	}

	transferID, ok := data["transfer_id"].(string)
	if !ok || transferID == "" {
		h.sendFileTransferError(client, 400, "缺少transfer_id", "")
		return
	}

	session, err := h.fileTransferService.GetSession(transferID)
	if err != nil {
		h.sendFileTransferError(client, 404, "传输会话不存在", transferID)
		return
	}

	if session.ToUserID != client.UserID {
		h.sendFileTransferError(client, 403, "无权请求此传输重传", transferID)
		return
	}

	// 验证状态转换合法性 — 允许从 transferring 或 ending 进入 resending 恢复模式
	if session.Status == "completed" {
		logrus.WithField("transfer_id", transferID).Debug("ignore late resend request for completed transfer")
		return
	}
	if session.Status != "transferring" && session.Status != "ending" && session.Status != "interrupted" {
		h.sendFileTransferError(client, 400, "传输会话状态错误，无法请求重传: "+session.Status, transferID)
		return
	}

	requestedChunkIndexes, err := parseRequestedChunkIndexes(data["chunk_indexes"], data["missing_count"], session.TotalChunks)
	if err != nil {
		h.sendFileTransferError(client, 400, err.Error(), transferID)
		return
	}
	if len(requestedChunkIndexes) > 0 {
		replayed, missing, replayErr := h.fileTransferService.ReplaySpoolChunksToReceiver(transferID, requestedChunkIndexes)
		if replayErr != nil {
			logrus.WithFields(logrus.Fields{
				"operation":   "relay.spool_replay",
				"transfer_id": transferID,
				"requested":   len(requestedChunkIndexes),
				"replayed":    len(replayed),
				"missing":     len(missing),
				"error":       replayErr.Error(),
			}).Warn("relay spool replay failed")
			if service.IsRelayReceiverUnavailable(replayErr) {
				if state, stateErr := h.fileTransferService.GetResumeState(transferID, client.UserID); stateErr == nil {
					h.sendMessage(client, model.NewWebSocketMessage(
						model.MessageTypeFileTransferResumeState,
						state.RoomName,
						"",
						state,
					))
				}
				return
			}
		}

		if len(replayed) > 0 {
			logrus.WithFields(logrus.Fields{
				"operation":   "relay.spool_replay",
				"transfer_id": transferID,
				"replayed":    replayed,
				"missing":     missing,
			}).Info("replayed relay chunks from server spool")
		}

		if len(replayed) > 0 && len(missing) == 0 {
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
			return
		}

		if len(replayed) > 0 && len(missing) > 0 {
			data["chunk_indexes"] = missing
			data["missing_count"] = len(missing)
		}
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
		h.sendFileTransferError(client, 404, "找不到发送者", transferID)
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
		// data字段损坏，静默跳过——移动网络不稳定时常见，不应因无法解析而产生额外错误
		logrus.WithField("client_id", client.ID).WithError(err).Debug("CANCEL消息data解析失败，静默跳过")
		return
	}

	// 防御性检查：data为空对象时可能是客户端重连导致的异常消息，跳过处理
	if len(data) == 0 {
		logrus.WithField("client_id", client.ID).Debug("CANCEL消息data字段为空，跳过处理")
		return
	}

	transferID, ok := data["transfer_id"].(string)
	if !ok || transferID == "" {
		// CANCEL是通知性消息，缺少transfer_id时静默跳过
		// 传输最终会通过客户端断连或超时清理
		logrus.WithFields(logrus.Fields{
			"client_id": client.ID,
			"data_keys": getMapKeys(data),
		}).Debug("CANCEL消息缺少transfer_id，静默跳过——传输最终将通过断连或超时清理")
		return
	}

	session, err := h.fileTransferService.GetSession(transferID)
	if err != nil {
		h.sendFileTransferError(client, 404, "传输会话不存在", transferID)
		return
	}

	// 验证状态转换合法性 — 终端状态不可取消
	if session.Status == "completed" || session.Status == "cancelled" || session.Status == "error" || session.Status == "rejected" {
		h.sendFileTransferError(client, 400, "传输会话已结束，无法取消: "+session.Status, transferID)
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

	if err := h.fileTransferService.SendMessageToUser(session.FromUserID, session.RoomName, errorMsg); err != nil {
		logrus.WithFields(logrus.Fields{
			"transfer_id": session.TransferID,
			"user_id":     session.FromUserID,
			"error":       err.Error(),
		}).Warn("通知发送方传输错误失败")
	}
	if err := h.fileTransferService.SendMessageToUser(session.ToUserID, session.RoomName, errorMsg); err != nil {
		logrus.WithFields(logrus.Fields{
			"transfer_id": session.TransferID,
			"user_id":     session.ToUserID,
			"error":       err.Error(),
		}).Warn("通知接收方传输错误失败")
	}

	// 延迟清理会话，给客户端时间处理错误消息
	go func() {
		time.Sleep(5 * time.Second)
		h.fileTransferService.RemoveSession(session.TransferID)
	}()
}

// getMapKeys 返回map的所有key，用于调试日志（如排查transfer_id丢失问题）
func getMapKeys(data map[string]interface{}) []string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	return keys
}

func parseRequestedChunkIndexes(value interface{}, missingCountValue interface{}, totalChunks int) ([]int, error) {
	indexes, provided, err := parseChunkIndexes(value)
	if err != nil {
		return nil, err
	}
	if !provided {
		return nil, nil
	}
	if totalChunks <= 0 {
		return nil, fmt.Errorf("invalid total_chunks for resend")
	}
	if len(indexes) == 0 {
		return nil, fmt.Errorf("chunk_indexes cannot be empty")
	}
	if len(indexes) > maxRelayResendChunkIndexes {
		return nil, fmt.Errorf("chunk_indexes exceeds limit: %d > %d", len(indexes), maxRelayResendChunkIndexes)
	}

	seen := make(map[int]bool, len(indexes))
	unique := make([]int, 0, len(indexes))
	for _, index := range indexes {
		if index < 0 || index >= totalChunks {
			return nil, fmt.Errorf("chunk index out of range: %d/%d", index, totalChunks)
		}
		if seen[index] {
			continue
		}
		seen[index] = true
		unique = append(unique, index)
	}

	if missingCount, provided, err := parseNonNegativeInt(missingCountValue); err != nil {
		return nil, fmt.Errorf("invalid missing_count: %w", err)
	} else if provided && missingCount != len(unique) {
		return nil, fmt.Errorf("missing_count mismatch: %d != %d", missingCount, len(unique))
	}

	return unique, nil
}

func parseChunkIndexes(value interface{}) ([]int, bool, error) {
	switch typed := value.(type) {
	case []int:
		return typed, true, nil
	case []float64:
		indexes := make([]int, 0, len(typed))
		for _, item := range typed {
			index, err := parseFloatIndex(item)
			if err != nil {
				return nil, true, err
			}
			indexes = append(indexes, index)
		}
		return indexes, true, nil
	case []interface{}:
		indexes := make([]int, 0, len(typed))
		for _, item := range typed {
			index, err := parseChunkIndexValue(item)
			if err != nil {
				return nil, true, err
			}
			indexes = append(indexes, index)
		}
		return indexes, true, nil
	default:
		if value == nil {
			return nil, false, nil
		}
		return nil, true, fmt.Errorf("chunk_indexes must be an array")
	}
}

func parseChunkIndexValue(value interface{}) (int, error) {
	switch v := value.(type) {
	case int:
		return v, nil
	case int64:
		if v > int64(math.MaxInt) || v < int64(math.MinInt) {
			return 0, fmt.Errorf("chunk index outside int range")
		}
		return int(v), nil
	case float64:
		return parseFloatIndex(v)
	case json.Number:
		parsed, err := v.Int64()
		if err != nil {
			return 0, err
		}
		if parsed > int64(math.MaxInt) || parsed < int64(math.MinInt) {
			return 0, fmt.Errorf("chunk index outside int range")
		}
		return int(parsed), nil
	default:
		return 0, fmt.Errorf("chunk index must be an integer")
	}
}

func parseFloatIndex(value float64) (int, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value {
		return 0, fmt.Errorf("chunk index must be a finite integer")
	}
	if value > float64(math.MaxInt) || value < float64(math.MinInt) {
		return 0, fmt.Errorf("chunk index outside int range")
	}
	return int(value), nil
}

func parseNonNegativeInt(value interface{}) (int, bool, error) {
	if value == nil {
		return 0, false, nil
	}
	parsed, err := parseChunkIndexValue(value)
	if err != nil {
		return 0, true, err
	}
	if parsed < 0 {
		return 0, true, fmt.Errorf("must be non-negative")
	}
	return parsed, true, nil
}

func isConnClosedError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "close sent") ||
		strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "forcibly closed") ||
		strings.Contains(msg, "connection aborted") ||
		strings.Contains(msg, "connection was aborted") ||
		strings.Contains(msg, "i/o timeout") ||
		websocket.IsCloseError(err,
			websocket.CloseNormalClosure,
			websocket.CloseGoingAway,
			websocket.CloseNoStatusReceived,
			websocket.CloseAbnormalClosure,
		)
}
