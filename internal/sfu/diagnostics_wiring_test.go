package sfu

import (
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"
)

// 接线回归测试（V0.1 归因缺口⑤）：V0a 的本质路径是
// 「真实 PC 状态跃迁 → Pion OnConnectionStateChange → p.logConnectionStateChange → logrus Info」。
// 此前删掉 room.go / participant.go 里那行 p.logConnectionStateChange(pc, state)，
// 全部测试依然绿（假绿敞口）。本文件用真实 PeerConnection 走完整握手/关闭，
// 配合真实 logrus hook（installCaptureHook，级别钉在 Info 与生产一致）断言
// Info 级日志确实产出且带 prev_state / prev_state_duration_ms 因果字段。

// waitForLogEntry 轮询等待满足谓词的日志条目。
// Pion 的连接态 handler 经 go handler(cs) 异步触发，必须等待而非立即断言。
func waitForLogEntry(t *testing.T, hook *captureHook, match func(capturedEntry) bool) capturedEntry {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, entry := range hook.snapshot() {
			if match(entry) {
				return entry
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("超时：未等到期望的日志条目，已捕获 %#v", hook.snapshot())
	return capturedEntry{}
}

// assertCausalTransitionFields 断言一条状态跃迁日志带齐因果字段（V0a 不变量）。
// prev_state_duration_ms 不仅要是 int64，还必须非负（值级断言）。
func assertCausalTransitionFields(t *testing.T, entry capturedEntry) {
	t.Helper()
	if entry.level != log.InfoLevel {
		t.Fatalf("状态跃迁日志级别为 %v，期望 Info: %#v", entry.level, entry)
	}
	if _, ok := entry.fields["prev_state"].(string); !ok {
		t.Fatalf("日志缺少 prev_state 字段: %#v", entry.fields)
	}
	duration, ok := entry.fields["prev_state_duration_ms"].(int64)
	if !ok {
		t.Fatalf("日志缺少 int64 型 prev_state_duration_ms 字段: %#v", entry.fields)
	}
	if duration < 0 {
		t.Fatalf("prev_state_duration_ms 为负值 %d: %#v", duration, entry.fields)
	}
}

// icePeer 抽象 Participant / Subscriber 共有的 ICE 候选收发面，供握手 helper 复用。
type icePeer interface {
	AddICECandidate(candidate webrtc.ICECandidateInit) error
	OnICECandidate(callback func(*webrtc.ICECandidate))
}

// exchangeICECandidates 双向转发 ICE 候选；加锁避免两个回调并发 AddICECandidate 交错。
func exchangeICECandidates(t *testing.T, remote icePeer, client *webrtc.PeerConnection) {
	t.Helper()
	var mux sync.Mutex
	remote.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		mux.Lock()
		defer mux.Unlock()
		if err := client.AddICECandidate(c.ToJSON()); err != nil {
			t.Errorf("client.AddICECandidate 失败: %v", err)
		}
	})
	client.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		mux.Lock()
		defer mux.Unlock()
		if err := remote.AddICECandidate(c.ToJSON()); err != nil {
			t.Errorf("remote.AddICECandidate 失败: %v", err)
		}
	})
}

// dialPair 让一个带 opus 音频 track 的真实客户端 PC 与服务端 Participant 完成
// 真实握手到 connected：建 PC → AddTrack → offer/answer 交换 → 双向 ICE → waitConnected。
// 供主连接与订阅发布端两条接线测试共用；任何一步失败立即 t.Fatalf（不吞错、不降级）。
func dialPair(t *testing.T, api *API, part *Participant, trackID, streamID string) (*webrtc.PeerConnection, *webrtc.TrackLocalStaticRTP) {
	t.Helper()
	client := newOfflineClientPC(t, api)
	opusCap := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	track, err := webrtc.NewTrackLocalStaticRTP(opusCap, trackID, streamID)
	if err != nil {
		t.Fatalf("创建本地音频 track 失败: %v", err)
	}
	if _, err := client.AddTrack(track); err != nil {
		t.Fatalf("AddTrack 失败: %v", err)
	}
	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatalf("客户端 CreateOffer 失败: %v", err)
	}
	if err := client.SetLocalDescription(offer); err != nil {
		t.Fatalf("客户端 SetLocalDescription 失败: %v", err)
	}
	answer, err := part.Offer(offer)
	if err != nil {
		t.Fatalf("Participant.Offer 失败: %v", err)
	}
	if err := client.SetRemoteDescription(answer); err != nil {
		t.Fatalf("客户端 SetRemoteDescription 失败: %v", err)
	}
	exchangeICECandidates(t, part, client)
	waitConnected(t, part.ConnectionState)
	return client, track
}

// dialSubscriberPair 完成订阅侧真实握手：服务端 sub.Offer → 客户端 answer →
// 双向 ICE → waitConnected（等客户端侧 connected，与原始测试同侧）。
func dialSubscriberPair(t *testing.T, api *API, sub *Subscriber) *webrtc.PeerConnection {
	t.Helper()
	client := newOfflineClientPC(t, api)
	if err := client.SetRemoteDescription(sub.Offer()); err != nil {
		t.Fatalf("订阅端 SetRemoteDescription 失败: %v", err)
	}
	subAnswer, err := client.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("订阅端 CreateAnswer 失败: %v", err)
	}
	if err := client.SetLocalDescription(subAnswer); err != nil {
		t.Fatalf("订阅端 SetLocalDescription 失败: %v", err)
	}
	if err := sub.SetRemoteDescription(subAnswer); err != nil {
		t.Fatalf("Subscriber.SetRemoteDescription 失败: %v", err)
	}
	exchangeICECandidates(t, sub, client)
	waitConnected(t, client.ConnectionState)
	return client
}

// publishOpusTrack 让真实客户端 PC 发布一条 opus track、注入 5 个 RTP 包，
// 等服务端 OnTrack 登记完成（订阅测试的发布端前置条件）。
func publishOpusTrack(t *testing.T, api *API, pub *Participant, trackID, streamID string, ssrc uint32) {
	t.Helper()
	_, track := dialPair(t, api, pub, trackID, streamID)
	for seq := uint16(0); seq < 5; seq++ {
		if err := track.WriteRTP(makeOpusPacket(seq, ssrc)); err != nil {
			t.Fatalf("注入 RTP 失败: %v", err)
		}
	}
	waitForCount(t, pub.PublishedTracks, 1)
}

// TestMainConnectionTransitionReachesLogrusWithCausalFields 保护 room.go 里
// p.logConnectionStateChange(pc, state) 这行接线：删除它本测试必须变红。
// 不变量：主连接的真实状态跃迁必须经由 logrus 产出 Info 日志，
// 且带 prev_state / prev_state_duration_ms 因果字段。
func TestMainConnectionTransitionReachesLogrusWithCausalFields(t *testing.T) {
	hook := installCaptureHook(t)
	mgr := MustNewManager(OfflineSettingEngine())
	room := mgr.JoinRoom("wiring-room")
	part, err := room.AddParticipant("wiring-pub")
	if err != nil {
		t.Fatalf("AddParticipant 失败: %v", err)
	}
	dialPair(t, mgr.API(), part, "aud-wiring", "stream-wiring")

	connected := waitForLogEntry(t, hook, func(e capturedEntry) bool {
		return e.message == "参与者的主连接状态变化" &&
			e.fields["state"] == "connected" && e.fields["participantID"] == "wiring-pub"
	})
	assertCausalTransitionFields(t, connected)
	if connected.fields["roomID"] != "wiring-room" {
		t.Fatalf("roomID=%v，期望 wiring-room: %#v", connected.fields["roomID"], connected.fields)
	}

	// 真实 Closed 跃迁（拆除参与者）同样必须经由同一接线落日志。
	if err := room.RemoveParticipant("wiring-pub"); err != nil {
		t.Fatalf("RemoveParticipant 失败: %v", err)
	}
	closed := waitForLogEntry(t, hook, func(e capturedEntry) bool {
		return e.message == "参与者的主连接状态变化" &&
			e.fields["state"] == "closed" && e.fields["participantID"] == "wiring-pub"
	})
	assertCausalTransitionFields(t, closed)
}

// TestBindPeerConnectionTransitionReachesLogrusWithCausalFields 保护
// participant.go bindPeerConnection 里那行 p.logConnectionStateChange(pc, state)
// （resetFailedInitialOffer 替换主 PC 后走的就是这条绑定路径）。
func TestBindPeerConnectionTransitionReachesLogrusWithCausalFields(t *testing.T) {
	hook := installCaptureHook(t)
	mgr := MustNewManager(OfflineSettingEngine())
	room := mgr.JoinRoom("wiring-bind-room")
	part, err := room.AddParticipant("wiring-bind")
	if err != nil {
		t.Fatalf("AddParticipant 失败: %v", err)
	}

	pc, err := mgr.API().newPeerConnection()
	if err != nil {
		t.Fatalf("创建 PeerConnection 失败: %v", err)
	}
	part.bindPeerConnection(pc)
	if err := pc.Close(); err != nil {
		t.Fatalf("关闭 PeerConnection 失败: %v", err)
	}

	entry := waitForLogEntry(t, hook, func(e capturedEntry) bool {
		return e.message == "参与者的主连接状态变化" &&
			e.fields["state"] == "closed" && e.fields["participantID"] == "wiring-bind"
	})
	assertCausalTransitionFields(t, entry)
}

// TestSubscriberDisconnectReasonValues 值级断言订阅侧 disconnected / failed 跃迁日志的
// reason 字段（"pc-disconnected" / "pc-failed"）——此前只测 connected/closed 的存在性。
// 构造方式（两次实证后确定）：断连态在进程内不可确定性构造——对端优雅 Close()
// 会让订阅 PC 直接 closed（DTLS close_notify，不经 disconnected/failed）；订阅 PC
// 无客户端应答则停在 new 零跃迁。故用真实订阅 PC 直接驱动同一跃迁函数的
// disconnected/failed 分支，断言产出日志的 reason 值。
func TestSubscriberDisconnectReasonValues(t *testing.T) {
	hook := installCaptureHook(t)
	mgr := MustNewManager(OfflineSettingEngine())
	room := mgr.JoinRoom("wiring-reason-room")
	pub, err := room.AddParticipant("reason-pub")
	if err != nil {
		t.Fatalf("AddParticipant(pub) 失败: %v", err)
	}
	subPart, err := room.AddParticipant("reason-sub")
	if err != nil {
		t.Fatalf("AddParticipant(sub) 失败: %v", err)
	}
	publishOpusTrack(t, mgr.API(), pub, "aud-reason", "stream-reason", 0xA3)

	sub, _, err := subPart.SubscribeTo("reason-pub")
	if err != nil {
		t.Fatalf("SubscribeTo 失败: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	sub.logConnectionStateChange(sub.pc, webrtc.PeerConnectionStateDisconnected)
	disconnected := waitForLogEntry(t, hook, func(e capturedEntry) bool {
		return e.message == "参与者的订阅连接状态变化" &&
			e.fields["state"] == "disconnected" && e.fields["publisherID"] == "reason-pub"
	})
	assertCausalTransitionFields(t, disconnected)
	if disconnected.fields["reason"] != "pc-disconnected" {
		t.Fatalf("reason=%v，期望 pc-disconnected: %#v", disconnected.fields["reason"], disconnected.fields)
	}

	sub.logConnectionStateChange(sub.pc, webrtc.PeerConnectionStateFailed)
	failed := waitForLogEntry(t, hook, func(e capturedEntry) bool {
		return e.message == "参与者的订阅连接状态变化" &&
			e.fields["state"] == "failed" && e.fields["publisherID"] == "reason-pub"
	})
	assertCausalTransitionFields(t, failed)
	if failed.fields["reason"] != "pc-failed" {
		t.Fatalf("reason=%v，期望 pc-failed: %#v", failed.fields["reason"], failed.fields)
	}
}

// TestSubscriberConnectionTransitionReachesLogrusWithPublisherID 保护
// buildSubscriber 里订阅 PC 的 OnConnectionStateChange 接线（订阅侧观测，加分项）。
// 不变量：订阅侧真实跃迁必须落 Info 日志，且带 publisherID 以区分同
// 一订阅者的多条订阅 PC。
func TestSubscriberConnectionTransitionReachesLogrusWithPublisherID(t *testing.T) {
	hook := installCaptureHook(t)
	mgr := MustNewManager(OfflineSettingEngine())
	room := mgr.JoinRoom("wiring-sub-room")
	pub, err := room.AddParticipant("wiring-pubA")
	if err != nil {
		t.Fatalf("AddParticipant(pubA) 失败: %v", err)
	}
	subPart, err := room.AddParticipant("wiring-subB")
	if err != nil {
		t.Fatalf("AddParticipant(subB) 失败: %v", err)
	}

	// 发布端：真实客户端 PC + 真实 RTP，触发服务端 OnTrack 登记。
	publishOpusTrack(t, mgr.API(), pub, "aud-sub", "stream-sub", 0xA2)

	// 订阅端：订阅 PC 建立并完成真实握手。
	sub, _, err := subPart.SubscribeTo("wiring-pubA")
	if err != nil {
		t.Fatalf("SubscribeTo 失败: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	dialSubscriberPair(t, mgr.API(), sub)

	connected := waitForLogEntry(t, hook, func(e capturedEntry) bool {
		return e.message == "参与者的订阅连接状态变化" &&
			e.fields["state"] == "connected" && e.fields["publisherID"] == "wiring-pubA"
	})
	assertCausalTransitionFields(t, connected)
	if connected.fields["participantID"] != "wiring-subB" {
		t.Fatalf("participantID=%v，期望 wiring-subB: %#v", connected.fields["participantID"], connected.fields)
	}

	// 订阅 PC 关闭（真实 Closed 跃迁）同样必须落日志。
	if err := sub.Close(); err != nil {
		t.Fatalf("关闭订阅失败: %v", err)
	}
	closed := waitForLogEntry(t, hook, func(e capturedEntry) bool {
		return e.message == "参与者的订阅连接状态变化" &&
			e.fields["state"] == "closed" && e.fields["publisherID"] == "wiring-pubA"
	})
	assertCausalTransitionFields(t, closed)
}
