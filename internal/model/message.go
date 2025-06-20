package model

import (
	"encoding/json"
	"time"
)

// WebSocket消息类型
const (
	MessageTypeSubscribe   = "subscribe"
	MessageTypeUnsubscribe = "unsubscribe"
	MessageTypePublish     = "publish"
	MessageTypeSubscribed  = "subscribed"
	MessageTypeMessage     = "message"
	MessageTypeError       = "error"
)

// WebSocketMessage 表示WebSocket消息（兼容Ably格式）
type WebSocketMessage struct {
	Type      string          `json:"type"`
	Channel   string          `json:"channel,omitempty"`
	Event     string          `json:"event,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Timestamp int64           `json:"timestamp,omitempty"`
	Error     *ErrorInfo      `json:"error,omitempty"`
}

// ErrorInfo 表示错误信息
type ErrorInfo struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Client 表示WebSocket客户端
type Client struct {
	ID         string                 `json:"id"`
	UserID     string                 `json:"user_id"`
	Connection interface{}            `json:"-"` // WebSocket连接
	Rooms      map[string]bool        `json:"rooms"`
	Events     map[string]bool        `json:"events"` // 订阅的事件
	LastPing   time.Time              `json:"last_ping"`
	Metadata   map[string]interface{} `json:"metadata"`
}

// Room 表示房间
type Room struct {
	Name      string          `json:"name"`
	ClientIDs map[string]bool `json:"client_ids"` // 存储客户端ID而不是指针，避免循环引用
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// NewWebSocketMessage 创建新的WebSocket消息
func NewWebSocketMessage(msgType, channel, event string, data interface{}) *WebSocketMessage {
	msg := &WebSocketMessage{
		Type:      msgType,
		Channel:   channel,
		Event:     event,
		Timestamp: time.Now().UnixMilli(),
	}

	if data != nil {
		if dataBytes, err := json.Marshal(data); err == nil {
			msg.Data = dataBytes
		}
	}

	return msg
}

// NewErrorMessage 创建错误消息
func NewErrorMessage(code int, message string) *WebSocketMessage {
	return &WebSocketMessage{
		Type: MessageTypeError,
		Error: &ErrorInfo{
			Code:    code,
			Message: message,
		},
		Timestamp: time.Now().UnixMilli(),
	}
}

// NewClient 创建新客户端
func NewClient(id, userID string, conn interface{}) *Client {
	return &Client{
		ID:         id,
		UserID:     userID,
		Connection: conn,
		Rooms:      make(map[string]bool),
		Events:     make(map[string]bool),
		LastPing:   time.Now(),
		Metadata:   make(map[string]interface{}),
	}
}

// NewRoom 创建新房间
func NewRoom(name string) *Room {
	return &Room{
		Name:      name,
		ClientIDs: make(map[string]bool), // 改为存储客户端ID
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
}
