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
	sessions      map[string]*model.FileTransferSession // transferID -> session
	sessionsMutex sync.RWMutex
	wsService     *WebSocketService
	maxFileSize   int64 // 最大文件大小
	chunkSize     int   // 默认分块大小
}

// NewFileTransferService 创建文件传输服务
func NewFileTransferService(wsService *WebSocketService, maxFileSize int64, chunkSize int) *FileTransferService {
	fts := &FileTransferService{
		sessions:    make(map[string]*model.FileTransferSession),
		wsService:   wsService,
		maxFileSize: maxFileSize,
		chunkSize:   chunkSize,
	}

	// 启动会话清理
	go fts.startSessionCleanup()

	return fts
}

// CreateTransferSession 创建文件传输会话
func (fts *FileTransferService) CreateTransferSession(request *model.FileTransferRequest) (*model.FileTransferSession, error) {
	// 验证文件大小
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

// ForwardChunkToReceiver 转发数据块给接收者(零拷贝转发)
func (fts *FileTransferService) ForwardChunkToReceiver(transferID string, chunkData []byte, chunkMeta *model.FileTransferChunk) error {
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

	// 发送元数据(JSON格式)
	metaMsg := model.NewWebSocketMessage(
		model.MessageTypeFileTransferChunk,
		session.RoomName,
		"",
		chunkMeta,
	)

	conn, ok := receiverClient.Connection.(*websocket.Conn)
	if !ok {
		return fmt.Errorf("无效的WebSocket连接")
	}

	// 先发送JSON元数据
	if err := conn.WriteJSON(metaMsg); err != nil {
		return fmt.Errorf("发送块元数据失败: %w", err)
	}

	// 再发送二进制数据(零拷贝转发)
	if err := conn.WriteMessage(websocket.BinaryMessage, chunkData); err != nil {
		return fmt.Errorf("转发数据块失败: %w", err)
	}

	// 发送进度更新给发送者
	bytesTransferred := int64(chunkMeta.ChunkIndex+1) * int64(session.ChunkSize)
	if bytesTransferred > session.FileSize {
		bytesTransferred = session.FileSize
	}

	progress := &model.FileTransferProgress{
		TransferID:       transferID,
		ChunkIndex:       chunkMeta.ChunkIndex,
		TotalChunks:      chunkMeta.TotalChunks,
		BytesTransferred: bytesTransferred,
		TotalBytes:       session.FileSize,
		Percentage:       float64(bytesTransferred) / float64(session.FileSize) * 100,
	}

	fts.sendProgressUpdate(session, progress)

	logrus.WithFields(logrus.Fields{
		"transfer_id": transferID,
		"chunk_index": chunkMeta.ChunkIndex,
		"chunk_size":  len(chunkData),
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

	return map[string]interface{}{
		"total_sessions": len(fts.sessions),
		"status_count":   statusCount,
	}
}
