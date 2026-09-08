package handler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"letshare-server/internal/model"
	"letshare-server/internal/sfu"
)

// 会议核心 P0 回归（WF-003/004）：
//  1. 发布者尚未 ready（未登记 track）时订阅 → 服务器返回 meeting:sdp 前缀的局部错误，
//     客户端可据此分类为「可重试、不动全局 stage」；
//  2. 发布者迟发布首个 track → 服务器必须为「已 join 但尚未成功订阅」的成员
//     自动建立订阅并定向推送订阅 offer —— 订阅方无需再发请求即可等到媒体
//     （修复「publisher 尚未 ready 时订阅方应等待或重试」的服务器侧缺口：
//     旧实现只向已有订阅者扇出重协商，重试窗口耗尽的成员永远收不到迟发布者）。
func TestMeetingLatePublisherAutoSubscribe(t *testing.T) {
	ts := newMeetingTestServer(t)
	defer ts.Close()

	offAPI, err := sfu.NewAPI(sfu.OfflineSettingEngine())
	if err != nil {
		t.Fatalf("初始化离线 API 失败: %v", err)
	}

	const userA = "uniqPubLate"
	const userB = "uniqSubEarly"

	a := newWSRPC(t, ts.srv, userA)
	b := newWSRPC(t, ts.srv, userB)

	if err := a.sendJSON(model.WebSocketMessage{Type: "meeting:create"}); err != nil {
		t.Fatalf("发送 meeting:create 失败: %v", err)
	}
	room, err := a.waitCreate(5 * time.Second)
	if err != nil {
		t.Fatalf("等待 meeting:create 失败: %v", err)
	}

	for _, r := range []*wsRPC{a, b} {
		if err := r.sendJSON(model.WebSocketMessage{Type: "subscribe", Channel: room, Event: "signal:all"}); err != nil {
			t.Fatal(err)
		}
		if err := r.waitSubscribed(5 * time.Second); err != nil {
			t.Fatal(err)
		}
	}
	join := func(r *wsRPC) {
		data, _ := json.Marshal(map[string]interface{}{"roomId": room})
		if err := r.sendJSON(model.WebSocketMessage{Type: "meeting:join", Channel: room, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	join(a)
	join(b)
	time.Sleep(300 * time.Millisecond)

	// ---- 1) B 抢先订阅尚未发布的 A → meeting:sdp 前缀的局部可重试错误 ----
	subEarly, _ := json.Marshal(map[string]interface{}{"type": "offer", "to": userA})
	if err := b.sendJSON(model.WebSocketMessage{Type: "meeting:sdp", Channel: room, Data: subEarly}); err != nil {
		t.Fatal(err)
	}
	errMsg, err := b.waitError(5 * time.Second)
	if err != nil {
		t.Fatalf("等待订阅错误帧: %v", err)
	}
	if !strings.HasPrefix(errMsg, "meeting:sdp") {
		t.Fatalf("订阅失败错误必须带 meeting:sdp 前缀（客户端局部错误分类依据），实际 %q", errMsg)
	}
	t.Logf("publisher 未 ready 时的订阅错误（局部可重试）: %s", errMsg)

	// ---- A 迟发布 ----
	pubA, err := offAPI.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer pubA.Close()
	trA := makeOpusTrack(t, offAPI, "aud-late", "stream-late")
	if _, err := pubA.AddTrack(trA); err != nil {
		t.Fatal(err)
	}
	a.pubPC = pubA

	offerA, err := pubA.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pubA.SetLocalDescription(offerA); err != nil {
		t.Fatal(err)
	}
	pubA.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		data, _ := json.Marshal(map[string]interface{}{"candidate": c.ToJSON()})
		_ = a.sendJSON(model.WebSocketMessage{Type: "meeting:ice", Channel: room, Data: data})
	})
	pubData, _ := json.Marshal(map[string]interface{}{"type": "offer", "sdp": offerA.SDP})
	if err := a.sendJSON(model.WebSocketMessage{Type: "meeting:sdp", Channel: room, Data: pubData}); err != nil {
		t.Fatal(err)
	}
	ansMsg, err := a.waitSDP(func(m model.WebSocketMessage) bool {
		var d map[string]interface{}
		_ = json.Unmarshal(m.Data, &d)
		return d["type"] == "answer" && d["sdp"] != ""
	}, 8*time.Second)
	if err != nil {
		t.Fatalf("等待 A 的发布 answer: %v", err)
	}
	var ansD map[string]interface{}
	_ = json.Unmarshal(ansMsg.Data, &ansD)
	if err := pubA.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ansD["sdp"].(string)}); err != nil {
		t.Fatal(err)
	}
	waitPCConnected(t, pubA)
	// 注入 RTP 触发服务器 OnTrack → 应为 B 自动建订阅并推送 offer
	for seq := uint16(0); seq < 6; seq++ {
		_ = trA.WriteRTP(makeOpusRTPPacket(seq, 0xE1))
	}

	// ---- B 无需重新请求，应自动收到该发布者的订阅 offer ----
	subOfferMsg, err := b.waitSDP(func(m model.WebSocketMessage) bool {
		var d map[string]interface{}
		_ = json.Unmarshal(m.Data, &d)
		return d["type"] == "offer" && d["to"] == userA && d["sdp"] != ""
	}, 10*time.Second)
	if err != nil {
		t.Fatalf("迟发布者首个 track 应自动为未订阅成员推送订阅 offer: %v", err)
	}
	var subOfferD map[string]interface{}
	_ = json.Unmarshal(subOfferMsg.Data, &subOfferD)

	// ---- B 应答并接收媒体（auto-subscribe 闭环） ----
	recvB, err := offAPI.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer recvB.Close()
	b.subPC = recvB
	received := make(chan struct{}, 8)
	recvB.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			for {
				if _, _, err := track.ReadRTP(); err != nil {
					return
				}
				select {
				case received <- struct{}{}:
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
	if err := recvB.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: subOfferD["sdp"].(string)}); err != nil {
		t.Fatal(err)
	}
	subAnswer, err := recvB.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := recvB.SetLocalDescription(subAnswer); err != nil {
		t.Fatal(err)
	}
	ansData, _ := json.Marshal(map[string]interface{}{"type": "answer", "to": userA, "sdp": subAnswer.SDP})
	if err := b.sendJSON(model.WebSocketMessage{Type: "meeting:sdp", Channel: room, Data: ansData}); err != nil {
		t.Fatal(err)
	}
	waitPCConnected(t, recvB)

	for seq := uint16(100); seq < 140; seq++ {
		_ = trA.WriteRTP(makeOpusRTPPacket(seq, 0xA1))
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case <-received:
			t.Log("B 经自动订阅收到迟发布者的转发媒体")
			return
		default:
			if time.Now().After(deadline) {
				t.Fatal("超时：B 未收到迟发布者的转发媒体")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}
