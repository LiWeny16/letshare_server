package model

import (
	"encoding/json"
	"sync" // Added sync
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
	// 文件传输相关消息类型
	MessageTypeFileTransferRequest  = "file:transfer:request"  // 发起文件传输请求
	MessageTypeFileTransferAccept   = "file:transfer:accept"   // 接受文件传输
	MessageTypeFileTransferReject   = "file:transfer:reject"   // 拒绝文件传输
	MessageTypeFileTransferStart    = "file:transfer:start"    // 开始传输
	MessageTypeFileTransferChunk    = "file:transfer:chunk"    // 传输数据块(二进制)
	MessageTypeFileTransferEnd      = "file:transfer:end"      // 传输完成
	MessageTypeFileTransferComplete = "file:transfer:complete" // 接收方已完成文件组装
	MessageTypeFileTransferResend   = "file:transfer:resend"   // 接收方请求重传缺失分片
	MessageTypeFileTransferCancel   = "file:transfer:cancel"   // 取消传输
	MessageTypeFileTransferError    = "file:transfer:error"    // 传输错误
	MessageTypeFileTransferProgress = "file:transfer:progress" // 传输进度
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
	ConnMutex  sync.Mutex             `json:"-"` // 保护连接写入
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

// FileTransferRequest 文件传输请求信息
type FileTransferRequest struct {
	TransferID  string `json:"transfer_id"`          // 传输会话ID
	FileName    string `json:"file_name"`            // 文件名
	FileSize    int64  `json:"file_size"`            // 文件大小(字节)
	FileType    string `json:"file_type"`            // 文件MIME类型
	ChunkSize   int    `json:"chunk_size"`           // 分块大小(字节)
	TotalChunks int    `json:"total_chunks"`         // 总块数
	FromUserID  string `json:"from_user_id"`         // 发送者用户ID
	ToUserID    string `json:"to_user_id"`           // 接收者用户ID
	RoomName    string `json:"room_name"`            // 房间名称
	AdminPass   string `json:"admin_pass,omitempty"` // 管理员密码(超过基础限制时必需)
}

// FileTransferChunk 文件数据块(用于二进制消息的元数据)
type FileTransferChunk struct {
	TransferID  string `json:"transfer_id"`  // 传输会话ID
	ChunkIndex  int    `json:"chunk_index"`  // 块索引(从0开始)
	ChunkSize   int    `json:"chunk_size"`   // 本块大小
	TotalChunks int    `json:"total_chunks"` // 总块数
	// 实际数据在二进制消息中,不在JSON中
}

// FileTransferProgress 文件传输进度
type FileTransferProgress struct {
	TransferID       string  `json:"transfer_id"`
	ChunkIndex       int     `json:"chunk_index"`
	TotalChunks      int     `json:"total_chunks"`
	BytesTransferred int64   `json:"bytes_transferred"`
	TotalBytes       int64   `json:"total_bytes"`
	Percentage       float64 `json:"percentage"`
}

// FileTransferSession 文件传输会话
type FileTransferSession struct {
	TransferID   string
	FromClientID string
	ToClientID   string
	FromUserID   string
	ToUserID     string
	RoomName     string
	FileName     string
	FileSize     int64
	FileType     string
	ChunkSize    int
	TotalChunks  int
	StartTime    time.Time
	LastActivity time.Time
	Status       string // "pending", "accepted", "transferring", "completed", "cancelled", "error"
}
