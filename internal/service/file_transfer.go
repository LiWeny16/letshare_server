package service

import (
	"encoding/json"
	"fmt"
	"letshare-server/internal/model"
	"sync"
	"time"

	"github.com/gorilla/websocket"
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
func (fts *FileTransferService) UpdateSessionStatus(transferID, status string) error {
	fts.sessionsMutex.Lock()
	defer fts.sessionsMutex.Unlock()

	session, exists := fts.sessions[transferID]
	if !exists {
		return fmt.Errorf("传输会话不存在: %s", transferID)
	}

	session.Status = status
	session.LastActivity = time.Now()

	logrus.WithFields(logrus.Fields{
		"transfer_id": transferID,
		"status":      status,
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

	// 更新会话活动时间
	fts.sessionsMutex.Lock()
	session.LastActivity = time.Now()
	fts.sessionsMutex.Unlock()

	// 查找接收者客户端
	receiverClient, err := fts.findClientByUserID(session.ToUserID, session.RoomName)
	if err != nil {
		return fmt.Errorf("找不到接收者: %w", err)
	}

	conn, ok := receiverClient.Connection.(*websocket.Conn)
	if !ok {
		return fmt.Errorf("无效的WebSocket连接")
	}

	receiverClient.ConnMutex.Lock()
	defer receiverClient.ConnMutex.Unlock()

	// 发送带帧头的二进制数据，避免接收端依赖“上一条元数据属于下一条二进制”的状态配对
	if err := conn.WriteMessage(websocket.BinaryMessage, framedChunkData); err != nil {
		return fmt.Errorf("转发数据块失败: %w", err)
	}

	// 发送进度更新给发送者
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

	conn, ok := client.Connection.(*websocket.Conn)
	if !ok {
		return fmt.Errorf("无效的WebSocket连接")
	}

	client.ConnMutex.Lock()
	defer client.ConnMutex.Unlock()

	if err := conn.WriteJSON(message); err != nil {
		return fmt.Errorf("发送消息失败: %w", err)
	}

	return nil
}

// findClientByUserID 在房间中查找用户的客户端
func (fts *FileTransferService) findClientByUserID(userID, roomName string) (*model.Client, error) {
	// 遍历所有客户端,查找匹配的用户ID和房间
	fts.wsService.clientsMutex.RLock()
	defer fts.wsService.clientsMutex.RUnlock()

	for _, client := range fts.wsService.clients {
		if client.UserID == userID && client.Rooms[roomName] {
			return client, nil
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
	fts.sessionsMutex.Lock()
	defer fts.sessionsMutex.Unlock()

	timeout := 10 * time.Minute
	now := time.Now()
	var staleTransferIDs []string

	for transferID, session := range fts.sessions {
		// 清理超过10分钟无活动的会话
		if now.Sub(session.LastActivity) > timeout {
			staleTransferIDs = append(staleTransferIDs, transferID)
		}
	}

	for _, transferID := range staleTransferIDs {
		delete(fts.sessions, transferID)
		logrus.WithField("transfer_id", transferID).Info("清理过期传输会话")
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
