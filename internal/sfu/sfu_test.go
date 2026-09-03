package sfu

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// newOfflineClientPC 创建一个“客户端侧”连接，使用与服务端相同的离线 API，
// 保证在同一进程内无外网也能 ICE 连通。
func newOfflineClientPC(t *testing.T, api *API) *webrtc.PeerConnection {
	t.Helper()
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("创建离线客户端 PC 失败: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	return pc
}

// TestManagerRoomsAndParticipantOfferAnswer 验证 Manager 建房间 / 取相同房间、参与者 offer/answer 握手路径，
// 以及本地 ICE 候选能在离线回环下连通。
func TestManagerRoomsAndParticipantOfferAnswer(t *testing.T) {
	mgr := MustNewManager(OfflineSettingEngine())

	room1 := mgr.JoinRoom("room-1")
	room1Again := mgr.JoinRoom("room-1")
	if room1 != room1Again {
		t.Fatalf("JoinRoom 对同 roomID 应返回同一实例")
	}
	if got, ok := mgr.GetRoom("room-1"); !ok || got != room1 {
		t.Fatalf("GetRoom 应返回已创建的房间")
	}

	partA, err := room1.AddParticipant("pubA")
	if err != nil {
		t.Fatalf("AddParticipant 失败: %v", err)
	}
	if partA.ID != "pubA" {
		t.Fatalf("参与者 ID 应为 pubA，实际 %q", partA.ID)
	}
	if room1.Count() != 1 {
		t.Fatalf("房间参与者数量应为 1，实际 %d", room1.Count())
	}

	// 客户端 A：发布一条音频 track 并发起 offer
	clientA := newOfflineClientPC(t, mgr.API())
	opusCap := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	clientATrack, err := webrtc.NewTrackLocalStaticRTP(opusCap, "aud-A", "stream-A")
	if err != nil {
		t.Fatalf("创建客户端本地音频 track 失败: %v", err)
	}
	if _, err := clientA.AddTrack(clientATrack); err != nil {
		t.Fatalf("AddTrack 失败: %v", err)
	}

	offer, err := clientA.CreateOffer(nil)
	if err != nil {
		t.Fatalf("客户端 CreateOffer 失败: %v", err)
	}
	if err := clientA.SetLocalDescription(offer); err != nil {
		t.Fatalf("客户端 SetLocalDescription 失败: %v", err)
	}

	answer, err := partA.Offer(offer)
	if err != nil {
		t.Fatalf("Participant.Offer 失败: %v", err)
	}
	if answer.Type != webrtc.SDPTypeAnswer {
		t.Fatalf("返回值应为 answer，实际 %s", answer.Type.String())
	}
	if err := clientA.SetRemoteDescription(answer); err != nil {
		t.Fatalf("客户端 SetRemoteDescription(answer) 失败: %v", err)
	}

	// 双向 ICE：服务端参与者的候选 -> 客户端；客户端候选 -> 服务端参与者
	partA.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		_ = clientA.AddICECandidate(c.ToJSON())
	})
	clientA.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		if err := partA.NewICECandidate(c); err != nil {
			t.Errorf("Participant.NewICECandidate 失败: %v", err)
		}
	})

	// 等待连接（本地回环 ICE 应快速连通）
	waitConnected(t, partA.ConnectionState)
}

// TestPublishSubscribeForward 验证完整回路：A publish 一条音频 track，
// B 订阅 A，服务器把 A 的媒体扇出到 B，B 侧能收到被转发出的 RTP。
func TestPublishSubscribeForward(t *testing.T) {
	mgr := MustNewManager(OfflineSettingEngine())
	room := mgr.JoinRoom("room-2")

	partA, err := room.AddParticipant("pubA")
	if err != nil {
		t.Fatalf("AddParticipant(A) 失败: %v", err)
	}
	partB, err := room.AddParticipant("subB")
	if err != nil {
		t.Fatalf("AddParticipant(B) 失败: %v", err)
	}

	// -- 客户端 A 发布音频 --
	clientA := newOfflineClientPC(t, mgr.API())
	opusCap := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	clientATrack, err := webrtc.NewTrackLocalStaticRTP(opusCap, "aud-A", "stream-A")
	if err != nil {
		t.Fatalf("创建本地音频 track 失败: %v", err)
	}
	senderA, err := clientA.AddTrack(clientATrack)
	if err != nil {
		t.Fatalf("AddTrack 失败: %v", err)
	}
	_ = senderA

	offer, err := clientA.CreateOffer(nil)
	if err != nil {
		t.Fatalf("客户端 A CreateOffer 失败: %v", err)
	}
	if err := clientA.SetLocalDescription(offer); err != nil {
		t.Fatalf("客户端 A SetLocalDescription 失败: %v", err)
	}
	answer, err := partA.Offer(offer)
	if err != nil {
		t.Fatalf("Participant A.Offer 失败: %v", err)
	}
	if err := clientA.SetRemoteDescription(answer); err != nil {
		t.Fatalf("客户端 A SetRemoteDescription 失败: %v", err)
	}

	var aMux sync.Mutex
	partA.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		aMux.Lock()
		err := clientA.AddICECandidate(c.ToJSON())
		aMux.Unlock()
		if err != nil {
			t.Errorf("clientA.AddICECandidate 失败: %v", err)
		}
	})
	clientA.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		aMux.Lock()
		err := partA.AddICECandidate(c.ToJSON())
		aMux.Unlock()
		if err != nil {
			t.Errorf("partA.AddICECandidate 失败: %v", err)
		}
	})

	waitConnected(t, partA.ConnectionState)

	// 注入首批媒体，触发服务端 partA.OnTrack 登记该 track。
	// （pion 在收到首个 RTP 包时才回调 OnTrack，因此必须先发媒体。）
	for seq := uint16(0); seq < 5; seq++ {
		if err := clientATrack.WriteRTP(makeOpusPacket(seq, 0xA1)); err != nil {
			t.Fatalf("注入 RTP（触发登记）失败: %v", err)
		}
	}
	waitForCount(t, partA.PublishedTracks, 1)
	tracks := partA.PublishedTracks()
	if tracks[0].ID() != "aud-A" {
		t.Fatalf("登记 track ID 应为 aud-A，实际 %q", tracks[0].ID())
	}

	// -- 客户端 B 作为订阅者，接收“B 订阅 A”的扇出 --
	clientServePC := newOfflineClientPC(t, mgr.API())
	received := make(chan *rtp.Packet, 64)
	clientServePC.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		go func() {
			for {
				pkt, _, err := track.ReadRTP()
				if err != nil {
					if !errors.Is(err, io.EOF) {
						t.Logf("订阅端读取 track 结束: %v", err)
					}
					return
				}
				select {
				case received <- pkt:
				default:
				}
			}
		}()
	})

	sub, err := partB.SubscribeTo("pubA")
	if err != nil {
		t.Fatalf("SubscribeTo 失败: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	// 让客户端 B 回答订阅 offer
	if err := clientServePC.SetRemoteDescription(sub.Offer()); err != nil {
		t.Fatalf("客户端 B SetRemoteDescription(订阅offer) 失败: %v", err)
	}
	subAnswer, err := clientServePC.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("客户端 B CreateAnswer 失败: %v", err)
	}
	if err := clientServePC.SetLocalDescription(subAnswer); err != nil {
		t.Fatalf("客户端 B SetLocalDescription 失败: %v", err)
	}
	if err := sub.SetRemoteDescription(subAnswer); err != nil {
		t.Fatalf("Subscriber.SetRemoteDescription 失败: %v", err)
	}

	var subMux sync.Mutex
	sub.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		subMux.Lock()
		err := clientServePC.AddICECandidate(c.ToJSON())
		subMux.Unlock()
		if err != nil {
			t.Errorf("clientServePC.AddICECandidate 失败: %v", err)
		}
	})
	clientServePC.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		subMux.Lock()
		err := sub.AddICECandidate(c.ToJSON())
		subMux.Unlock()
		if err != nil {
			t.Errorf("sub.AddICECandidate 失败: %v", err)
		}
	})

	waitConnected(t, clientServePC.ConnectionState)

	// -- 客户端 A 注入媒体，验证转发到订阅端 --
	pt := uint8(111) // opus 默认 payload type
	for seq := uint16(100); seq < 130; seq++ {
		if err := clientATrack.WriteRTP(makeOpusPacket(seq, 0xA1)); err != nil {
			t.Fatalf("注入 RTP 失败: %v", err)
		}
	}

	// 给转发管线和 SRTP 一个传送时间
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case p := <-received:
			if p.PayloadType != pt {
				t.Fatalf("转发 payload type 应为 %d，实际 %d", pt, p.PayloadType)
			}
			t.Logf("订阅端成功收到被转发的 RTP 包（PT=%d, seq=%d, len=%d）",
				p.PayloadType, p.SequenceNumber, len(p.Payload))
			return // 收到即说明 P2P->服务器扇出回路打通
		default:
			if time.Now().After(deadline) {
				t.Fatal("超时：订阅端未收到任何被转发的 RTP 包")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// TestParticipantIsolation 验证广播脆弱隔离：
// 关闭一个参与者（模拟其不可用）不影响房间其它参与者及其连接。
// 离线回环下断连即可，重点是不返回错误、房间与另一参与者仍可操作。
func TestParticipantIsolation(t *testing.T) {
	mgr := MustNewManager(OfflineSettingEngine())
	room := mgr.JoinRoom("room-3")

	partA, err := room.AddParticipant("A")
	if err != nil {
		t.Fatalf("AddParticipant(A) 失败: %v", err)
	}
	partB, err := room.AddParticipant("B")
	if err != nil {
		t.Fatalf("AddParticipant(B) 失败: %v", err)
	}
	if room.Count() != 2 {
		t.Fatalf("房间应有 2 名参与者，实际 %d", room.Count())
	}

	// 关闭 A，模拟其不可用
	if err := room.RemoveParticipant("A"); err != nil {
		t.Fatalf("RemoveParticipant(A) 失败: %v", err)
	}
	if _, ok := room.GetParticipant("A"); ok {
		t.Fatalf("A 应已被移除")
	}

	// B 不受影响：仍能拿到、仍能打开 publish 通道
	if got, ok := room.GetParticipant("B"); !ok || got != partB {
		t.Fatalf("B 应仍在房间内")
	}
	// 关闭已离席参与者的连接（幂等，不应 panic / 返回致命错误）
	_ = partA.Close()
	if room.Count() != 1 {
		t.Fatalf("移除 A 后房间应只剩 1 名参与者，实际 %d", room.Count())
	}
}

func waitConnected(t *testing.T, state func() webrtc.PeerConnectionState) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if state() == webrtc.PeerConnectionStateConnected {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("连接未建立，state=%s", state().String())
}

func waitForCount(t *testing.T, get func() []*webrtc.TrackRemote, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if n := len(get()); n == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待 %d 条 track 超时，当前 %d", want, len(get()))
}

// makeOpusPacket 构造一个可被 TrackLocalStaticRTP 发送的最小合法 opus RTP 包。
func makeOpusPacket(seq uint16, ssrc uint32) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    111, // opus
			SequenceNumber: seq,
			Timestamp:      uint32(48000 / 50 * seq),
			SSRC:           ssrc,
		},
		Payload: []byte{0xF8, 0xFF, 0xFE, 0x00, 0x01, 0x02},
	}
}
