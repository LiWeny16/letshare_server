package handler

import (
	"encoding/json"
	"letshare-server/internal/model"
	"letshare-server/internal/service"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		// CORS检查在中间件中处理，这里允许所有来源
		return true
	},
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

type WebSocketHandler struct {
	wsService   *service.WebSocketService
	authService *service.AuthService
}

func NewWebSocketHandler(wsService *service.WebSocketService, authService *service.AuthService) *WebSocketHandler {
	return &WebSocketHandler{
		wsService:   wsService,
		authService: authService,
	}
}

// HandleWebSocket 处理WebSocket连接
func (h *WebSocketHandler) HandleWebSocket(c *gin.Context) {
	// 从查询参数获取token
	token := c.Query("token")
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
	defer conn.Close()
	
	// 创建客户端
	clientID := uuid.New().String()
	// 由于不再需要JWT的用户信息，使用客户端ID作为用户ID
	client := model.NewClient(clientID, clientID, conn)
	client.Metadata["authenticated"] = true
	
	// 添加到服务
	h.wsService.AddClient(client)
	defer h.wsService.RemoveClient(clientID)
	
	// 设置连接参数
	conn.SetReadLimit(512 * 1024) // 512KB
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		client.LastPing = time.Now()
		return nil
	})
	
	// 启动ping定时器
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	
	// 处理消息
	go h.handleMessages(client, conn)
	
	// 保持连接和定期ping
	for {
		select {
		case <-ticker.C:
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				logrus.WithField("client_id", clientID).WithError(err).Error("发送ping失败")
				return
			}
		}
	}
}

// handleMessages 处理客户端消息
func (h *WebSocketHandler) handleMessages(client *model.Client, conn *websocket.Conn) {
	for {
		var message model.WebSocketMessage
		if err := conn.ReadJSON(&message); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				logrus.WithField("client_id", client.ID).WithError(err).Error("WebSocket连接异常关闭")
			}
			break
		}
		
		// 更新最后活跃时间
		client.LastPing = time.Now()
		
		// 处理不同类型的消息
		h.processMessage(client, &message)
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
	
	// 默认事件为signal:all，也支持特定事件如signal:user-id
	event := message.Event
	if event == "" {
		event = "signal:all"
	}
	
	if err := h.wsService.SubscribeToRoom(client.ID, message.Channel, event); err != nil {
		h.sendError(client, 400, err.Error())
		return
	}
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
	
	if err := conn.WriteJSON(message); err != nil {
		logrus.WithFields(logrus.Fields{
			"client_id": client.ID,
			"error":     err.Error(),
		}).Error("发送消息失败")
	}
}

// sendError 发送错误消息
func (h *WebSocketHandler) sendError(client *model.Client, code int, message string) {
	logrus.WithFields(logrus.Fields{
		"client_id": client.ID,
		"code":      code,
		"message":   message,
	}).Warn("发送错误消息")
	
	errorMsg := model.NewErrorMessage(code, message)
	h.sendMessage(client, errorMsg)
} 