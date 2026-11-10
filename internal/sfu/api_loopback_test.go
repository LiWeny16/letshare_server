package sfu

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// A loopback address is useful for a local websocket endpoint, but it is not
// a public interface address to rewrite ICE host candidates to. This exercises
// the production setting path with real Pion media rather than a fake SDP.
func TestPublicSettingEngineLoopbackKeepsAudioReachable(t *testing.T) {
	manager := MustNewManager(PublicSettingEngine("127.0.0.1"))
	room := manager.JoinRoom("loopback-audio-test")
	t.Cleanup(func() { manager.RemoveRoom("loopback-audio-test") })

	publisher, err := room.AddParticipant("loopback-audio-publisher")
	if err != nil {
		t.Fatal(err)
	}
	subscriber, err := room.AddParticipant("loopback-audio-subscriber")
	if err != nil {
		t.Fatal(err)
	}

	var tapped atomic.Int32
	room.SetAudioTap(func(_ string, _ *webrtc.TrackRemote, _ *rtp.Packet) {
		tapped.Add(1)
	})

	_, localTrack := dialPair(t, manager.API(), publisher, "loopback-audio", "loopback-stream")
	for seq := uint16(0); seq < 5; seq++ {
		if err := localTrack.WriteRTP(makeOpusPacket(seq, 0x12345678)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitForCount(t, publisher.PublishedTracks, 1)

	sub, _, err := subscriber.SubscribeTo(publisher.ID)
	if err != nil {
		t.Fatal(err)
	}
	subscriberClient := newOfflineClientPC(t, manager.API())
	received := make(chan struct{}, 128)
	subscriberClient.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
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
	if err := subscriberClient.SetRemoteDescription(sub.Offer()); err != nil {
		t.Fatal(err)
	}
	answer, err := subscriberClient.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := subscriberClient.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	if err := sub.SetRemoteDescription(answer); err != nil {
		t.Fatal(err)
	}
	exchangeICECandidates(t, sub, subscriberClient)
	waitConnected(t, subscriberClient.ConnectionState)

	for seq := uint16(10); seq < 50; seq++ {
		if err := localTrack.WriteRTP(makeOpusPacket(seq, 0x12345678)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	deadline := time.Now().Add(3 * time.Second)
	for (tapped.Load() == 0 || len(received) == 0) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if tapped.Load() == 0 {
		t.Fatal("room audio tap received no publisher RTP")
	}
	if len(received) == 0 {
		t.Fatal("subscriber received no forwarded RTP")
	}
}
