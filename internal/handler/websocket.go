package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"letshare-server/internal/model"
	"letshare-server/internal/service"
	"letshare-server/internal/sfu"
	"math"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
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
	sfuManager          *sfu.Manager
	// activeMeetingRooms 保存当前合法的（已登记）会议号 → *meetingMeta。
	// 会议号与 SFU 房间名（message.Channel）是同一个值；加入前需在此存在才放行。
	activeMeetingRooms sync.Map
	// activeMeetingInvites 保存进行中的会议邀请：inviteId → *meetingInvite（单次邀请状态机）。
	activeMeetingInvites sync.Map
	// inviteMu serializes duplicate detection with reservation before delivery.
	inviteMu sync.Mutex
}

// meetingMeta 会议元数据：房主权威 + breakout 父子关系 + 房主断线宽限定时器。
// 挂在 activeMeetingRooms 的 value 上（指针），自身细粒度加锁，避免全局锁竞争。
type meetingMeta struct {
	// BreakoutMembers is immutable after child-room creation and restricts
	// direct joins to users explicitly assigned by the host.
	BreakoutMembers map[string]bool
	Host            string // 房主 UserID（uniqId）
	Title           string // 会议标题（可选，≤64 字）
	Parent          string // breakout 房间指向主会议号；主会议为空
	mu              sync.Mutex
	endTimer        *time.Timer // 房主断线宽限期后自动结束会议（刷新场景由重入取消）
	presentation    meetingPresentationState
	excalidraw      meetingExcalidrawState
	minutes         meetingMinutesState
}

type meetingPresentationState struct {
	Mode      string `json:"mode"`
	BoardMode string `json:"boardMode,omitempty"`
	OwnerID   string `json:"ownerId"`
	Epoch     uint64 `json:"epoch"`
}

type meetingExcalidrawState struct {
	Revision uint64
	Scene    json.RawMessage
}

// meetingMinutesState intentionally contains no provider API key. Keys stay in
// the host browser and are never placed on the meeting WebSocket.
type meetingMinutesState struct {
	Configured      bool            `json:"configured"`
	Running         bool            `json:"running"`
	RequireConsent  bool            `json:"requireConsent"`
	AsrSource       string          `json:"asrSource,omitempty"`
	AsrModel        string          `json:"asrModel,omitempty"`
	SummaryProvider string          `json:"summaryProvider,omitempty"`
	SummaryModel    string          `json:"summaryModel,omitempty"`
	Summary         string          `json:"summary,omitempty"`
	Consented       map[string]bool `json:"-"`
}

const meetingExcalidrawMaxSceneBytes = 384 * 1024

// hostLeaveGrace 房主意外断线（刷新/网络抖动）后等待其重入的宽限期。
// 到点仍未回到房间则结束会议并释放资源，防止僵尸会议占用会议号与 SFU 内存。
const hostLeaveGrace = 12 * time.Second

// meetingInviteTTL 邀请有效期：超时未响应按过期处理（accept 迟到亦会被服务端拒绝）。
// var 以便测试注入更短的 TTL。
var meetingInviteTTL = 60 * time.Second

// meetingInvite 单次会议邀请状态机（inviteId → 状态），挂在 activeMeetingInvites 上。
type meetingInvite struct {
	InviteID   string
	MeetingID  string
	From       string // 房主 UserID（uniqId）
	To         string // 被邀请方 UserID（uniqId）
	SourceRoom string // 原始 LetShare 房间号（与会议号分开，仅作定向投递通道）
	ExpiresAt  time.Time
	mu         sync.Mutex
	Status     string // "pending" / "accepted" / "rejected" / "expired"
}

// meetingInviteMsg meeting:invite 上行数据（房主发起 / 被邀请方响应共用一个类型）。
type meetingInviteMsg struct {
	Action     string `json:"action"` // invite / accept / reject
	To         string `json:"to"`
	SourceRoom string `json:"sourceRoomId"`
	InviteURL  string `json:"inviteUrl"`
	InviteID   string `json:"inviteId"`
}

// meetingChatMaxLen 会议聊天单条文本上限（字节）。
const meetingChatMaxLen = 2000

func NewWebSocketHandler(wsService *service.WebSocketService, authService *service.AuthService, fileTransferService *service.FileTransferService, jwtService *service.JWTService) *WebSocketHandler {
	handler := &WebSocketHandler{
		wsService:           wsService,
		authService:         authService,
		fileTransferService: fileTransferService,
		jwtService:          jwtService,
		errorRateLimiter:    NewErrorRateLimiter(),
		// 默认 SFU Manager：生产使用空 SettingEngine（真实 ICE）。
		// 测试可用 SetSFU 注入离线引擎。
		sfuManager: sfu.MustNewManager(&webrtc.SettingEngine{}),
	}

	// 注册客户端断开回调：当 WebSocket 客户端断开时，清理其作为发送方的文件传输会话
	wsService.SetOnClientDisconnect(func(clientID string) {
		fileTransferService.HandleClientDisconnect(clientID)
	})

	return handler
}

// SetSFU 注入/替换 SFU Manager（主要用于测试注入离线回环引擎；生产直接用默认 Manager）。
func (h *WebSocketHandler) SetSFU(m *sfu.Manager) {
	if m != nil {
		h.sfuManager = m
	}
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
		// 会议侧资源清理：SFU 参与者、空房会议号释放、房主宽限定时
		h.cleanupMeetingState(client)

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
	case model.MessageTypeMeetingCreate:
		h.handleMeetingCreate(client, message)
	case model.MessageTypeMeetingUpdate:
		h.handleMeetingUpdate(client, message)
	case model.MessageTypeMeetingJoin:
		h.handleMeetingJoin(client, message)
	case model.MessageTypeMeetingLeave:
		h.handleMeetingLeave(client, message)
	case model.MessageTypeMeetingSDP:
		h.handleMeetingSDP(client, message)
	case model.MessageTypeMeetingICE:
		h.handleMeetingICE(client, message)
	case model.MessageTypeMeetingEnd:
		h.handleMeetingEnd(client, message)
	case model.MessageTypeMeetingKick:
		h.handleMeetingKick(client, message)
	case model.MessageTypeMeetingMediaControl:
		h.handleMeetingMediaControl(client, message)
	case model.MessageTypeMeetingChat:
		h.handleMeetingChat(client, message)
	case model.MessageTypeMeetingDraw:
		h.handleMeetingDraw(client, message)
	case model.MessageTypeMeetingBreakout:
		h.handleMeetingBreakout(client, message)
	case model.MessageTypeMeetingInvite:
		h.handleMeetingInvite(client, message)
	case model.MessageTypeMeetingPresentation:
		h.handleMeetingPresentation(client, message)
	case model.MessageTypeMeetingExcalidraw:
		h.handleMeetingExcalidraw(client, message)
	case model.MessageTypeMeetingMinutes:
		h.handleMeetingMinutes(client, message)
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

	// Presence 中心化：订阅成功后下发权威成员表快照，并广播入房事件给房间内其它成员。
	h.wsService.SendMembershipSnapshot(client.ID, message.Channel)
	h.wsService.BroadcastMembershipEvent(message.Channel, "membership:changed", map[string]interface{}{
		"type":   "join",
		"userId": client.UserID,
	}, client.UserID)

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

// ============ Meeting / SFU 会议媒体通道 ============
// 信令方向约定（客户端为 uniqId，等于 client.UserID）：
//   - meeting:join  → 在 SFU 房间登记该客户端（幂等）。
//   - meeting:sdp（offer，无 to）→ 发布 PC：Participant.Offer → answer 定向回发布者。
//   - meeting:sdp（offer，to=发布者）→ 订阅请求：订阅者发起，服务器建 Subscriber 并用其
//     （服务端生成的）offer 定向回订阅者；订阅者回 answer（to=发布者）→ Subscriber.SetRemoteDescription。
//   - meeting:ice（无 to）→ 发布 PC 候选；meeting:ice（to=发布者）→ 订阅 PC 候选。
//   服务器侧每条 PC 的本地候选经 Participant/Subscriber 的 OnICECandidate 定向回发客户端。

type meetingSDPMsg struct {
	Type string `json:"type"`
	SDP  string `json:"sdp"`
	To   string `json:"to"`
}

type meetingICEMsg struct {
	Candidate json.RawMessage `json:"candidate"`
	To        string          `json:"to"`
}

func (h *WebSocketHandler) handleMeetingCreate(client *model.Client, message *model.WebSocketMessage) {
	// 可选会议标题（≤64 字，超长截断）。
	title := ""
	if len(message.Data) > 0 {
		var d struct {
			Title string `json:"title"`
		}
		if json.Unmarshal(message.Data, &d) == nil {
			title = strings.TrimSpace(d.Title)
			if len(title) > 64 {
				title = title[:64]
			}
		}
	}
	// 生成 4 位数字会议号，循环直到不与已登记会议冲突。
	var roomID string
	for i := 0; i < 10000; i++ {
		candidate := fmt.Sprintf("%04d", rand.Intn(10000))
		if _, loaded := h.activeMeetingRooms.LoadOrStore(candidate, &meetingMeta{
			Host: client.UserID,
			Title: title,
			minutes: meetingMinutesState{RequireConsent: true},
		}); !loaded {
			roomID = candidate
			break
		}
	}
	if roomID == "" {
		h.sendError(client, 500, "meeting:create 会议号生成失败")
		return
	}
	// 回发创建的会议号给发起者（channel 设为会议号，便于后续 join 使用同一值）。
	h.sendMessage(client, model.NewWebSocketMessage(
		model.MessageTypeMeetingCreate,
		roomID,
		"signal:"+client.UserID,
		map[string]interface{}{"roomId": roomID, "host": client.UserID},
	))
}

// handleMeetingUpdate updates metadata for a reserved meeting before the host joins.
// Only the registered host may change the title.
func (h *WebSocketHandler) handleMeetingUpdate(client *model.Client, message *model.WebSocketMessage) {
	if message.Channel == "" {
		h.sendError(client, 400, "meeting:update 缺少会议房间")
		return
	}
	v, ok := h.activeMeetingRooms.Load(message.Channel)
	if !ok {
		h.sendError(client, 404, "会议不存在")
		return
	}
	meta, ok := v.(*meetingMeta)
	if !ok || meta == nil {
		h.sendError(client, 404, "会议不存在")
		return
	}
	if meta.Host != client.UserID {
		h.sendError(client, 403, "仅房主可以更新会议")
		return
	}
	var update struct {
		Title string `json:"title"`
	}
	if len(message.Data) > 0 {
		if err := json.Unmarshal(message.Data, &update); err != nil {
			h.sendError(client, 400, "meeting:update 数据格式错误")
			return
		}
	}
	title := strings.TrimSpace(update.Title)
	if len(title) > 64 {
		title = title[:64]
	}
	meta.mu.Lock()
	meta.Title = title
	meta.mu.Unlock()
	// Before join there is no SFU participant. Once the host has joined, refresh
	// all current members so the title stays consistent for everyone.
	if room, ok := h.sfuManager.GetRoom(message.Channel); ok {
		for _, participant := range room.Participants() {
			if participant == nil || participant.ID == "" {
				continue
			}
			h.sendMeetingInfo(message.Channel, participant.ID, meta)
		}
	}
}

func (h *WebSocketHandler) handleMeetingJoin(client *model.Client, message *model.WebSocketMessage) {
	if message.Channel == "" {
		h.sendError(client, 400, "meeting:join 缺少房间")
		return
	}
	// 加入前校验：会议号必须已登记。不存在则拒绝，防止任意号码自动建房。
	v, ok := h.activeMeetingRooms.Load(message.Channel)
	if !ok {
		h.sendError(client, 404, "会议不存在")
		return
	}
	meta, _ := v.(*meetingMeta)
	if meta != nil && meta.Parent != "" && !meta.BreakoutMembers[client.UserID] {
		h.sendError(client, 403, "breakout 房间仅允许受邀成员加入")
		return
	}
	// 任何成员成功加入都取消挂起的自动结束定时器（宽限期语义：空房等待重入）。
	if meta != nil {
		meta.mu.Lock()
		if meta.endTimer != nil {
			meta.endTimer.Stop()
			meta.endTimer = nil
		}
		meta.mu.Unlock()
	}
	room := h.sfuManager.JoinRoom(message.Channel)
	// 发布者中途新增 track（屏幕共享等）→ 通知所有已订阅者重协商
	room.SetOnTrackPublished(h.onMeetingTrackPublished)
	// 幂等：已存在则不重复建 PC
	if _, ok := room.GetParticipant(client.UserID); ok {
		h.sendMeetingInfo(message.Channel, client.UserID, meta)
		return
	}
	p, err := room.AddParticipant(client.UserID)
	if err != nil {
		h.sendError(client, 500, "meeting:join 失败: "+err.Error())
		return
	}
	// 接入该参与者发布 PC 的本地候选，定向回发给客户端
	p.OnICECandidate(func(c *webrtc.ICECandidate) {
		h.forwardMeetingICE(message.Channel, client.UserID, c, "")
	})
	// 定向告知会议信息（房主/标题）：前端用于房主徽章、控制按钮和无媒体入会状态。
	h.sendMeetingInfo(message.Channel, client.UserID, meta)
}

// sendMeetingInfo 是 meeting:join 的逻辑确认。它与媒体采集/PeerConnection
// 解耦：即使设备不存在或浏览器暂时没有建立媒体 PC，客户端也已经是有效会议成员。
func (h *WebSocketHandler) sendMeetingInfo(roomID, userID string, meta *meetingMeta) {
	if meta == nil {
		return
	}
	meta.mu.Lock()
	presentation := meta.presentation
	excalidrawScene := append(json.RawMessage(nil), meta.excalidraw.Scene...)
	excalidrawRevision := meta.excalidraw.Revision
	minutes := meta.minutes
	minutes.Consented = nil
	host := meta.Host
	title := meta.Title
	meta.mu.Unlock()
	h.wsService.SendDirectedToUser(roomID, userID, "signal:"+userID, model.MessageTypeMeetingInfo, map[string]interface{}{
		"roomId": roomID, "host": host, "title": title, "minutes": minutes,
		"joined": true, "presentation": presentation,
	})
	if len(excalidrawScene) > 0 {
		h.wsService.SendDirectedToUser(roomID, userID, "signal:"+userID, model.MessageTypeMeetingExcalidraw, map[string]interface{}{
			"action": "snapshot", "revision": excalidrawRevision, "scene": json.RawMessage(excalidrawScene),
		})
	}
}

func (h *WebSocketHandler) handleMeetingLeave(client *model.Client, message *model.WebSocketMessage) {
	roomName := message.Channel
	if roomName == "" {
		return
	}
	// 房主显式离开 = 结束会议（无主机转移机制，避免留下无人管控的僵尸会议）。
	if v, ok := h.activeMeetingRooms.Load(roomName); ok {
		if meta, _ := v.(*meetingMeta); meta != nil && meta.Host == client.UserID {
			h.endMeetingRoom(roomName, "host-left")
			return
		}
	}
	if room, ok := h.sfuManager.GetRoom(roomName); ok {
		h.releaseMeetingPresentation(roomName, client.UserID)
		_ = room.RemoveParticipant(client.UserID)
		// 房间已无参与者则整体移除，同时释放该会议号以便后续复用
		if room.Count() == 0 {
			h.teardownMeeting(roomName, false)
		}
	}
}

// endMeetingRoom 房主主动结束会议：通知房间内所有成员 + 级联拆除 breakout 子房间 + 释放资源。
func (h *WebSocketHandler) endMeetingRoom(roomID, reason string) {
	if room, ok := h.sfuManager.GetRoom(roomID); ok && room.Count() > 0 {
		h.broadcastMeetingMembers(roomID, room, model.MessageTypeMeetingEnded, map[string]interface{}{
			"roomId": roomID, "reason": reason,
		}, "")
	}
	h.teardownMeeting(roomID, true)
}

// teardownMeeting 拆除一个会议房间：停宽限定时器、关 SFU 房间（关闭所有 PC）、
// 释放会议号，并级联拆除其 breakout 子房间。
func (h *WebSocketHandler) teardownMeeting(roomID string, notify bool) {
	if v, loaded := h.activeMeetingRooms.LoadAndDelete(roomID); loaded {
		if meta, _ := v.(*meetingMeta); meta != nil {
			meta.mu.Lock()
			if meta.endTimer != nil {
				meta.endTimer.Stop()
				meta.endTimer = nil
			}
			meta.mu.Unlock()
		}
	}
	if notify {
		if room, ok := h.sfuManager.GetRoom(roomID); ok && room.Count() > 0 {
			h.broadcastMeetingMembers(roomID, room, model.MessageTypeMeetingEnded, map[string]interface{}{
				"roomId": roomID, "reason": "ended",
			}, "")
		}
	}
	h.sfuManager.RemoveRoom(roomID)
	// 级联：拆除以本房间为父的 breakout 房间（递归深度恒为 1，Parent 指向主会议号）。
	h.activeMeetingRooms.Range(func(key, value any) bool {
		if meta, ok := value.(*meetingMeta); ok && meta.Parent == roomID {
			h.teardownMeeting(key.(string), notify)
		}
		return true
	})
}

// hasRegistered 判断会议号是否仍登记（不区分 SFU 房间是否已建）。
func (h *WebSocketHandler) hasRegistered(roomID string) bool {
	_, ok := h.activeMeetingRooms.Load(roomID)
	return ok
}

// cleanupMeetingState 客户端断线后的会议侧清理：
//  1. 从所有 SFU 房间移除其参与者；房间变空则启动宽限定时（重入即取消），不立即回收——
//     正常流程"创建→路由跳转(WS 弹跳)→join"依赖这一点。
//  2. 房主登记了会议但从未进入（无 SFU 房间）：同样宽限后回收，防"创建即泄漏"。
func (h *WebSocketHandler) cleanupMeetingState(client *model.Client) {
	for _, roomID := range h.sfuManager.Rooms() {
		room, ok := h.sfuManager.GetRoom(roomID)
		if !ok {
			continue
		}
		if _, exists := room.GetParticipant(client.UserID); !exists {
			continue
		}
		h.releaseMeetingPresentation(roomID, client.UserID)
		_ = room.RemoveParticipant(client.UserID)
		if room.Count() == 0 {
			h.scheduleMeetingEnd(roomID)
		}
	}
	// 登记但从未进入的会议（房主断线）：宽限后回收。
	h.activeMeetingRooms.Range(func(key, value any) bool {
		roomID, _ := key.(string)
		meta, ok := value.(*meetingMeta)
		if !ok || meta.Host != client.UserID || meta.Parent != "" {
			return true
		}
		if _, roomExists := h.sfuManager.GetRoom(roomID); !roomExists {
			h.scheduleMeetingEnd(roomID)
		}
		return true
	})
}

// scheduleMeetingEnd 为会议安排宽限期后自动结束：到点时若房间已有成员（重入）则跳过，
// 否则通知+拆除+释放会议号。幂等：已有定时器时不重复安排。
func (h *WebSocketHandler) scheduleMeetingEnd(roomID string) {
	v, ok := h.activeMeetingRooms.Load(roomID)
	if !ok {
		return
	}
	meta, _ := v.(*meetingMeta)
	if meta == nil {
		return
	}
	meta.mu.Lock()
	if meta.endTimer == nil {
		meta.endTimer = time.AfterFunc(hostLeaveGrace, func() {
			if room, ok := h.sfuManager.GetRoom(roomID); ok && room.Count() > 0 {
				return // 有人重入，会议继续
			}
			h.endMeetingRoom(roomID, "timeout")
		})
	}
	meta.mu.Unlock()
}

func (h *WebSocketHandler) handleMeetingSDP(client *model.Client, message *model.WebSocketMessage) {
	if message.Channel == "" {
		h.sendError(client, 400, "meeting:sdp 缺少房间")
		return
	}
	var m meetingSDPMsg
	if err := json.Unmarshal(message.Data, &m); err != nil {
		h.sendError(client, 400, "meeting:sdp 数据格式错误: "+err.Error())
		return
	}

	room, ok := h.sfuManager.GetRoom(message.Channel)
	if !ok {
		h.sendError(client, 400, "meeting:sdp 尚未加入该会议房间")
		return
	}
	part, ok := room.GetParticipant(client.UserID)
	if !ok {
		h.sendError(client, 400, "meeting:sdp 请先 meeting:join")
		return
	}

	switch {
	case m.Type == "offer" && m.To == "":
		// 客户端发布 offer（本人主 PC）
		offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: m.SDP}
		answer, err := part.Offer(offer)
		if err != nil {
			h.sendError(client, 500, "meeting:sdp 生成 answer 失败: "+err.Error())
			return
		}
		reply := map[string]interface{}{"type": "answer", "sdp": answer.SDP, "to": client.UserID}
		h.wsService.SendDirectedToUser(message.Channel, client.UserID, "signal:"+client.UserID, model.MessageTypeMeetingSDP, reply)
		// Pion 可能在 Offer 返回后异步触发 OnTrack；为避免“轨已注册但
		// 迟发布回调恰好错过”的竞态，发布成功后做一个有界最终一致性补偿。
		go h.reconcileMeetingSubscriptionsEventually(message.Channel, client.UserID)

	case m.Type == "offer" && m.To != "":
		// 订阅请求：订阅者要求订阅 m.To 发布者；服务器建 Subscriber 并把其 offer 回发订阅者
		sub, created, err := part.SubscribeTo(m.To)
		if err != nil {
			h.sendError(client, 400, "meeting:sdp 订阅失败: "+err.Error())
			// 发布者可能正在加入或刚完成 publish；不要让一次早到的请求
			// 决定最终结果，后续由 SFU 侧 reconcile 在 track 就绪后补推 offer。
			go h.reconcileMeetingSubscriptionsEventually(message.Channel, m.To)
			return
		}
		if !created {
			// 幂等重试：若首个 offer 可能在网络中丢失，只重发仍在等待 answer
			// 的同一个 offer；稳定连接不重复协商。
			if offer, pending := sub.OfferForRetry(); pending {
				h.sendMeetingSubscriberOffer(message.Channel, client.UserID, m.To, offer)
			}
			return
		}
		// 接入该订阅 PC 的本地候选，定向回发给订阅者（默认引擎非离线需真实 ICE）
		sub.OnICECandidate(func(c *webrtc.ICECandidate) {
			h.forwardMeetingICE(message.Channel, client.UserID, c, m.To)
		})
		h.sendMeetingSubscriberOffer(message.Channel, client.UserID, m.To, sub.Offer())

	case m.Type == "answer" && m.To != "":
		// 订阅者回 answer（订阅 PC）：按发布者 m.To 找到对应 Subscriber
		sub, ok := part.GetSubscriber(m.To)
		if !ok {
			h.sendError(client, 400, fmt.Sprintf("meeting:sdp 未找到订阅 %s 的连接", m.To))
			return
		}
		if err := sub.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m.SDP}); err != nil {
			h.sendError(client, 500, "meeting:sdp answer 设置失败: "+err.Error())
			return
		}
		// 新轨道可能在上一 offer 等待 answer 期间到达。answer 确认后，
		// 生成并发送一个合并后的后续 offer，闭合音频/摄像头/屏幕的迟发布窗口。
		if next, ok, err := sub.PendingOffer(); err != nil {
			h.sendError(client, 500, "meeting:sdp 后续 offer 生成失败: "+err.Error())
			return
		} else if ok {
			h.sendMeetingSubscriberOffer(message.Channel, client.UserID, m.To, next)
		}

	case m.Type == "answer" && m.To == "":
		// 发布 PC 的重协商 answer（少见）
		if err := part.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m.SDP}); err != nil {
			h.sendError(client, 500, "meeting:sdp answer 设置失败: "+err.Error())
			return
		}

	default:
		h.sendError(client, 400, "meeting:sdp 未知的 type/direction 组合")
	}
}

func (h *WebSocketHandler) handleMeetingICE(client *model.Client, message *model.WebSocketMessage) {
	if message.Channel == "" {
		h.sendError(client, 400, "meeting:ice 缺少房间")
		return
	}
	var m meetingICEMsg
	if err := json.Unmarshal(message.Data, &m); err != nil {
		h.sendError(client, 400, "meeting:ice 数据格式错误: "+err.Error())
		return
	}
	var init webrtc.ICECandidateInit
	if len(m.Candidate) > 0 {
		_ = json.Unmarshal(m.Candidate, &init)
	}

	room, ok := h.sfuManager.GetRoom(message.Channel)
	if !ok {
		h.sendError(client, 400, "meeting:ice 尚未加入该会议房间")
		return
	}
	part, ok := room.GetParticipant(client.UserID)
	if !ok {
		h.sendError(client, 400, "meeting:ice 请先 meeting:join")
		return
	}

	if m.To == "" {
		// 发布 PC 候选
		if err := part.AddICECandidate(init); err != nil {
			h.sendError(client, 500, "meeting:ice 添加失败: "+err.Error())
			return
		}
		return
	}
	// 订阅 PC 候选（to=发布者）
	sub, ok := part.GetSubscriber(m.To)
	if !ok {
		h.sendError(client, 400, fmt.Sprintf("meeting:ice 未找到订阅 %s 的连接", m.To))
		return
	}
	if err := sub.AddICECandidate(init); err != nil {
		h.sendError(client, 500, "meeting:ice 添加失败: "+err.Error())
		return
	}
}

// onMeetingTrackPublished 发布者新增 track 时的扇出：
//   - 已订阅该发布者的成员：推送重协商 offer（客户端 answer 走既有 meeting:sdp answer 通道）；
//   - 尚未建立订阅的成员（订阅早于发布被拒、重试窗口已耗尽）：自动建立订阅并推送其 offer ——
//     「publisher 尚未 ready」从此变成服务器侧等待 + 主动补推，订阅方无需感知重试节奏。
func (h *WebSocketHandler) onMeetingTrackPublished(roomID, publisherID string, track *webrtc.TrackRemote) {
	room, ok := h.sfuManager.GetRoom(roomID)
	if !ok {
		return
	}
	logrus.WithFields(logrus.Fields{
		"room": roomID, "publisher": publisherID, "kind": track.Kind().String(), "participants": room.Count(),
	}).Info("meeting track published: reconcile subscribers")
	for _, p := range room.Participants() {
		if p.ID == publisherID {
			continue
		}
		if sub, ok := p.GetSubscriber(publisherID); ok && !sub.IsClosed() {
			offer, err := sub.AddPublishedTrack(track)
			if err != nil {
				logrus.WithError(err).WithFields(logrus.Fields{
					"room": roomID, "publisher": publisherID, "subscriber": p.ID,
				}).Warn("会议 track 扇出重协商失败")
				continue
			}
			if offer.SDP == "" {
				continue // 幂等跳过
			}
			h.sendMeetingSubscriberOffer(roomID, p.ID, publisherID, offer)
			continue
		}
		// 无有效订阅（旧订阅已关闭/不存在）：为迟发布自动建订阅并推送 offer
		sub, created, err := p.SubscribeTo(publisherID)
		if err != nil {
			logrus.WithError(err).WithFields(logrus.Fields{
				"room": roomID, "publisher": publisherID, "subscriber": p.ID,
			}).Debug("迟发布自动补订阅未成功")
			continue
		}
		if !created {
			continue // 并发路径已有人建订阅并推送
		}
		sub.OnICECandidate(func(c *webrtc.ICECandidate) {
			h.forwardMeetingICE(roomID, p.ID, c, publisherID)
		})
		h.sendMeetingSubscriberOffer(roomID, p.ID, publisherID, sub.Offer())
	}
}

// reconcileMeetingSubscriptionsEventually closes the timing gap between a
// participant join/publish request and Pion's asynchronous OnTrack callback.
// It is deliberately bounded: this is recovery for the join race, not a
// permanent polling loop. Once a subscriber exists, only a pending offer is
// resent; no duplicate peer connection is created.
func (h *WebSocketHandler) reconcileMeetingSubscriptionsEventually(roomID, publisherID string) {
	for attempt := 0; attempt < 10; attempt++ {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		h.reconcileMeetingSubscriptions(roomID, publisherID)
	}
}

func (h *WebSocketHandler) reconcileMeetingSubscriptions(roomID, publisherID string) {
	room, ok := h.sfuManager.GetRoom(roomID)
	if !ok {
		return
	}
	publisher, ok := room.GetParticipant(publisherID)
	if !ok || publisher.IsClosed() {
		return
	}
	if len(publisher.PublishedTracks()) == 0 {
		return
	}
	for _, subscriber := range room.Participants() {
		if subscriber.ID == publisherID || subscriber.IsClosed() {
			continue
		}
		sub, created, err := subscriber.SubscribeTo(publisherID)
		if err != nil {
			continue
		}
		if created {
			sub.OnICECandidate(func(c *webrtc.ICECandidate) {
				h.forwardMeetingICE(roomID, subscriber.ID, c, publisherID)
			})
			h.sendMeetingSubscriberOffer(roomID, subscriber.ID, publisherID, sub.Offer())
			continue
		}
		if offer, pending := sub.OfferForRetry(); pending {
			h.sendMeetingSubscriberOffer(roomID, subscriber.ID, publisherID, offer)
		}
	}
}

// sendMeetingSubscriberOffer 统一发送订阅方向 offer，避免不同入口遗漏 to/channel。
func (h *WebSocketHandler) sendMeetingSubscriberOffer(roomID, subscriberID, publisherID string, offer webrtc.SessionDescription) {
	if offer.SDP == "" {
		return
	}
	logrus.WithFields(logrus.Fields{
		"room": roomID, "subscriber": subscriberID, "publisher": publisherID, "sdpLength": len(offer.SDP),
	}).Info("meeting subscriber offer sent")
	h.wsService.SendDirectedToUser(roomID, subscriberID, "signal:"+subscriberID, model.MessageTypeMeetingSDP, map[string]interface{}{
		"type": "offer", "sdp": offer.SDP, "to": publisherID,
	})
}

// forwardMeetingICE 把服务器侧某条 PC 的本地候选定向回发给客户端。
// pubID 为发布者时表示订阅 PC 的候选；为空表示发布 PC 的候选。
func (h *WebSocketHandler) forwardMeetingICE(roomName, toUserID string, c *webrtc.ICECandidate, pubID string) {
	if c == nil {
		// ice 收集完成哨兵：nil 候选，跳过（客户端可自行判断收尾）
		return
	}
	init := c.ToJSON()
	payload := map[string]interface{}{"candidate": init, "to": pubID}
	h.wsService.SendDirectedToUser(roomName, toUserID, "signal:"+toUserID, model.MessageTypeMeetingICE, payload)
}

// handleMeetingEnd 房主结束会议：全员收到 meeting:ended 后退出，会议号与 SFU 资源全部释放。
func (h *WebSocketHandler) handleMeetingEnd(client *model.Client, message *model.WebSocketMessage) {
	roomID := message.Channel
	if roomID == "" {
		return
	}
	v, ok := h.activeMeetingRooms.Load(roomID)
	if !ok {
		return // 已结束/不存在：幂等静默
	}
	if meta, _ := v.(*meetingMeta); meta == nil || meta.Host != client.UserID {
		h.sendError(client, 403, "仅房主可结束会议")
		return
	}
	h.endMeetingRoom(roomID, "host-ended")
}

// handleMeetingKick 房主移出成员：目标收到定向 meeting:kicked，其余成员经成员表更新感知。
func (h *WebSocketHandler) handleMeetingKick(client *model.Client, message *model.WebSocketMessage) {
	roomID := message.Channel
	if roomID == "" {
		return
	}
	var m struct {
		To string `json:"to"`
	}
	if len(message.Data) > 0 {
		if err := json.Unmarshal(message.Data, &m); err != nil || m.To == "" {
			h.sendError(client, 400, "meeting:kick 缺少目标")
			return
		}
	}
	v, ok := h.activeMeetingRooms.Load(roomID)
	if !ok {
		return
	}
	meta, _ := v.(*meetingMeta)
	if meta == nil || meta.Host != client.UserID {
		h.sendError(client, 403, "仅房主可移出成员")
		return
	}
	if m.To == client.UserID || m.To == meta.Host {
		h.sendError(client, 400, "不能移出房主")
		return
	}
	// 从 SFU 房间移除（断其上下行媒体）
	if room, ok := h.sfuManager.GetRoom(roomID); ok {
		h.releaseMeetingPresentation(roomID, m.To)
		_ = room.RemoveParticipant(m.To)
	}
	// 定向通知被移出者（其前端自行 leaveMeeting + 退出导航）
	h.wsService.SendDirectedToUser(roomID, m.To, "signal:"+m.To, model.MessageTypeMeetingKicked, map[string]interface{}{
		"roomId": roomID,
	})
	// 广播成员表变更，让其余成员即时更新列表（被移出者随后 meeting:leave 幂等）
	h.wsService.BroadcastMembershipEvent(roomID, "membership:changed", map[string]interface{}{
		"type": "leave", "userId": m.To,
	}, m.To)
}

type meetingMediaControlMsg struct {
	Action string `json:"action"` // mute-all / request-unmute
}

// handleMeetingMediaControl 会议主持人媒体控制：服务端只负责鉴权和定向广播，
// 具体的浏览器麦克风状态仍由每个客户端自己的 meetingManager 修改。
func (h *WebSocketHandler) handleMeetingMediaControl(client *model.Client, message *model.WebSocketMessage) {
	roomID := message.Channel
	if roomID == "" {
		h.sendError(client, 400, "meeting:media-control 缺少房间")
		return
	}
	v, ok := h.activeMeetingRooms.Load(roomID)
	if !ok {
		h.sendError(client, 404, "会议不存在")
		return
	}
	meta, _ := v.(*meetingMeta)
	room, roomOK := h.sfuManager.GetRoom(roomID)
	if meta == nil || !roomOK || !meetingParticipantIDs(room)[client.UserID] {
		h.sendError(client, 403, "meeting:media-control 发送者不是该会议成员")
		return
	}
	if meta.Host != client.UserID {
		h.sendError(client, 403, "只有主持人可以控制全员麦克风")
		return
	}
	var m meetingMediaControlMsg
	if err := json.Unmarshal(message.Data, &m); err != nil {
		h.sendError(client, 400, "meeting:media-control 数据格式错误")
		return
	}
	if m.Action != "mute-all" && m.Action != "request-unmute" {
		h.sendError(client, 400, "meeting:media-control 未知 action")
		return
	}
	h.broadcastMeetingMembers(roomID, room, model.MessageTypeMeetingMediaControl, map[string]interface{}{
		"action": m.Action,
		"from":   client.UserID,
		"ts":     time.Now().UnixMilli(),
	}, client.UserID)
}

// handleMeetingChat 会议内实时聊天：服务器纯转发（不落盘、不存历史）。
//   - 无 to：房间广播（与会成员可见）。
//   - 带 to：定向私聊 —— 服务器校验发送者和目标都是当前会议成员
//     （以 SFU 房间参与者为准，meeting:join 后才算成员），
//     禁止跨会议、跨房间投递（目标只按 meetingID 房间内查找）。
//
// 文本长度服务端封顶，防止小水管被大包滥用。
func (h *WebSocketHandler) handleMeetingChat(client *model.Client, message *model.WebSocketMessage) {
	roomID := message.Channel
	if roomID == "" {
		return
	}
	room, ok := h.sfuManager.GetRoom(roomID)
	if !ok {
		h.sendError(client, 400, "meeting:chat 尚未加入该会议房间")
		return
	}
	// 发送者必须是当前会议成员（SFU 参与者；防跨会议、跨房间伪造投递）
	memberIDs := meetingParticipantIDs(room)
	if !memberIDs[client.UserID] {
		h.sendError(client, 403, "meeting:chat 发送者不是该会议成员")
		return
	}
	var m struct {
		Text string `json:"text"`
		To   string `json:"to"`
	}
	if err := json.Unmarshal(message.Data, &m); err != nil {
		h.sendError(client, 400, "meeting:chat 数据格式错误")
		return
	}
	text := strings.TrimSpace(m.Text)
	if text == "" {
		return
	}
	if len(text) > meetingChatMaxLen {
		text = text[:meetingChatMaxLen]
	}

	// 定向私聊：目标必须仍是本会议成员（roomID == meetingID，杜绝跨会议/跨房间）
	if m.To != "" {
		if m.To == client.UserID {
			return
		}
		if !memberIDs[m.To] {
			h.sendError(client, 404, "目标成员不在当前会议中")
			return
		}
		delivered := h.wsService.SendDirectedToUser(roomID, m.To, "signal:"+m.To, model.MessageTypeMeetingChat, map[string]interface{}{
			"from": client.UserID, "text": text, "ts": time.Now().UnixMilli(), "to": m.To,
		})
		if !delivered {
			h.sendError(client, 404, "目标成员已离开会议")
		}
		return
	}

	h.broadcastMeetingMembers(roomID, room, model.MessageTypeMeetingChat, map[string]interface{}{
		"from": client.UserID, "text": text, "ts": time.Now().UnixMilli(),
	}, client.UserID) // 排除发送者：其 UI 已本地回显，省一次回环
}

// meetingParticipantIDs 汇总 SFU 会议房间的参与者 UserID 集（会议成员权威名单）。
func meetingParticipantIDs(room *sfu.Room) map[string]bool {
	ids := make(map[string]bool)
	if room == nil {
		return ids
	}
	for _, p := range room.Participants() {
		if p != nil && p.ID != "" {
			ids[p.ID] = true
		}
	}
	return ids
}

// handleMeetingDraw 协作画板：服务器纯转发笔画/清空操作，所有成员同步渲染。
func (h *WebSocketHandler) handleMeetingDraw(client *model.Client, message *model.WebSocketMessage) {
	roomID := message.Channel
	if roomID == "" {
		return
	}
	room, ok := h.sfuManager.GetRoom(roomID)
	if !ok {
		h.sendError(client, 400, "meeting:draw 尚未加入该会议房间")
		return
	}
	if !meetingParticipantIDs(room)[client.UserID] {
		h.sendError(client, 403, "meeting:draw 发送者不是该会议成员")
		return
	}
	// 原样转发（数据已在 conn 层 512KB 限制内），补充发送者便于去重回显。
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(message.Data, &payload); err != nil {
		return
	}
	payload["from"] = json.RawMessage(fmt.Sprintf("%q", client.UserID))
	data, _ := json.Marshal(payload)
	h.broadcastMeetingMembers(roomID, room, model.MessageTypeMeetingDraw, json.RawMessage(data), client.UserID) // 排除发送者：本地已实时绘制
}

type meetingMinutesMsg struct {
	Action          string `json:"action"`
	RequireConsent  bool   `json:"requireConsent"`
	AsrSource       string `json:"asrSource"`
	AsrModel        string `json:"asrModel"`
	SummaryProvider string `json:"summaryProvider"`
	SummaryModel    string `json:"summaryModel"`
	Accepted        bool   `json:"accepted"`
	SegmentID       string `json:"segmentId"`
	Text            string `json:"text"`
	StartMs         int64  `json:"startMs"`
	EndMs           int64  `json:"endMs"`
	Final           bool   `json:"final"`
	Summary         string `json:"summary"`
}

// handleMeetingMinutes owns the non-media AI collaboration plane. Provider
// credentials are intentionally not accepted here: they remain in the host
// browser and are used only for the BYOK request.
func (h *WebSocketHandler) handleMeetingMinutes(client *model.Client, message *model.WebSocketMessage) {
	roomID := message.Channel
	if roomID == "" {
		h.sendError(client, 400, "meeting:minutes 缺少会议房间")
		return
	}
	v, ok := h.activeMeetingRooms.Load(roomID)
	meta, metaOK := v.(*meetingMeta)
	room, roomOK := h.sfuManager.GetRoom(roomID)
	if !ok || !metaOK || meta == nil || !roomOK {
		h.sendError(client, 404, "会议不存在")
		return
	}
	if !meetingParticipantIDs(room)[client.UserID] {
		h.sendError(client, 403, "meeting:minutes 发送者不是会议成员")
		return
	}
	var m meetingMinutesMsg
	if err := json.Unmarshal(message.Data, &m); err != nil {
		h.sendError(client, 400, "meeting:minutes 数据格式错误")
		return
	}

	meta.mu.Lock()
	if meta.minutes.Consented == nil {
		meta.minutes.Consented = make(map[string]bool)
	}
	state := meta.minutes
	meta.mu.Unlock()

	switch m.Action {
	case "configure":
		if meta.Host != client.UserID {
			h.sendError(client, 403, "仅主持人可以配置会议纪要")
			return
		}
		if m.AsrSource != "browser-speech" && m.AsrSource != "wasm" && m.AsrSource != "iflytek" {
			h.sendError(client, 400, "不支持的 ASR 来源")
			return
		}
		if m.SummaryProvider != "mimo" && m.SummaryProvider != "openai" && m.SummaryProvider != "anthropic" && m.SummaryProvider != "custom" {
			h.sendError(client, 400, "不支持的摘要供应商")
			return
		}
		if len(m.AsrModel) > 64 || len(m.SummaryModel) > 128 {
			h.sendError(client, 400, "模型名称过长")
			return
		}
		meta.mu.Lock()
		meta.minutes = meetingMinutesState{
			Configured: true, Running: false, RequireConsent: m.RequireConsent,
			AsrSource: m.AsrSource, AsrModel: strings.TrimSpace(m.AsrModel),
			SummaryProvider: m.SummaryProvider, SummaryModel: strings.TrimSpace(m.SummaryModel),
			Consented: make(map[string]bool),
		}
		state = meta.minutes
		state.Consented = nil
		meta.mu.Unlock()
		h.broadcastMeetingMembers(roomID, room, model.MessageTypeMeetingMinutes, map[string]interface{}{"kind": "configured", "minutes": state}, "")
	case "start":
		if meta.Host != client.UserID {
			h.sendError(client, 403, "仅主持人可以启动会议纪要")
			return
		}
		meta.mu.Lock()
		if !meta.minutes.Configured {
			meta.mu.Unlock()
			h.sendError(client, 409, "请先配置会议纪要来源")
			return
		}
		meta.minutes.Running = true
		state = meta.minutes
		state.Consented = nil
		meta.mu.Unlock()
		h.broadcastMeetingMembers(roomID, room, model.MessageTypeMeetingMinutes, map[string]interface{}{"kind": "started", "minutes": state}, "")
	case "stop":
		if meta.Host != client.UserID {
			h.sendError(client, 403, "仅主持人可以停止会议纪要")
			return
		}
		meta.mu.Lock()
		meta.minutes.Running = false
		state = meta.minutes
		state.Consented = nil
		meta.mu.Unlock()
		h.broadcastMeetingMembers(roomID, room, model.MessageTypeMeetingMinutes, map[string]interface{}{"kind": "stopped", "minutes": state}, "")
	case "consent":
		meta.mu.Lock()
		meta.minutes.Consented[client.UserID] = m.Accepted
		meta.mu.Unlock()
		h.broadcastMeetingMembers(roomID, room, model.MessageTypeMeetingMinutes, map[string]interface{}{"kind": "consent", "userId": client.UserID, "accepted": m.Accepted}, "")
	case "segment":
		meta.mu.Lock()
		running := meta.minutes.Running
		requireConsent := meta.minutes.RequireConsent
		consented := meta.minutes.Consented[client.UserID]
		meta.mu.Unlock()
		if !running {
			h.sendError(client, 409, "会议纪要尚未启动")
			return
		}
		if requireConsent && !consented {
			h.sendError(client, 403, "璇峰厛鍚屾剰浼氳绾褰曢煶杞啓")
			return
		}
		text := strings.TrimSpace(m.Text)
		if text == "" || len(text) > 4000 {
			h.sendError(client, 400, "转写片段为空或过长")
			return
		}
		segmentID := strings.TrimSpace(m.SegmentID)
		if segmentID == "" {
			segmentID = fmt.Sprintf("%s:%d", client.UserID, time.Now().UnixNano())
		}
		speakerName := client.UserID
		if index := strings.IndexByte(speakerName, ':'); index > 0 {
			speakerName = speakerName[:index]
		}
		h.broadcastMeetingMembers(roomID, room, model.MessageTypeMeetingMinutes, map[string]interface{}{
			"kind": "segment", "segmentId": segmentID, "from": client.UserID, "speakerName": speakerName,
			"text": text, "startMs": m.StartMs, "endMs": m.EndMs, "final": m.Final,
		}, "")
	case "summary":
		if meta.Host != client.UserID {
			h.sendError(client, 403, "仅主持人可以提交会议摘要")
			return
		}
		if len(m.Summary) > 128*1024 {
			h.sendError(client, 400, "会议摘要过长")
			return
		}
		meta.mu.Lock()
		meta.minutes.Summary = strings.TrimSpace(m.Summary)
		meta.minutes.Running = false
		state = meta.minutes
		state.Consented = nil
		meta.mu.Unlock()
		h.broadcastMeetingMembers(roomID, room, model.MessageTypeMeetingMinutes, map[string]interface{}{"kind": "summary", "summary": state.Summary, "minutes": state}, "")
	default:
		h.sendError(client, 400, "meeting:minutes 未知 action")
	}
}

// broadcastMeetingMembers 只向已登记 SFU 参与者投递会议协作事件。
// 会议频道可能同时被原始房间/旧客户端订阅，不能把“订阅频道”误当成“会议成员”。
func (h *WebSocketHandler) broadcastMeetingMembers(roomID string, room *sfu.Room, msgType string, payload interface{}, exclude string) {
	if room == nil {
		return
	}
	for _, participant := range room.Participants() {
		if participant == nil || participant.ID == "" || participant.ID == exclude {
			continue
		}
		h.wsService.SendDirectedToUser(roomID, participant.ID, "signal:"+participant.ID, msgType, payload)
	}
}

type meetingPresentationMsg struct {
	Action    string `json:"action"`    // claim / release
	Mode      string `json:"mode"`      // screen / whiteboard
	BoardMode string `json:"boardMode"` // basic / excalidraw
}

// handleMeetingPresentation 是展示主持权的唯一写入口：同一时刻只有一名
// owner，任意会议成员可以 claim（抢占旧 owner），当前 owner 或房主可以 release。
func (h *WebSocketHandler) handleMeetingPresentation(client *model.Client, message *model.WebSocketMessage) {
	roomID := message.Channel
	if roomID == "" {
		h.sendError(client, 400, "meeting:presentation 缺少房间")
		return
	}
	v, ok := h.activeMeetingRooms.Load(roomID)
	if !ok {
		h.sendError(client, 404, "会议不存在")
		return
	}
	meta, _ := v.(*meetingMeta)
	room, roomOK := h.sfuManager.GetRoom(roomID)
	if meta == nil || !roomOK || !meetingParticipantIDs(room)[client.UserID] {
		h.sendError(client, 403, "meeting:presentation 发送者不是该会议成员")
		return
	}
	var m meetingPresentationMsg
	if err := json.Unmarshal(message.Data, &m); err != nil {
		h.sendError(client, 400, "meeting:presentation 数据格式错误")
		return
	}
	if m.Action != "claim" && m.Action != "release" {
		h.sendError(client, 400, "meeting:presentation 未知 action")
		return
	}
	if m.Action == "claim" && m.Mode != "screen" && m.Mode != "whiteboard" {
		h.sendError(client, 400, "meeting:presentation 未知 mode")
		return
	}
	if m.Action == "claim" && m.Mode == "whiteboard" && m.BoardMode != "basic" && m.BoardMode != "excalidraw" {
		m.BoardMode = "excalidraw"
	}

	meta.mu.Lock()
	state := meta.presentation
	switch m.Action {
	case "claim":
		if state.OwnerID != client.UserID || state.Mode != m.Mode || state.BoardMode != m.BoardMode {
			state.Epoch++
			state.OwnerID = client.UserID
			state.Mode = m.Mode
			state.BoardMode = ""
			if m.Mode == "whiteboard" {
				state.BoardMode = m.BoardMode
			}
		}
		meta.presentation = state
	case "release":
		if state.OwnerID != client.UserID && meta.Host != client.UserID {
			meta.mu.Unlock()
			h.sendError(client, 403, "只有当前展示者或房主可以停止展示")
			return
		}
		if state.OwnerID != "" || state.Mode != "" {
			state.Epoch++
			state.OwnerID = ""
			state.Mode = ""
			state.BoardMode = ""
		}
		meta.presentation = state
	}
	meta.mu.Unlock()
	h.broadcastMeetingMembers(roomID, room, model.MessageTypeMeetingPresentation, state, "")
}

type meetingExcalidrawMsg struct {
	Action   string          `json:"action"` // update / request
	Revision uint64          `json:"revision"`
	Scene    json.RawMessage `json:"scene"`
}

// handleMeetingExcalidraw 保存受限大小的最新场景快照并只向真实会议成员
// 广播。服务端生成 revision；客户端 revision 过期时不得覆盖最新场景。
func (h *WebSocketHandler) handleMeetingExcalidraw(client *model.Client, message *model.WebSocketMessage) {
	roomID := message.Channel
	room, ok := h.sfuManager.GetRoom(roomID)
	if !ok {
		h.sendError(client, 400, "meeting:excalidraw 尚未加入该会议房间")
		return
	}
	if !meetingParticipantIDs(room)[client.UserID] {
		h.sendError(client, 403, "meeting:excalidraw 发送者不是该会议成员")
		return
	}
	v, loaded := h.activeMeetingRooms.Load(roomID)
	meta, metaOK := v.(*meetingMeta)
	if !loaded || !metaOK || meta == nil {
		h.sendError(client, 404, "会议不存在")
		return
	}
	var m meetingExcalidrawMsg
	if err := json.Unmarshal(message.Data, &m); err != nil {
		h.sendError(client, 400, "meeting:excalidraw 数据格式错误")
		return
	}
	if m.Action == "request" {
		meta.mu.Lock()
		scene := append(json.RawMessage(nil), meta.excalidraw.Scene...)
		revision := meta.excalidraw.Revision
		meta.mu.Unlock()
		if len(scene) > 0 {
			h.wsService.SendDirectedToUser(roomID, client.UserID, "signal:"+client.UserID, model.MessageTypeMeetingExcalidraw, map[string]interface{}{
				"action": "snapshot", "revision": revision, "scene": json.RawMessage(scene),
			})
		}
		return
	}
	if m.Action != "update" || len(m.Scene) == 0 || len(m.Scene) > meetingExcalidrawMaxSceneBytes {
		h.sendError(client, 400, "meeting:excalidraw 场景为空或超过大小限制")
		return
	}
	var scene struct {
		Elements json.RawMessage `json:"elements"`
	}
	if err := json.Unmarshal(m.Scene, &scene); err != nil || len(bytes.TrimSpace(scene.Elements)) == 0 || bytes.TrimSpace(scene.Elements)[0] != '[' {
		h.sendError(client, 400, "meeting:excalidraw 场景必须包含 elements 数组")
		return
	}
	meta.mu.Lock()
	meta.excalidraw.Revision++
	revision := meta.excalidraw.Revision
	meta.excalidraw.Scene = append(json.RawMessage(nil), m.Scene...)
	stored := append(json.RawMessage(nil), meta.excalidraw.Scene...)
	meta.mu.Unlock()
	h.broadcastMeetingMembers(roomID, room, model.MessageTypeMeetingExcalidraw, map[string]interface{}{
		"action": "snapshot", "revision": revision, "scene": json.RawMessage(stored),
	}, "")
}

func (h *WebSocketHandler) releaseMeetingPresentation(roomID, userID string) {
	v, loaded := h.activeMeetingRooms.Load(roomID)
	meta, metaOK := v.(*meetingMeta)
	if !loaded || !metaOK || meta == nil {
		return
	}
	meta.mu.Lock()
	state := meta.presentation
	if state.OwnerID != userID {
		meta.mu.Unlock()
		return
	}
	state.Epoch++
	state.OwnerID = ""
	state.Mode = ""
	state.BoardMode = ""
	meta.presentation = state
	meta.mu.Unlock()
	if room, ok := h.sfuManager.GetRoom(roomID); ok {
		h.broadcastMeetingMembers(roomID, room, model.MessageTypeMeetingPresentation, state, "")
	}
}

// handleMeetingBreakout 分组讨论：
//   - action=create（房主）：登记 breakout 子房间（Parent=主会议号），向每个成员定向下发 invite。
//   - action=recall（房主）：向所有 breakout 房间成员定向下发召回，并拆除子房间。
//     成员侧收到 invite/recall 后自行切换房间（leave + join 复用既有流程）。
func (h *WebSocketHandler) handleMeetingBreakout(client *model.Client, message *model.WebSocketMessage) {
	roomID := message.Channel
	if roomID == "" {
		return
	}
	v, ok := h.activeMeetingRooms.Load(roomID)
	if !ok {
		return
	}
	meta, _ := v.(*meetingMeta)
	if meta == nil || meta.Host != client.UserID {
		h.sendError(client, 403, "仅房主可管理分组讨论")
		return
	}
	mainRoom, mainRoomOK := h.sfuManager.GetRoom(roomID)
	if !mainRoomOK || !meetingParticipantIDs(mainRoom)[client.UserID] {
		h.sendError(client, 403, "房主尚未加入主会场")
		return
	}
	var m struct {
		Action      string `json:"action"`
		Assignments []struct {
			Room    string   `json:"room"`
			Members []string `json:"members"`
		} `json:"assignments"`
	}
	if err := json.Unmarshal(message.Data, &m); err != nil {
		h.sendError(client, 400, "meeting:breakout 数据格式错误")
		return
	}
	switch m.Action {
	case "create":
		if meta.Parent != "" {
			h.sendError(client, 400, "breakout 房间内不能再分组")
			return
		}
		sent := 0
		assigned := map[string]bool{}
		mainMembers := meetingParticipantIDs(mainRoom)
		for _, a := range m.Assignments {
			if a.Room == "" || !strings.HasPrefix(a.Room, roomID) || a.Room == roomID || len(a.Members) == 0 {
				continue
			}
			members := make([]string, 0, len(a.Members))
			for _, uid := range a.Members {
				if uid == meta.Host || !mainMembers[uid] || assigned[uid] {
					continue
				}
				assigned[uid] = true
				members = append(members, uid)
			}
			if len(members) == 0 {
				continue
			}
			breakoutMembers := make(map[string]bool, len(members))
			for _, uid := range members {
				breakoutMembers[uid] = true
			}
			// 幂等登记：重复 create 同名房间不覆盖
			if _, exists := h.activeMeetingRooms.LoadOrStore(a.Room, &meetingMeta{
				Host: meta.Host,
				Parent: roomID,
				BreakoutMembers: breakoutMembers,
				minutes: meetingMinutesState{RequireConsent: true},
			}); exists {
				continue
			}
			for _, uid := range members {
				if h.wsService.SendDirectedToUser(roomID, uid, "signal:"+uid, model.MessageTypeMeetingBreakout, map[string]interface{}{
					"action": "invite", "room": a.Room, "main": roomID,
				}) {
					sent++
				}
			}
		}
		if sent == 0 {
			h.sendError(client, 400, "meeting:breakout 无有效分组")
		}
	case "recall":
		// 向每个 breakout 房间内的成员定向召回，然后拆除全部子房间
		h.activeMeetingRooms.Range(func(key, value any) bool {
			childID, _ := key.(string)
			cmeta, ok := value.(*meetingMeta)
			if !ok || cmeta.Parent != roomID {
				return true
			}
			if room, ok := h.sfuManager.GetRoom(childID); ok {
				seen := map[string]bool{}
				for _, p := range room.Participants() {
					if seen[p.ID] {
						continue
					}
					seen[p.ID] = true
					h.wsService.SendDirectedToUser(childID, p.ID, "signal:"+p.ID, model.MessageTypeMeetingBreakout, map[string]interface{}{
						"action": "recall", "room": roomID,
					})
				}
			}
			h.teardownMeeting(childID, false)
			return true
		})
	default:
		h.sendError(client, 400, "meeting:breakout 未知 action")
	}
}

// handleMeetingInvite 会议邀请（仅原始房间在线名单中的定向投递，绝不广播）：
//   - action=invite（房主上行）：校验房主身份 / 发送者与目标同属原始房间 / 会议有效，
//     生成 inviteId 并把邀请定向送达目标用户，随后给房主 sent 回执。
//   - action=accept|reject（被邀请方上行）：校验邀请归属与有效期，把结果定向回执双方。
func (h *WebSocketHandler) handleMeetingInvite(client *model.Client, message *model.WebSocketMessage) {
	var m meetingInviteMsg
	if len(message.Data) > 0 {
		if err := json.Unmarshal(message.Data, &m); err != nil {
			h.sendError(client, 400, "meeting:invite 数据格式错误")
			return
		}
	}
	switch m.Action {
	case "invite":
		h.handleMeetingInviteSend(client, message, m)
	case "accept", "reject":
		h.handleMeetingInviteRespond(client, m.Action, m.InviteID)
	default:
		h.sendError(client, 400, "meeting:invite 未知 action")
	}
}

// handleMeetingInviteSend 房主发起定向邀请：from 由服务器取自连接身份，杜绝伪造。
func (h *WebSocketHandler) handleMeetingInviteSend(client *model.Client, message *model.WebSocketMessage, m meetingInviteMsg) {
	roomID := message.Channel
	if roomID == "" {
		h.sendError(client, 400, "meeting:invite 缺少会议号")
		return
	}
	v, ok := h.activeMeetingRooms.Load(roomID)
	if !ok {
		h.sendError(client, 404, "会议不存在或已结束")
		return
	}
	meta, _ := v.(*meetingMeta)
	if meta == nil || meta.Host != client.UserID {
		h.sendError(client, 403, "仅房主可发送会议邀请")
		return
	}
	if m.To == "" || m.To == client.UserID {
		h.sendError(client, 400, "meeting:invite 目标用户无效")
		return
	}
	sourceRoom := m.SourceRoom
	if sourceRoom == "" {
		h.sendError(client, 400, "meeting:invite 缺少原始房间号")
		return
	}
	// 发送者必须当前仍在原始房间（原始房间只提供在线名单与投递通道）。
	if !h.wsService.RoomHasUserID(sourceRoom, client.UserID) {
		h.sendError(client, 403, "未连接到原始房间，无法发送邀请")
		return
	}
	// 同一（会议, 目标）已存在未过期且待处理的邀请时拒绝重复发送。
	now := time.Now()
	h.inviteMu.Lock()
	dup := false
	h.activeMeetingInvites.Range(func(_, value any) bool {
		inv, ok := value.(*meetingInvite)
		if !ok || inv.MeetingID != roomID || inv.To != m.To {
			return true
		}
		inv.mu.Lock()
		if inv.Status == "pending" && now.Before(inv.ExpiresAt) {
			dup = true
		}
		inv.mu.Unlock()
		return !dup
	})
	if dup {
		h.inviteMu.Unlock()
		h.sendError(client, 409, "该用户已有待处理的邀请")
		return
	}
	inviteID := uuid.New().String()
	expiresAt := now.Add(meetingInviteTTL)
	payload := map[string]interface{}{
		"kind":         "invite",
		"inviteId":     inviteID,
		"meetingId":    roomID,
		"sourceRoomId": sourceRoom,
		"title":        meta.Title,
		"from":         client.UserID,
		"fromName":     displayNameFromUniqID(client.UserID),
		"to":           m.To,
		"inviteUrl":    m.InviteURL,
		"createdAt":    now.UnixMilli(),
		"expiresAt":    expiresAt.UnixMilli(),
	}
	// 仅定向送达目标用户：SendDirectedToUser 要求目标在原始房间成员表内，天然校验在线与同房。
	// Reserve before delivery: the target may accept immediately after receiving
	// the frame, so storing after SendDirectedToUser would race the response.
	invite := &meetingInvite{
		InviteID:   inviteID,
		MeetingID:  roomID,
		From:       client.UserID,
		To:         m.To,
		SourceRoom: sourceRoom,
		ExpiresAt:  expiresAt,
		Status:     "pending",
	}
	h.activeMeetingInvites.Store(inviteID, invite)
	h.inviteMu.Unlock()
	if !h.wsService.SendDirectedToUser(sourceRoom, m.To, "signal:"+m.To, model.MessageTypeMeetingInvite, payload) {
		h.activeMeetingInvites.Delete(inviteID)
		h.sendError(client, 404, "目标用户不在线或不在同一房间")
		return
	}
	// 受理回执：房主据此把该目标行从「发送中」推进为「已发送/等待回应」。
	h.wsService.SendDirectedToUser(sourceRoom, client.UserID, "signal:"+client.UserID, model.MessageTypeMeetingInvite, map[string]interface{}{
		"kind": "status", "action": "sent", "inviteId": inviteID, "meetingId": roomID, "userId": m.To,
	})
	logrus.WithFields(logrus.Fields{
		"meeting":   roomID,
		"from":      client.UserID,
		"to":        m.To,
		"invite_id": inviteID,
	}).Info("会议邀请已定向发送")
}

// handleMeetingInviteRespond 被邀请方响应：校验归属、有效期与会议存续后，向双方定向回执。
func (h *WebSocketHandler) handleMeetingInviteRespond(client *model.Client, action, inviteID string) {
	if inviteID == "" {
		h.sendError(client, 400, "meeting:invite 缺少 inviteId")
		return
	}
	v, loaded := h.activeMeetingInvites.Load(inviteID)
	if !loaded {
		h.sendError(client, 404, "邀请不存在或已处理")
		return
	}
	inv, _ := v.(*meetingInvite)
	if inv == nil {
		h.sendError(client, 404, "邀请不存在或已处理")
		return
	}
	inv.mu.Lock()
	if inv.To != client.UserID {
		inv.mu.Unlock()
		h.sendError(client, 403, "无权响应此邀请")
		return
	}
	if inv.Status != "pending" {
		inv.mu.Unlock()
		h.sendError(client, 409, "邀请已处理: "+inv.Status)
		return
	}
	now := time.Now()
	if !now.Before(inv.ExpiresAt) {
		inv.Status = "expired"
		inv.mu.Unlock()
		h.notifyInviteStatus(inv, "expired")
		return
	}
	inv.Status = action // accepted / rejected
	logMeetingID, logInviteID, logTo := inv.MeetingID, inv.InviteID, inv.To
	inv.mu.Unlock()

	// 会议已结束/释放：按过期语义回执，双方显示 expired，不允许继续加入。
	if !h.hasRegistered(logMeetingID) {
		inv.mu.Lock()
		inv.Status = "expired"
		inv.mu.Unlock()
		h.notifyInviteStatus(inv, "expired")
		return
	}
	h.notifyInviteStatus(inv, action)
	logrus.WithFields(logrus.Fields{
		"meeting":   logMeetingID,
		"invite_id": logInviteID,
		"to":        logTo,
		"action":    action,
	}).Info("会议邀请已响应")
}

// notifyInviteStatus 邀请状态变化定向回执双方：被邀请方更新/关闭来电弹窗，房主更新邀请行状态。
func (h *WebSocketHandler) notifyInviteStatus(inv *meetingInvite, action string) {
	inv.mu.Lock()
	inviteID, meetingID, sourceRoom, from, to := inv.InviteID, inv.MeetingID, inv.SourceRoom, inv.From, inv.To
	inv.mu.Unlock()
	payload := map[string]interface{}{
		"kind": "status", "action": action, "inviteId": inviteID, "meetingId": meetingID, "userId": to,
	}
	// 被邀请方（expired 时其弹窗翻转为过期态；accept/reject 为其自身动作的回执）
	h.wsService.SendDirectedToUser(sourceRoom, to, "signal:"+to, model.MessageTypeMeetingInvite, payload)
	// 房主
	h.wsService.SendDirectedToUser(sourceRoom, from, "signal:"+from, model.MessageTypeMeetingInvite, payload)
}

// displayNameFromUniqID 从 uniqId（"name:uuid"）取展示名前缀；无分隔符时原样返回。
func displayNameFromUniqID(uniqID string) string {
	if i := strings.Index(uniqID, ":"); i > 0 {
		return uniqID[:i]
	}
	return uniqID
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
				"operation": "media.forward",
				"call_id":   callID,
				"seq":       seq,
				"track":     track,
				"target":    target.UserID,
				"room":      roomName,
				"error":     err.Error(),
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
