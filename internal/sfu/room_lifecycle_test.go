package sfu

import (
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// TestRoomRemovesClosedParticipantFromMap 验证参与者自关闭（发布 PC failed/closed）
// 后必须从房间 map 摘除：GetParticipant 不得再返回已关闭的参与者，
// 否则后续重连/订阅会复用 closed participant（线上「参与者已关闭」「暂无已发布 track」
// 永久报错的根因），Count 也会虚高导致空房回收误判。
func TestRoomRemovesClosedParticipantFromMap(t *testing.T) {
	mgr := MustNewManager(OfflineSettingEngine())
	room := mgr.JoinRoom("closed-part-room")

	partA, err := room.AddParticipant("pubA")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := room.AddParticipant("subB"); err != nil {
		t.Fatal(err)
	}
	if room.Count() != 2 {
		t.Fatalf("初始参与者数应为 2，实际 %d", room.Count())
	}

	// 参与者自身 PC 失败触发自关闭（与 OnConnectionStateChange -> p.Close() 同路径）
	if err := partA.Close(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := room.GetParticipant("pubA"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("closed participant 仍留在房间 map（GetParticipant 返回 true）")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if room.Count() != 1 {
		t.Fatalf("自关闭后房间参与者数应为 1，实际 %d", room.Count())
	}
}

// TestSubscribeToIsIdempotent 验证重复订阅同一发布者是幂等的：
// 订阅重试风暴（客户端 1.2s 重试 × N）在订阅已成功但远端 track 未到达时，
// 会再次发起 SubscribeTo —— 服务器必须返回既有订阅而不是 400
// 「已订阅 …，请先 UnsubscribeFrom」——该通用 error 帧曾把 joining 重置为 idle，
// 引发整条级联（ICE 永不连接、摄像头/麦克风/共享屏幕失效）。
func TestSubscribeToIsIdempotent(t *testing.T) {
	mgr := MustNewManager(OfflineSettingEngine())
	room := mgr.JoinRoom("idempotent-room")

	pubA, err := room.AddParticipant("pubA")
	if err != nil {
		t.Fatal(err)
	}
	subB, err := room.AddParticipant("subB")
	if err != nil {
		t.Fatal(err)
	}

	// A 发布一条 track（B 需要可订阅的真实轨）
	clientA := newOfflineClientPC(t, mgr.API())
	track := makeTestTrack(t, "aud-idem", "stream-idem")
	if _, err := clientA.AddTrack(track); err != nil {
		t.Fatal(err)
	}
	offer, err := clientA.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := clientA.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	answer, err := pubA.Offer(offer)
	if err != nil {
		t.Fatal(err)
	}
	if err := clientA.SetRemoteDescription(answer); err != nil {
		t.Fatal(err)
	}
	// 双向 ICE（离线回环 host 候选）
	pubA.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		_ = clientA.AddICECandidate(c.ToJSON())
	})
	clientA.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		_ = pubA.AddICECandidate(c.ToJSON())
	})
	waitConnected(t, pubA.ConnectionState)
	// 触发服务器登记 A 的 track（OnTrack 在 RTP 到达后触发）
	for seq := uint16(0); seq < 4; seq++ {
		_ = track.WriteRTP(makeOpusPacket(seq, 0xB1))
	}
	waitForTracks(t, pubA, 1)

	// 第一次订阅成功
	sub1, _, err := subB.SubscribeTo("pubA")
	if err != nil {
		t.Fatalf("首次订阅失败: %v", err)
	}

	// 第二次订阅（重试风暴）必须幂等返回同一订阅，不得报错
	sub2, _, err := subB.SubscribeTo("pubA")
	if err != nil {
		t.Fatalf("重复订阅不得报错（级联根因）: %v", err)
	}
	if sub1 != sub2 {
		t.Fatal("重复订阅应返回既有 Subscriber 实例")
	}
	if got, ok := subB.GetSubscriber("pubA"); !ok || got != sub1 {
		t.Fatal("订阅表应保持同一实例")
	}

	// 已关闭的订阅必须被替换为新订阅（重连不得复用 closed subscriber）
	if err := sub1.Close(); err != nil {
		t.Fatal(err)
	}
	sub3, _, err := subB.SubscribeTo("pubA")
	if err != nil {
		t.Fatalf("关闭后的重新订阅失败: %v", err)
	}
	if sub3 == sub1 {
		t.Fatal("closed subscriber 不得被复用")
	}
	if got, ok := subB.GetSubscriber("pubA"); !ok || got != sub3 {
		t.Fatal("订阅表应指向新订阅实例")
	}
}

// TestSubscriberCoalescesTracksWhileOfferIsPending 验证多轨道迟到时不会并发
// CreateOffer：首个订阅 offer 尚未收到 answer 时，后续音/视频轨只登记，answer
// 到达后统一生成一个 pending offer。这个行为直接覆盖浏览器偶发缺少反向视频
// 或屏幕共享轨的协商竞态。
func TestSubscriberCoalescesTracksWhileOfferIsPending(t *testing.T) {
	mgr := MustNewManager(OfflineSettingEngine())
	room := mgr.JoinRoom("coalesce-room")
	pub, err := room.AddParticipant("pub")
	if err != nil {
		t.Fatal(err)
	}
	subParticipant, err := room.AddParticipant("sub")
	if err != nil {
		t.Fatal(err)
	}

	clientPub := newOfflineClientPC(t, mgr.API())
	track1 := makeTestTrack(t, "aud-coalesce-1", "stream-coalesce")
	if _, err := clientPub.AddTrack(track1); err != nil {
		t.Fatal(err)
	}
	offer1, err := clientPub.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := clientPub.SetLocalDescription(offer1); err != nil {
		t.Fatal(err)
	}
	answer1, err := pub.Offer(offer1)
	if err != nil {
		t.Fatal(err)
	}
	if err := clientPub.SetRemoteDescription(answer1); err != nil {
		t.Fatal(err)
	}
	pub.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			_ = clientPub.AddICECandidate(c.ToJSON())
		}
	})
	clientPub.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			_ = pub.AddICECandidate(c.ToJSON())
		}
	})
	waitConnected(t, pub.ConnectionState)
	for seq := uint16(0); seq < 4; seq++ {
		_ = track1.WriteRTP(makeOpusPacket(seq, 0xD1))
	}
	waitForTracks(t, pub, 1)

	sub, _, err := subParticipant.SubscribeTo("pub")
	if err != nil {
		t.Fatal(err)
	}
	initialOffer, pending := sub.OfferForRetry()
	if !pending || initialOffer.SDP == "" {
		t.Fatal("首次订阅 offer 应处于等待 answer 状态")
	}

	room.SetOnTrackPublished(func(_ string, _ string, track *webrtc.TrackRemote) {
		_, _ = sub.AddPublishedTrack(track)
	})
	track2 := makeTestTrack(t, "vid-coalesce-2", "stream-coalesce")
	if _, err := clientPub.AddTrack(track2); err != nil {
		t.Fatal(err)
	}
	offer2, err := clientPub.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := clientPub.SetLocalDescription(offer2); err != nil {
		t.Fatal(err)
	}
	answer2, err := pub.Offer(offer2)
	if err != nil {
		t.Fatal(err)
	}
	if err := clientPub.SetRemoteDescription(answer2); err != nil {
		t.Fatal(err)
	}
	for seq := uint16(10); seq < 14; seq++ {
		_ = track2.WriteRTP(makeOpusPacket(seq, 0xD2))
	}
	waitForTracks(t, pub, 2)

	// track2 到达时 initial offer 仍在途：不能替换/并发生成第二个 offer。
	if got, stillPending := sub.OfferForRetry(); !stillPending || got.SDP != initialOffer.SDP {
		t.Fatal("首个 answer 到达前不应替换订阅 offer")
	}

	// 用真实离线 PeerConnection 应答首个订阅 offer，随后才允许生成合并后的 pending offer。
	clientSub := newOfflineClientPC(t, mgr.API())
	if err := clientSub.SetRemoteDescription(initialOffer); err != nil {
		t.Fatal(err)
	}
	answerSub, err := clientSub.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := clientSub.SetLocalDescription(answerSub); err != nil {
		t.Fatal(err)
	}
	if err := sub.SetRemoteDescription(answerSub); err != nil {
		t.Fatal(err)
	}
	next, ok, err := sub.PendingOffer()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || next.SDP == "" || next.SDP == initialOffer.SDP {
		t.Fatal("迟到轨道应在首个 answer 后生成新的合并 offer")
	}
}

// TestPublisherRemovalTearsDownSubscriberPCs 验证发布者被移出房间时，
// 其余成员对该发布者的订阅 PC 被拆除且订阅表清空 —— 否则客户端订阅一条
// 已死的转发连接（旧 session 污染新 session 的服务器侧镜像）。
func TestPublisherRemovalTearsDownSubscriberPCs(t *testing.T) {
	mgr := MustNewManager(OfflineSettingEngine())
	room := mgr.JoinRoom("teardown-room")

	pubA, err := room.AddParticipant("pubA")
	if err != nil {
		t.Fatal(err)
	}
	subB, err := room.AddParticipant("subB")
	if err != nil {
		t.Fatal(err)
	}

	clientA := newOfflineClientPC(t, mgr.API())
	track := makeTestTrack(t, "aud-teardown", "stream-teardown")
	if _, err := clientA.AddTrack(track); err != nil {
		t.Fatal(err)
	}
	offer, err := clientA.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := clientA.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	answer, err := pubA.Offer(offer)
	if err != nil {
		t.Fatal(err)
	}
	if err := clientA.SetRemoteDescription(answer); err != nil {
		t.Fatal(err)
	}
	// 双向 ICE（离线回环 host 候选）
	pubA.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		_ = clientA.AddICECandidate(c.ToJSON())
	})
	clientA.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		_ = pubA.AddICECandidate(c.ToJSON())
	})
	waitConnected(t, pubA.ConnectionState)
	for seq := uint16(0); seq < 4; seq++ {
		_ = track.WriteRTP(makeOpusPacket(seq, 0xC1))
	}
	waitForTracks(t, pubA, 1)

	sub, _, err := subB.SubscribeTo("pubA")
	if err != nil {
		t.Fatal(err)
	}

	// 发布者离开（房主移出 / 客户端断线清理同路径）
	if err := room.RemoveParticipant("pubA"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := subB.GetSubscriber("pubA"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("发布者移出后订阅表仍残留 closed subscriber")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 订阅 PC 必须已关闭
	if !sub.IsClosed() {
		t.Fatal("发布者移出后订阅 PC 未关闭")
	}
	// 再订阅一个已离开的发布者 → 明确的「房间内不存在发布者」错误（而非 closed participant 复用）
	if _, _, err := subB.SubscribeTo("pubA"); err == nil {
		t.Fatal("订阅已离开的发布者应报错")
	}
}

// ── 测试助手 ──────────────────────────────────────────────────────

func makeTestTrack(t *testing.T, id, streamID string) *webrtc.TrackLocalStaticRTP {
	t.Helper()
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
	}, id, streamID)
	if err != nil {
		t.Fatalf("创建测试 track 失败: %v", err)
	}
	return track
}

// waitForTracks 等待参与者登记到 expected 条 track（OnTrack 异步触发）。
func waitForTracks(t *testing.T, p *Participant, expected int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if len(p.PublishedTracks()) >= expected {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("参与者未登记 track（期望 %d，实际 %d）", expected, len(p.PublishedTracks()))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
