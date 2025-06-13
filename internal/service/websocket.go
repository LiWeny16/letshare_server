package service

import (
	"encoding/json"
	"fmt"
	"letshare-server/internal/model"
	"letshare-server/pkg/logger"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

type WebSocketService struct {
	clients      map[string]*model.Client // clientID -> Client
	rooms        map[string]*model.Room   // roomName -> Room  
	clientsMutex sync.RWMutex
	roomsMutex   sync.RWMutex
	maxRoomUsers int
	roomService  *RoomService
	
	// 监控指标
	stats struct {
		TotalConnections    int64
		ActiveConnections   int64
		TotalMessages       int64
		TotalRooms          int64
		MessagesPerSecond   float64
		lastMessageCount    int64
		lastStatsTime       time.Time
		mutex              sync.RWMutex
	}
}

func NewWebSocketService(maxRoomUsers int) *WebSocketService {
	ws := &WebSocketService{
		clients:      make(map[string]*model.Client),
		rooms:        make(map[string]*model.Room),
		maxRoomUsers: maxRoomUsers,
		roomService:  NewRoomService(),
	}
	
	ws.stats.lastStatsTime = time.Now()
	
	// 启动定期清理和统计
	go ws.startMaintenance()
	
	return ws
}

// AddClient 添加新客户端
func (ws *WebSocketService) AddClient(client *model.Client) {
	ws.clientsMutex.Lock()
	defer ws.clientsMutex.Unlock()
	
	ws.clients[client.ID] = client
	
	ws.stats.mutex.Lock()
	ws.stats.TotalConnections++
	ws.stats.ActiveConnections++
	ws.stats.mutex.Unlock()
	
	logrus.WithFields(logrus.Fields{
		"client_id": client.ID,
		"user_id":   client.UserID,
	}).Info("客户端连接")
}

// RemoveClient 移除客户端
func (ws *WebSocketService) RemoveClient(clientID string) {
	ws.clientsMutex.Lock()
	client, exists := ws.clients[clientID]
	if !exists {
		ws.clientsMutex.Unlock()
		return
	}
	delete(ws.clients, clientID)
	ws.clientsMutex.Unlock()
	
	// 从所有房间中移除客户端
	if client != nil {
		for roomName := range client.Rooms {
			ws.leaveRoom(client, roomName)
		}
	}
	
	ws.stats.mutex.Lock()
	ws.stats.ActiveConnections--
	ws.stats.mutex.Unlock()
	
	logrus.WithField("client_id", clientID).Info("客户端断开")
}

// GetClient 获取客户端
func (ws *WebSocketService) GetClient(clientID string) (*model.Client, bool) {
	ws.clientsMutex.RLock()
	defer ws.clientsMutex.RUnlock()
	client, exists := ws.clients[clientID]
	return client, exists
}

// SubscribeToRoom 订阅房间
func (ws *WebSocketService) SubscribeToRoom(clientID, roomName, event string) error {
	// 验证房间名
	if valid, errMsg := ws.roomService.ValidateRoomName(roomName); !valid {
		return fmt.Errorf(errMsg)
	}
	
	client, exists := ws.GetClient(clientID)
	if !exists {
		return fmt.Errorf("客户端不存在")
	}
	
	// 检查房间人数限制
	ws.roomsMutex.Lock()
	room, roomExists := ws.rooms[roomName]
	if !roomExists {
		room = model.NewRoom(roomName)
		ws.rooms[roomName] = room
		ws.stats.mutex.Lock()
		ws.stats.TotalRooms++
		ws.stats.mutex.Unlock()
	}
	
	// 检查房间是否已满
	if len(room.Clients) >= ws.maxRoomUsers && !room.Clients[clientID] {
		ws.roomsMutex.Unlock()
		return fmt.Errorf("房间已满，最多支持%d个用户", ws.maxRoomUsers)
	}
	
	// 添加客户端到房间
	room.Clients[clientID] = client
	room.UpdatedAt = time.Now()
	ws.roomsMutex.Unlock()
	
	// 更新客户端信息
	ws.clientsMutex.Lock()
	client.Rooms[roomName] = true
	if event != "" {
		client.Events[event] = true
	}
	ws.clientsMutex.Unlock()
	
	// 发送订阅确认
	ws.sendToClient(client, model.NewWebSocketMessage(
		model.MessageTypeSubscribed,
		roomName,
		event,
		map[string]interface{}{
			"status": "subscribed",
			"room":   roomName,
		},
	))
	
	logrus.WithFields(logrus.Fields{
		"client_id": clientID,
		"room":      roomName,
		"event":     event,
		"room_size": len(room.Clients),
	}).Info("客户端订阅房间")
	
	return nil
}

// UnsubscribeFromRoom 取消订阅房间
func (ws *WebSocketService) UnsubscribeFromRoom(clientID, roomName, event string) error {
	client, exists := ws.GetClient(clientID)
	if !exists {
		return fmt.Errorf("客户端不存在")
	}
	
	ws.leaveRoom(client, roomName)
	
	logrus.WithFields(logrus.Fields{
		"client_id": clientID,
		"room":      roomName,
		"event":     event,
	}).Info("客户端取消订阅房间")
	
	return nil
}

// PublishToRoom 发布消息到房间
func (ws *WebSocketService) PublishToRoom(clientID, roomName, event string, data json.RawMessage) error {
	client, exists := ws.GetClient(clientID)
	if !exists {
		return fmt.Errorf("客户端不存在")
	}
	
	// 检查客户端是否在房间中
	if !client.Rooms[roomName] {
		return fmt.Errorf("客户端未订阅房间: %s", roomName)
	}
	
	ws.roomsMutex.RLock()
	room, roomExists := ws.rooms[roomName]
	ws.roomsMutex.RUnlock()
	
	if !roomExists {
		return fmt.Errorf("房间不存在: %s", roomName)
	}
	
	// 创建消息
	message := model.NewWebSocketMessage(model.MessageTypeMessage, roomName, event, data)
	
	// 广播到房间中的所有客户端
	count := 0
	for _, roomClient := range room.Clients {
		if roomClient.ID == clientID {
			continue // 不发送给自己
		}
		
		// 检查事件过滤
		if event != "" && !roomClient.Events[event] && !roomClient.Events["signal:all"] {
			continue
		}
		
		ws.sendToClient(roomClient, message)
		count++
	}
	
	ws.stats.mutex.Lock()
	ws.stats.TotalMessages++
	ws.stats.mutex.Unlock()
	
	logrus.WithFields(logrus.Fields{
		"client_id":     clientID,
		"room":          roomName,
		"event":         event,
		"recipients":    count,
		"room_size":     len(room.Clients),
	}).Debug("消息已广播")
	
	return nil
}

// sendToClient 发送消息给客户端
func (ws *WebSocketService) sendToClient(client *model.Client, message *model.WebSocketMessage) {
	conn, ok := client.Connection.(*websocket.Conn)
	if !ok {
		logrus.WithField("client_id", client.ID).Error("WebSocket连接类型错误")
		return
	}
	
	if err := conn.WriteJSON(message); err != nil {
		logrus.WithFields(logrus.Fields{
			"client_id": client.ID,
			"error":     err.Error(),
		}).Error("发送消息失败")
		
		// 连接出错，移除客户端
		ws.RemoveClient(client.ID)
	}
}

// leaveRoom 离开房间
func (ws *WebSocketService) leaveRoom(client *model.Client, roomName string) {
	ws.roomsMutex.Lock()
	defer ws.roomsMutex.Unlock()
	
	room, exists := ws.rooms[roomName]
	if !exists {
		return
	}
	
	// 从房间中移除客户端
	delete(room.Clients, client.ID)
	room.UpdatedAt = time.Now()
	
	// 更新客户端信息
	ws.clientsMutex.Lock()
	delete(client.Rooms, roomName)
	ws.clientsMutex.Unlock()
	
	// 如果房间为空，删除房间
	if len(room.Clients) == 0 {
		delete(ws.rooms, roomName)
		ws.stats.mutex.Lock()
		ws.stats.TotalRooms--
		ws.stats.mutex.Unlock()
		
		logrus.WithField("room", roomName).Debug("空房间已删除")
	}
}

// GetStats 获取统计信息
func (ws *WebSocketService) GetStats() map[string]interface{} {
	ws.stats.mutex.RLock()
	defer ws.stats.mutex.RUnlock()
	
	return map[string]interface{}{
		"total_connections":    ws.stats.TotalConnections,
		"active_connections":   ws.stats.ActiveConnections,
		"total_messages":       ws.stats.TotalMessages,
		"total_rooms":          ws.stats.TotalRooms,
		"messages_per_second":  ws.stats.MessagesPerSecond,
	}
}

// GetRoomInfo 获取房间信息
func (ws *WebSocketService) GetRoomInfo(roomName string) map[string]interface{} {
	ws.roomsMutex.RLock()
	defer ws.roomsMutex.RUnlock()
	
	room, exists := ws.rooms[roomName]
	if !exists {
		return nil
	}
	
	return map[string]interface{}{
		"name":         room.Name,
		"client_count": len(room.Clients),
		"max_users":    ws.maxRoomUsers,
		"created_at":   room.CreatedAt,
		"updated_at":   room.UpdatedAt,
	}
}

// startMaintenance 启动维护任务
func (ws *WebSocketService) startMaintenance() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	
	for range ticker.C {
		ws.updateStats()
		ws.cleanupInactiveClients()
		logger.CleanupLogs()
	}
}

// updateStats 更新统计信息
func (ws *WebSocketService) updateStats() {
	ws.stats.mutex.Lock()
	defer ws.stats.mutex.Unlock()
	
	now := time.Now()
	elapsed := now.Sub(ws.stats.lastStatsTime).Seconds()
	
	if elapsed > 0 {
		messagesDiff := ws.stats.TotalMessages - ws.stats.lastMessageCount
		ws.stats.MessagesPerSecond = float64(messagesDiff) / elapsed
		ws.stats.lastMessageCount = ws.stats.TotalMessages
		ws.stats.lastStatsTime = now
	}
}

// cleanupInactiveClients 清理非活跃客户端
func (ws *WebSocketService) cleanupInactiveClients() {
	ws.clientsMutex.RLock()
	var inactiveClients []string
	timeout := 5 * time.Minute
	
	for clientID, client := range ws.clients {
		if time.Since(client.LastPing) > timeout {
			inactiveClients = append(inactiveClients, clientID)
		}
	}
	ws.clientsMutex.RUnlock()
	
	// 移除非活跃客户端
	for _, clientID := range inactiveClients {
		ws.RemoveClient(clientID)
		logrus.WithField("client_id", clientID).Info("清理非活跃客户端")
	}
}

// Shutdown 关闭服务
func (ws *WebSocketService) Shutdown() {
	logrus.Info("正在关闭WebSocket服务...")
	
	ws.clientsMutex.Lock()
	for clientID, client := range ws.clients {
		if conn, ok := client.Connection.(*websocket.Conn); ok {
			conn.Close()
		}
		delete(ws.clients, clientID)
	}
	ws.clientsMutex.Unlock()
	
	ws.roomsMutex.Lock()
	ws.rooms = make(map[string]*model.Room)
	ws.roomsMutex.Unlock()
	
	logrus.Info("WebSocket服务已关闭")
} 