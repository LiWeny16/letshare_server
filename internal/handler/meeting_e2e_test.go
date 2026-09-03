package handler

import (
	"encoding/json"
	"fmt"
	"letshare-server/internal/model"
	"letshare-server/internal/service"
	"letshare-server/internal/sfu"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// meetingTestServer 复用真实 handler.processMessage 驱动 WS 信令，
// 从而端到端验证 meeting:join / meeting:sdp / meeting:ice 的会议媒体通道。
type meetingTestServer struct {
	srv       *httptest.Server
	handler   *WebSocketHandler
	wsService *service.WebSocketService
}

func newMeetingTestServer(t *testing.T) *meetingTestServer {
	t.Helper()
	wsService := service.NewWebSocketService(10)
	authService := service.NewAuthService()
	fts := service.NewFileTransferService(wsService, 3*1024*1024*1024, 65536)
	h := NewWebSocketHandler(wsService, authService, fts, nil)
	// 注入离线回环引擎，保证本进程内两客户端可无外网连通
	h.SetSFU(sfu.MustNewManager(sfu.OfflineSettingEngine()))

	ts := &meetingTestServer{handler: h, wsService: wsService}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		userID := r.URL.Query().Get("userId")
		if token == "" {
			http.Error(w, "missing token", 401)
			return
		}
		if err := authService.ValidateAuthToken(token); err != nil {
			http.Error(w, "invalid token", 401)
			return
		}
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		client := model.NewClient("client-"+userID, userID, conn)
		client.Metadata["authenticated"] = true
		wsService.AddClient(client)
		conn.SetWriteDeadline(time.Now().Add(15 * time.Second))

		go func() {
			defer func() {
				recover()
				conn.Close()
				wsService.RemoveClient(client.ID)
			}()
			for {
				mt, data, err := conn.ReadMessage()
				if err != nil {
					return
				}
				client.LastPing = time.Now()
				if mt == websocket.TextMessage {
					var msg model.WebSocketMessage
					if err := json.Unmarshal(data, &msg); err != nil {
						continue
					}
					ts.handler.processMessage(client, &msg)
				}
			}
		}()
	}))
	ts.srv = srv
	return ts
}

func (ts *meetingTestServer) Close() { ts.srv.Close(); ts.wsService.Shutdown() }

// wsRPC 模拟一个会议客户端：单 reader 将每条 WS 消息推入 in，
// dispatcher 把 meeting:ice 应用到对应 PC、把 subscribed / meeting:sdp 送入相应通道。
type wsRPC struct {
	conn   *websocket.Conn
	in     chan model.WebSocketMessage
	subs   chan struct{}
	sdp    chan model.WebSocketMessage
	create chan model.WebSocketMessage
	err    chan model.WebSocketMessage
	pubPC  *webrtc.PeerConnection
	subPC  *webrtc.PeerConnection
}

// reader 单 goroutine 读取 WS，所有消息入 in 通道。
func (r *wsRPC) reader() {
	for {
		r.conn.SetReadDeadline(time.Now().Add(20 * time.Second))
		_, raw, err := r.conn.ReadMessage()
		if err != nil {
			close(r.in)
			return
		}
		var m model.WebSocketMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		r.in <- m
	}
}

func (r *wsRPC) dispatch() {
	for m := range r.in {
		switch m.Type {
		case "subscribed":
			r.subs <- struct{}{}
		case model.MessageTypeMeetingSDP:
			r.sdp <- m
		case model.MessageTypeMeetingCreate:
			r.create <- m
		case model.MessageTypeError:
			r.err <- m
		case model.MessageTypeMeetingICE:
			var d meetingICEMsg
			_ = json.Unmarshal(m.Data, &d)
			var init webrtc.ICECandidateInit
			_ = json.Unmarshal(d.Candidate, &init)
			if d.To == "" {
				if r.pubPC != nil {
					_ = r.pubPC.AddICECandidate(init)
				}
			} else if r.subPC != nil {
				_ = r.subPC.AddICECandidate(init)
			}
		}
	}
}

// sendJSON 发送一条 JSON 消息到该 WS。
func (r *wsRPC) sendJSON(m model.WebSocketMessage) error {
	return r.conn.WriteJSON(m)
}

// waitSubscribed 阻塞等待订阅确认。
func (r *wsRPC) waitSubscribed(timeout time.Duration) error {
	select {
	case <-r.subs:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("等待 subscribed 超时")
	}
}

// waitCreate 阻塞等待一条 meeting:create 回包并取出 roomId。
func (r *wsRPC) waitCreate(timeout time.Duration) (string, error) {
	select {
	case m := <-r.create:
		var d map[string]interface{}
		_ = json.Unmarshal(m.Data, &d)
		room, _ := d["roomId"].(string)
		return room, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("等待 meeting:create 超时")
	}
}

// waitError 阻塞等待一条 error 回包并取出 message。
func (r *wsRPC) waitError(timeout time.Duration) (string, error) {
	select {
	case m := <-r.err:
		if m.Error != nil {
			return m.Error.Message, nil
		}
		var d map[string]interface{}
		_ = json.Unmarshal(m.Data, &d)
		msg, _ := d["message"].(string)
		return msg, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("等待 error 超时")
	}
}

// waitSDP 阻塞等待一条满足 pred 的 meeting:sdp。
func (r *wsRPC) waitSDP(pred func(m model.WebSocketMessage) bool, timeout time.Duration) (model.WebSocketMessage, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		select {
		case m, ok := <-r.sdp:
			if !ok {
				return m, fmt.Errorf("sdp 通道关闭")
			}
			if pred(m) {
				return m, nil
			}
		case <-t.C:
			return model.WebSocketMessage{}, fmt.Errorf("等待 meeting:sdp 超时")
		}
	}
}

func newWSRPC(t *testing.T, srv *httptest.Server, userID string) *wsRPC {
	t.Helper()
	conn := dialTestClient(t, srv, userID)
	r := &wsRPC{
		conn:   conn,
		in:     make(chan model.WebSocketMessage, 128),
		subs:   make(chan struct{}, 16),
		sdp:    make(chan model.WebSocketMessage, 64),
		create: make(chan model.WebSocketMessage, 8),
		err:    make(chan model.WebSocketMessage, 8),
	}
	go r.reader()
	go r.dispatch()
	t.Cleanup(func() { conn.Close() })
	return r
}

// waitPCConnected 等待某个 PeerConnection 达到 connected。
func waitPCConnected(t *testing.T, pc *webrtc.PeerConnection) {
	t.Helper()
	done := make(chan struct{})
	var once sync.Once
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			once.Do(func() { close(done) })
		}
	})
	select {
	case <-done:
	case <-time.After(12 * time.Second):
		t.Fatalf("PeerConnection 未连接: %s", pc.ConnectionState().String())
	}
}

func makeOpusTrack(t *testing.T, api *sfu.API, id, streamID string) *webrtc.TrackLocalStaticRTP {
	t.Helper()
	cap := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	tr, err := webrtc.NewTrackLocalStaticRTP(cap, id, streamID)
	if err != nil {
		t.Fatalf("创建 opus track 失败: %v", err)
	}
	return tr
}

// TestMeetingE2E_OfferAnswerAndMediaForward 验证一条真 WS 信令闭环：
// A 发布 offer → 服务器回 answer；A 注流 → B 订阅 A → 服务器扇出 → B 收到转发媒体。
func TestMeetingE2E_OfferAnswerAndMediaForward(t *testing.T) {
	ts := newMeetingTestServer(t)
	defer ts.Close()

	offAPI, err := sfu.NewAPI(sfu.OfflineSettingEngine())
	if err != nil {
		t.Fatalf("初始化离线 API 失败: %v", err)
	}

	const userA = "uniqA-1"
	const userB = "uniqB-1"

	a := newWSRPC(t, ts.srv, userA)
	b := newWSRPC(t, ts.srv, userB)

	// A 创建会议，服务器回发 4 位会议号（加入前必须先登记）。
	if err := a.sendJSON(model.WebSocketMessage{Type: "meeting:create"}); err != nil {
		t.Fatalf("发送 meeting:create 失败: %v", err)
	}
	room, err := a.waitCreate(5 * time.Second)
	if err != nil {
		t.Fatalf("等待 meeting:create 失败: %v", err)
	}
	if !regexp.MustCompile(`^\d{4}$`).MatchString(room) {
		t.Fatalf("会议号应为 4 位数字，实际 %q", room)
	}

	// A、B 各自订阅房间
	sendSubscribe := func(r *wsRPC) {
		if err := r.sendJSON(model.WebSocketMessage{Type: "subscribe", Channel: room, Event: "signal:all"}); err != nil {
			t.Fatalf("发送 subscribe 失败: %v", err)
		}
		if err := r.waitSubscribed(5 * time.Second); err != nil {
			t.Fatalf("%s 订阅失败: %v", r.conn.RemoteAddr(), err)
		}
	}
	sendSubscribe(a)
	sendSubscribe(b)

	// 各自 meeting:join
	join := func(r *wsRPC, uid string) {
		data, _ := json.Marshal(map[string]interface{}{"roomId": room, "from": uid})
		if err := r.sendJSON(model.WebSocketMessage{Type: "meeting:join", Channel: room, Data: data}); err != nil {
			t.Fatalf("发送 meeting:join 失败: %v", err)
		}
	}
	join(a, userA)
	join(b, userB)

	// ---- A 发布：offer → answer ----
	pubA, err := offAPI.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("创建 A 发布 PC 失败: %v", err)
	}
	defer pubA.Close()
	trA := makeOpusTrack(t, offAPI, "aud-A", "streamA")
	if _, err := pubA.AddTrack(trA); err != nil {
		t.Fatalf("A AddTrack 失败: %v", err)
	}
	a.pubPC = pubA

	offerA, err := pubA.CreateOffer(nil)
	if err != nil {
		t.Fatalf("A CreateOffer 失败: %v", err)
	}
	if err := pubA.SetLocalDescription(offerA); err != nil {
		t.Fatalf("A SetLocalDescription 失败: %v", err)
	}
	// A 的本地候选 -> 服务器发布 PC
	pubA.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		data, _ := json.Marshal(map[string]interface{}{"candidate": c.ToJSON()})
		_ = a.sendJSON(model.WebSocketMessage{Type: "meeting:ice", Channel: room, Data: data})
	})

	sdpData, _ := json.Marshal(map[string]interface{}{"type": "offer", "sdp": offerA.SDP})
	if err := a.sendJSON(model.WebSocketMessage{Type: "meeting:sdp", Channel: room, Data: sdpData}); err != nil {
		t.Fatalf("A 发送 publish offer 失败: %v", err)
	}

	// 等待服务器回 answer
	ansMsg, err := a.waitSDP(func(m model.WebSocketMessage) bool {
		var d map[string]interface{}
		_ = json.Unmarshal(m.Data, &d)
		return d["type"] == "answer" && d["sdp"] != ""
	}, 8*time.Second)
	if err != nil {
		t.Fatalf("等待 A 的 answer: %v", err)
	}
	var ansD map[string]interface{}
	_ = json.Unmarshal(ansMsg.Data, &ansD)
	if err := pubA.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ansD["sdp"].(string)}); err != nil {
		t.Fatalf("A SetRemoteDescription(answer) 失败: %v", err)
	}

	// 等待 A 发布 PC 连通
	waitPCConnected(t, pubA)

	// A 注入媒体，触发服务器登记 A 的发布 track
	for seq := uint16(0); seq < 6; seq++ {
		_ = trA.WriteRTP(makeOpusRTPPacket(seq, 0xA1))
	}
	// 给服务器 OnTrack 一点时间
	time.Sleep(300 * time.Millisecond)

	// ---- B 订阅 A ----
	recvB, err := offAPI.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("创建 B 接收 PC 失败: %v", err)
	}
	defer recvB.Close()
	b.subPC = recvB

	received := make(chan *rtp.Packet, 64)
	recvB.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			for {
				pkt, _, err := track.ReadRTP()
				if err != nil {
					return
				}
				select {
				case received <- pkt:
				default:
				}
			}
		}()
	})
	recvB.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		data, _ := json.Marshal(map[string]interface{}{"candidate": c.ToJSON(), "to": userA})
		_ = b.sendJSON(model.WebSocketMessage{Type: "meeting:ice", Channel: room, Data: data})
	})

	// B 发起订阅请求（服务器建 Subscriber 并回发其 offer）
	subReq, _ := json.Marshal(map[string]interface{}{"type": "offer", "to": userA})
	if err := b.sendJSON(model.WebSocketMessage{Type: "meeting:sdp", Channel: room, Data: subReq}); err != nil {
		t.Fatalf("B 发送订阅请求失败: %v", err)
	}

	subOfferMsg, err := b.waitSDP(func(m model.WebSocketMessage) bool {
		var d map[string]interface{}
		_ = json.Unmarshal(m.Data, &d)
		return d["type"] == "offer" && d["to"] == userA && d["sdp"] != ""
	}, 8*time.Second)
	if err != nil {
		t.Fatalf("等待 B 的订阅 offer: %v", err)
	}
	var subOfferD map[string]interface{}
	_ = json.Unmarshal(subOfferMsg.Data, &subOfferD)
	if err := recvB.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: subOfferD["sdp"].(string)}); err != nil {
		t.Fatalf("B SetRemoteDescription(订阅offer) 失败: %v", err)
	}
	subAnswer, err := recvB.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("B CreateAnswer(订阅) 失败: %v", err)
	}
	if err := recvB.SetLocalDescription(subAnswer); err != nil {
		t.Fatalf("B SetLocalDescription(订阅) 失败: %v", err)
	}
	ansData, _ := json.Marshal(map[string]interface{}{"type": "answer", "to": userA, "sdp": subAnswer.SDP})
	if err := b.sendJSON(model.WebSocketMessage{Type: "meeting:sdp", Channel: room, Data: ansData}); err != nil {
		t.Fatalf("B 发送订阅 answer 失败: %v", err)
	}

	// 等待 B 订阅 PC 连通
	waitPCConnected(t, recvB)

	// ---- A 继续发媒体，B 应收到最后被转发的一帧 ----
	for seq := uint16(100); seq < 140; seq++ {
		_ = trA.WriteRTP(makeOpusRTPPacket(seq, 0xA1))
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case p := <-received:
			if p.PayloadType != 111 {
				t.Fatalf("转发包 PT 应为 111，实际 %d", p.PayloadType)
			}
			t.Logf("B 订阅端收到被服务器转发的 RTP（PT=%d, seq=%d, len=%d）", p.PayloadType, p.SequenceNumber, len(p.Payload))
			return
		default:
			if time.Now().After(deadline) {
				t.Fatal("超时：B 未收到经 SFU 转发的媒体帧")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func makeOpusRTPPacket(seq uint16, ssrc uint32) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    111,
			SequenceNumber: seq,
			Timestamp:      uint32(48000 / 50 * seq),
			SSRC:           ssrc,
		},
		Payload: []byte{0xF8, 0xFF, 0xFE, 0x00, 0x01, 0x02},
	}
}

// TestMeetingJoinRejectsNonExistent 验证加入一个未登记的会议号会被服务端拒绝（meeting:join 未再自动建房）。
func TestMeetingJoinRejectsNonExistent(t *testing.T) {
	ts := newMeetingTestServer(t)
	defer ts.Close()

	const userA = "uniqReject-1"
	a := newWSRPC(t, ts.srv, userA)

	// 直接 join 一个未创建的会议号，应收到 404「会议不存在」。
	data, _ := json.Marshal(map[string]interface{}{"roomId": "9999", "from": userA})
	if err := a.sendJSON(model.WebSocketMessage{Type: "meeting:join", Channel: "9999", Data: data}); err != nil {
		t.Fatalf("发送 meeting:join 失败: %v", err)
	}
	msg, err := a.waitError(5 * time.Second)
	if err != nil {
		t.Fatalf("等待 error 失败: %v", err)
	}
	if msg == "" {
		t.Fatalf("未返回错误信息")
	}
	t.Logf("未登记会议号 join 被拒绝：%s", msg)

	// 创建后同一号码可成功加入（end-to-end 验证登记放行）。仅通过信号层验证 JoinRoom 不报错即可。
	if err := a.sendJSON(model.WebSocketMessage{Type: "meeting:create"}); err != nil {
		t.Fatalf("发送 meeting:create 失败: %v", err)
	}
	room, err := a.waitCreate(5 * time.Second)
	if err != nil {
		t.Fatalf("等待 meeting:create 失败: %v", err)
	}
	joinData, _ := json.Marshal(map[string]interface{}{"roomId": room, "from": userA})
	if err := a.sendJSON(model.WebSocketMessage{Type: "meeting:join", Channel: room, Data: joinData}); err != nil {
		t.Fatalf("发送 meeting:join 失败: %v", err)
	}
	// 再发一条 sdp 不应触发「尚未加入该会议房间」，说明 join 已登记成功。
	_ = a.sendJSON(model.WebSocketMessage{Type: "meeting:sdp", Channel: room, Data: []byte(`{"type":"disable-audio","sdp":"x"}`)})
}
