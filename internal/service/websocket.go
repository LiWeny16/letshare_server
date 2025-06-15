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
}

func NewWebSocketService(maxRoomUsers int) *WebSocketService {
	ws := &WebSocketService{
		clients:      make(map[string]*model.Client),
		rooms:        make(map[string]*model.Room),
		maxRoomUsers: maxRoomUsers,
		roomService:  NewRoomService(),
	}

	// 启动定期清理
	go ws.startMaintenance()

	return ws
}

// AddClient 添加新客户端
func (ws *WebSocketService) AddClient(client *model.Client) {
	ws.clientsMutex.Lock()
	defer ws.clientsMutex.Unlock()

	ws.clients[client.ID] = client

	logrus.WithFields(logrus.Fields{
		"client_id": client.ID,
		"user_id":   client.UserID,
	}).Info("客户端连接")
}

// RemoveClient 移除客户端 - 彻底清理所有引用
func (ws *WebSocketService) RemoveClient(clientID string) {
	ws.clientsMutex.Lock()
	client, exists := ws.clients[clientID]
	if !exists {
		ws.clientsMutex.Unlock()
		return
	}

	// 先从clients map中移除，防止其他goroutine访问
	delete(ws.clients, clientID)
	ws.clientsMutex.Unlock()

	// 彻底清理客户端资源
	if client != nil {
		ws.cleanupClientResources(client)
	}

	logrus.WithField("client_id", clientID).Info("客户端断开")
}

// cleanupClientResources 彻底清理客户端相关资源
func (ws *WebSocketService) cleanupClientResources(client *model.Client) {
	// 关闭WebSocket连接
	if conn, ok := client.Connection.(*websocket.Conn); ok {
		conn.Close()
	}

	// 从所有房间中移除客户端
	roomsToCleanup := make([]string, 0, len(client.Rooms))
	for roomName := range client.Rooms {
		roomsToCleanup = append(roomsToCleanup, roomName)
	}

	for _, roomName := range roomsToCleanup {
		ws.removeClientFromRoom(client.ID, roomName)
	}

	// 清理客户端内部引用
	client.Rooms = nil
	client.Events = nil
	client.Connection = nil
	client.Metadata = nil
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
	}

	// 检查房间是否已满（修复：检查clientID而不是Client指针）
	if len(room.ClientIDs) >= ws.maxRoomUsers {
		if _, exists := room.ClientIDs[clientID]; !exists {
			ws.roomsMutex.Unlock()
			return fmt.Errorf("房间已满，最多支持%d个用户", ws.maxRoomUsers)
		}
	}

	// 添加客户端ID到房间（避免循环引用）
	room.ClientIDs[clientID] = true
	room.UpdatedAt = time.Now()
	ws.roomsMutex.Unlock()

	// 更新客户端信息
	ws.clientsMutex.Lock()
	client.Rooms[roomName] = true
	if event != "" {
		client.Events[event] = true
	} else {
		// 如果没有指定事件，默认订阅所有事件
		client.Events["signal:all"] = true
	}
	ws.clientsMutex.Unlock()

	logrus.WithFields(logrus.Fields{
		"client_id": clientID,
		"user_id":   client.UserID,
		"room":      roomName,
		"event":     event,
		"room_size": len(room.ClientIDs),
	}).Info("客户端订阅房间")

	return nil
}

// UnsubscribeFromRoom 取消订阅房间
func (ws *WebSocketService) UnsubscribeFromRoom(clientID, roomName, event string) error {
	client, exists := ws.GetClient(clientID)
	if !exists {
		return fmt.Errorf("客户端不存在")
	}

	// 如果指定了特定事件，只移除该事件订阅
	if event != "" && event != "signal:all" {
		ws.clientsMutex.Lock()
		delete(client.Events, event)
		ws.clientsMutex.Unlock()

		logrus.WithFields(logrus.Fields{
			"client_id": clientID,
			"room":      roomName,
			"event":     event,
		}).Info("客户端取消事件订阅")

		return nil
	}

	// 完全离开房间
	ws.removeClientFromRoom(clientID, roomName)

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
	for roomClientID := range room.ClientIDs {
		if roomClientID == clientID {
			continue // 不发送给自己
		}

		// 获取房间中的客户端
		roomClient, exists := ws.GetClient(roomClientID)
		if !exists {
			// 客户端不存在，从房间中移除
			ws.removeClientFromRoom(roomClientID, roomName)
			continue
		}

		// 检查事件过滤
		shouldReceive := false
		if event == "" || event == "signal:all" {
			// 广播消息，检查是否订阅了signal:all
			shouldReceive = roomClient.Events["signal:all"]
		} else {
			// 特定事件消息，检查是否订阅了该事件或signal:all
			shouldReceive = roomClient.Events[event] || roomClient.Events["signal:all"]
		}

		if !shouldReceive {
			continue
		}

		ws.sendToClient(roomClient, message)
		count++
	}

	logrus.WithFields(logrus.Fields{
		"client_id":  clientID,
		"user_id":    client.UserID,
		"room":       roomName,
		"event":      event,
		"recipients": count,
		"room_size":  len(room.ClientIDs),
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

// removeClientFromRoom 从房间中移除客户端
func (ws *WebSocketService) removeClientFromRoom(clientID, roomName string) {
	ws.roomsMutex.Lock()
	defer ws.roomsMutex.Unlock()

	room, exists := ws.rooms[roomName]
	if !exists {
		return
	}

	// 从房间中移除客户端ID
	delete(room.ClientIDs, clientID)
	room.UpdatedAt = time.Now()

	// 更新客户端信息（如果客户端还存在）
	if client, exists := ws.GetClient(clientID); exists {
		ws.clientsMutex.Lock()
		delete(client.Rooms, roomName)
		// 清理该房间相关的事件订阅
		for event := range client.Events {
			if event != "signal:all" {
				delete(client.Events, event)
			}
		}
		ws.clientsMutex.Unlock()
	}

	// 如果房间为空，删除房间
	if len(room.ClientIDs) == 0 {
		delete(ws.rooms, roomName)
		logrus.WithField("room", roomName).Debug("空房间已删除")
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
		"client_count": len(room.ClientIDs),
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
		ws.cleanupInactiveClients()
		logger.CleanupLogs()
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
	clientIDs := make([]string, 0, len(ws.clients))
	for clientID := range ws.clients {
		clientIDs = append(clientIDs, clientID)
	}
	ws.clientsMutex.Unlock()

	// 逐个清理客户端
	for _, clientID := range clientIDs {
		ws.RemoveClient(clientID)
	}

	ws.roomsMutex.Lock()
	ws.rooms = make(map[string]*model.Room)
	ws.roomsMutex.Unlock()

	logrus.Info("WebSocket服务已关闭")
}

// GetStats 获取基本统计信息
func (ws *WebSocketService) GetStats() map[string]interface{} {
	ws.clientsMutex.RLock()
	activeConnections := len(ws.clients)
	ws.clientsMutex.RUnlock()

	ws.roomsMutex.RLock()
	totalRooms := len(ws.rooms)
	ws.roomsMutex.RUnlock()

	return map[string]interface{}{
		"active_connections": activeConnections,
		"total_rooms":        totalRooms,
	}
}
