package service

import (
	"encoding/json"
	"fmt"
	"letshare-server/internal/model"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// FileTransferService 文件传输服务
type FileTransferService struct {
	sessions       map[string]*model.FileTransferSession // transferID -> session
	sessionsMutex  sync.RWMutex
	wsService      *WebSocketService
	maxFileSize    int64  // 最大文件大小(管理员模式)
	chunkSize      int    // 默认分块大小
	basicSizeLimit int64  // 基础大小限制,超过需要管理员密码
	adminPassword  string // 管理员密码
}

// NewFileTransferService 创建文件传输服务
func NewFileTransferService(wsService *WebSocketService, maxFileSize int64, chunkSize int, basicSizeLimit int64, adminPassword string) *FileTransferService {
	fts := &FileTransferService{
		sessions:       make(map[string]*model.FileTransferSession),
		wsService:      wsService,
		maxFileSize:    maxFileSize,
		chunkSize:      chunkSize,
		basicSizeLimit: basicSizeLimit,
		adminPassword:  adminPassword,
	}

	// 启动会话清理
	go fts.startSessionCleanup()

	return fts
}

// CreateTransferSession 创建文件传输会话
func (fts *FileTransferService) CreateTransferSession(request *model.FileTransferRequest) (*model.FileTransferSession, error) {
	// 验证文件大小 — 两阶段检查: ≤50MB 自由传输, >50MB 需要管理员密码
	if request.FileSize > fts.basicSizeLimit {
		if request.AdminPass == "" {
			return nil, fmt.Errorf("文件大小 %.2f MB 超过 %.0f MB 限制,需要管理员密码",
				float64(request.FileSize)/(1024*1024), float64(fts.basicSizeLimit)/(1024*1024))
		}
		if request.AdminPass != fts.adminPassword {
			return nil, fmt.Errorf("管理员密码错误")
		}
	}

	// 验证绝对上限
	if request.FileSize > fts.maxFileSize {
		return nil, fmt.Errorf("文件大小超过限制: %d MB", fts.maxFileSize/(1024*1024))
	}

	// 验证发送者和接收者
	if request.FromUserID == "" || request.ToUserID == "" {
		return nil, fmt.Errorf("必须指定发送者和接收者")
	}

	// 设置默认分块大小
	if request.ChunkSize == 0 {
		request.ChunkSize = fts.chunkSize
	}

	// 计算总块数
	totalChunks := int(request.FileSize / int64(request.ChunkSize))
	if request.FileSize%int64(request.ChunkSize) != 0 {
		totalChunks++
	}
	if totalChunks == 0 {
		totalChunks = 1
	}
	request.TotalChunks = totalChunks

	// 创建会话
	session := &model.FileTransferSession{
		TransferID:   request.TransferID,
		FromUserID:   request.FromUserID,
		ToUserID:     request.ToUserID,
		RoomName:     request.RoomName,
		FileName:     request.FileName,
		FileSize:     request.FileSize,
		FileType:     request.FileType,
		ChunkSize:    request.ChunkSize,
		TotalChunks:  totalChunks,
		StartTime:    time.Now(),
		LastActivity: time.Now(),
		Status:       "pending",
	}

	fts.sessionsMutex.Lock()
	fts.sessions[request.TransferID] = session
	fts.sessionsMutex.Unlock()

	logrus.WithFields(logrus.Fields{
		"transfer_id": request.TransferID,
		"file_name":   request.FileName,
		"file_size":   request.FileSize,
		"from_user":   request.FromUserID,
		"to_user":     request.ToUserID,
		"chunks":      totalChunks,
	}).Info("创建文件传输会话")

	return session, nil
}

// GetSession 获取传输会话
func (fts *FileTransferService) GetSession(transferID string) (*model.FileTransferSession, error) {
	fts.sessionsMutex.RLock()
	defer fts.sessionsMutex.RUnlock()

	session, exists := fts.sessions[transferID]
	if !exists {
		return nil, fmt.Errorf("传输会话不存在: %s", transferID)
	}

	return session, nil
}

// UpdateSessionStatus 更新会话状态
// expectedStatus 为空时跳过 CAS 检查（best-effort），否则仅在当前状态匹配时才更新
func (fts *FileTransferService) UpdateSessionStatus(transferID, expectedStatus, newStatus string) error {
	fts.sessionsMutex.Lock()
	defer fts.sessionsMutex.Unlock()

	session, exists := fts.sessions[transferID]
	if !exists {
		return fmt.Errorf("传输会话不存在: %s", transferID)
	}

	if expectedStatus != "" && session.Status != expectedStatus {
		return fmt.Errorf("传输会话状态已变更: 期望 %s, 实际 %s", expectedStatus, session.Status)
	}

	session.Status = newStatus
	session.LastActivity = time.Now()

	logrus.WithFields(logrus.Fields{
		"transfer_id": transferID,
		"status":      newStatus,
	}).Debug("更新传输会话状态")

	return nil
}

// UpdateSessionClients 更新会话的客户端ID
func (fts *FileTransferService) UpdateSessionClients(transferID, fromClientID, toClientID string) error {
	fts.sessionsMutex.Lock()
	defer fts.sessionsMutex.Unlock()

	session, exists := fts.sessions[transferID]
	if !exists {
		return fmt.Errorf("传输会话不存在: %s", transferID)
	}

	session.FromClientID = fromClientID
	session.ToClientID = toClientID

	return nil
}

// HandleClientDisconnect 处理客户端断开连接，清理该客户端作为发送方或接收方的活跃传输会话
func (fts *FileTransferService) HandleClientDisconnect(clientID string) {
	// 先收集受影响的会话信息，避免锁内发送消息
	type sessionInfo struct {
		transferID   string
		roomName     string
		notifyUserID string
		errorText    string
		logText      string
	}

	fts.sessionsMutex.Lock()
	var affectedSessions []sessionInfo
	for transferID, session := range fts.sessions {
		// 只处理非终端状态的会话
		if session.Status == "completed" || session.Status == "cancelled" ||
			session.Status == "error" || session.Status == "rejected" {
			continue
		}

		if session.FromClientID == clientID {
			session.Status = "error"
			session.LastActivity = time.Now()
			affectedSessions = append(affectedSessions, sessionInfo{
				transferID:   transferID,
				roomName:     session.RoomName,
				notifyUserID: session.ToUserID,
				errorText:    "发送者已断开连接",
				logText:      "发送者断开，已通知接收者并标记传输错误",
			})
		} else if session.ToClientID == clientID {
			session.Status = "error"
			session.LastActivity = time.Now()
			affectedSessions = append(affectedSessions, sessionInfo{
				transferID:   transferID,
				roomName:     session.RoomName,
				notifyUserID: session.FromUserID,
				errorText:    "接收者已断开连接",
				logText:      "接收者断开，已通知发送者并标记传输错误",
			})
		}
	}
	fts.sessionsMutex.Unlock()

	for _, s := range affectedSessions {
		errorMsg := model.NewWebSocketMessage(
			model.MessageTypeFileTransferError,
			s.roomName,
			"",
			map[string]interface{}{
				"transfer_id": s.transferID,
				"error":       s.errorText,
			},
		)
		fts.SendMessageToUser(s.notifyUserID, s.roomName, errorMsg)

		logrus.WithFields(logrus.Fields{
			"transfer_id": s.transferID,
			"client_id":   clientID,
		}).Info(s.logText)

		// 延迟清理会话，给对方时间处理错误消息
		go func(tid string) {
			time.Sleep(5 * time.Second)
			fts.RemoveSession(tid)
		}(s.transferID)
	}
}

// RemoveSession 移除传输会话
func (fts *FileTransferService) RemoveSession(transferID string) {
	fts.sessionsMutex.Lock()
	defer fts.sessionsMutex.Unlock()

	delete(fts.sessions, transferID)

	logrus.WithField("transfer_id", transferID).Info("移除文件传输会话")
}

// ForwardChunkToReceiver 转发数据块给接收者(保留前端二进制帧头，接收端可直接按 transfer_id 定位)
func (fts *FileTransferService) ForwardChunkToReceiver(transferID string, framedChunkData []byte, chunkDataSize int, chunkMeta *model.FileTransferChunk) error {
	session, err := fts.GetSession(transferID)
	if err != nil {
		return err
	}

	// 更新会话活动时间，同时验证会话未被并发删除
	fts.sessionsMutex.Lock()
	if _, stillExists := fts.sessions[transferID]; !stillExists {
		fts.sessionsMutex.Unlock()
		return fmt.Errorf("传输会话已被取消: %s", transferID)
	}
	session.LastActivity = time.Now()
	fts.sessionsMutex.Unlock()

	// 查找接收者客户端
	receiverClient, err := fts.findClientByUserID(session.ToUserID, session.RoomName)
	if err != nil {
		return fmt.Errorf("找不到接收者: %w", err)
	}

	// 发送二进制帧给接收者。锁必须在此释放，
	// 因为后续 sendProgressUpdate → sendToClient 会对同一接收方再次加锁。
	if err := fts.wsService.writeBinaryToClient(receiverClient, framedChunkData); err != nil {
		return fmt.Errorf("转发数据块失败: %w", err)
	}

	// 发送进度更新给发送者（锁已释放，安全）
	bytesTransferred := int64(chunkMeta.ChunkIndex+1) * int64(session.ChunkSize)
	if bytesTransferred > session.FileSize {
		bytesTransferred = session.FileSize
	}

	percentage := 100.0
	if session.FileSize > 0 {
		percentage = float64(bytesTransferred) / float64(session.FileSize) * 100
	}

	progress := &model.FileTransferProgress{
		TransferID:       transferID,
		ChunkIndex:       chunkMeta.ChunkIndex,
		TotalChunks:      chunkMeta.TotalChunks,
		BytesTransferred: bytesTransferred,
		TotalBytes:       session.FileSize,
		Percentage:       percentage,
	}

	fts.sendProgressUpdate(session, progress)

	logrus.WithFields(logrus.Fields{
		"transfer_id": transferID,
		"chunk_index": chunkMeta.ChunkIndex,
		"chunk_size":  chunkDataSize,
		"progress":    fmt.Sprintf("%.2f%%", progress.Percentage),
	}).Debug("转发文件数据块")

	return nil
}

// SendMessageToUser 发送消息给指定用户
func (fts *FileTransferService) SendMessageToUser(userID, roomName string, message *model.WebSocketMessage) error {
	client, err := fts.findClientByUserID(userID, roomName)
	if err != nil {
		return err
	}

	if err := fts.wsService.writeJSONToClient(client, message); err != nil {
		return fmt.Errorf("发送消息失败: %w", err)
	}

	return nil
}

// findClientByUserID 在房间中查找用户的客户端(最多重试3次,移动网络下客户端可能延迟注册)
func (fts *FileTransferService) findClientByUserID(userID, roomName string) (*model.Client, error) {
	for attempt := 0; attempt < 3; attempt++ {
		fts.wsService.clientsMutex.RLock()
		var found *model.Client
		var clientsSnapshot []string
		for _, client := range fts.wsService.clients {
			if client.UserID == userID {
				clientsSnapshot = append(clientsSnapshot, fmt.Sprintf("%s(rooms=%v,status=connected)", client.ID, client.Rooms))
				if client.Rooms[roomName] {
					found = client
				}
			}
		}
		fts.wsService.clientsMutex.RUnlock()

		if found != nil {
			return found, nil
		}

		if attempt < 2 {
			logrus.WithFields(logrus.Fields{
				"user_id": userID,
				"room":    roomName,
				"attempt": attempt + 1,
				"clients": clientsSnapshot,
			}).Warn("找不到接收者，100ms后重试")
			time.Sleep(100 * time.Millisecond)
		} else {
			logrus.WithFields(logrus.Fields{
				"user_id": userID,
				"room":    roomName,
				"clients": clientsSnapshot,
			}).Warn("找不到接收者(已重试3次)")
		}
	}

	return nil, fmt.Errorf("找不到用户: %s 在房间: %s", userID, roomName)
}

// sendProgressUpdate 发送进度更新
func (fts *FileTransferService) sendProgressUpdate(session *model.FileTransferSession, progress *model.FileTransferProgress) {
	progressMsg := model.NewWebSocketMessage(
		model.MessageTypeFileTransferProgress,
		session.RoomName,
		"",
		progress,
	)

	// 发送给发送者
	if err := fts.SendMessageToUser(session.FromUserID, session.RoomName, progressMsg); err != nil {
		logrus.WithError(err).Warn("发送进度更新给发送者失败")
	}

	// 发送给接收者
	if err := fts.SendMessageToUser(session.ToUserID, session.RoomName, progressMsg); err != nil {
		logrus.WithError(err).Warn("发送进度更新给接收者失败")
	}
}

// startSessionCleanup 启动会话清理
func (fts *FileTransferService) startSessionCleanup() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		fts.cleanupStaleSessions()
	}
}

// cleanupStaleSessions 清理过期会话
func (fts *FileTransferService) cleanupStaleSessions() {
	timeout := 10 * time.Minute
	now := time.Now()

	// 收集过期会话信息（避免锁内发送消息）
	type staleInfo struct {
		transferID string
		roomName   string
		fromUserID string
		toUserID   string
	}

	fts.sessionsMutex.Lock()
	var staleSessions []staleInfo
	for transferID, session := range fts.sessions {
		// 清理超过10分钟无活动的会话
		if now.Sub(session.LastActivity) > timeout {
			staleSessions = append(staleSessions, staleInfo{
				transferID: transferID,
				roomName:   session.RoomName,
				fromUserID: session.FromUserID,
				toUserID:   session.ToUserID,
			})
		}
	}
	fts.sessionsMutex.Unlock()

	for _, s := range staleSessions {
		// 通知客户端会话已过期
		errorMsg := model.NewWebSocketMessage(
			model.MessageTypeFileTransferError,
			s.roomName,
			"",
			map[string]interface{}{
				"transfer_id": s.transferID,
				"error":       "传输会话超时",
			},
		)
		fts.SendMessageToUser(s.fromUserID, s.roomName, errorMsg)
		fts.SendMessageToUser(s.toUserID, s.roomName, errorMsg)

		// 删除会话
		fts.sessionsMutex.Lock()
		delete(fts.sessions, s.transferID)
		fts.sessionsMutex.Unlock()

		logrus.WithField("transfer_id", s.transferID).Info("清理过期传输会话")
	}
}

// BroadcastFileTransferMessage 广播文件传输相关消息到房间
func (fts *FileTransferService) BroadcastFileTransferMessage(roomName, event string, data interface{}) error {
	dataBytes, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("序列化数据失败: %w", err)
	}

	return fts.wsService.PublishToRoom("system", roomName, event, dataBytes)
}

// GetStats 获取传输统计信息
func (fts *FileTransferService) GetStats() map[string]interface{} {
	fts.sessionsMutex.RLock()
	defer fts.sessionsMutex.RUnlock()

	statusCount := make(map[string]int)
	for _, session := range fts.sessions {
		statusCount[session.Status]++
	}

	stats := map[string]interface{}{
		"total_sessions": len(fts.sessions),
		"status_count":   statusCount,
	}

	return stats
}
