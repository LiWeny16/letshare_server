package sfu

import (
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// FIX-AC-003: a fanout closes when its RTP reader exits while the participant
// remains connected, and it refuses outputs that would attach to the dead reader.
func TestFixAC003_FanoutClosesAfterRemoteReadError(t *testing.T) {
	mgr := MustNewManager(OfflineSettingEngine())
	room := mgr.JoinRoom("fanout-reader-exit-room")
	t.Cleanup(func() { _ = room.Close() })

	pub, err := room.AddParticipant("fanout-reader-exit-pub")
	if err != nil {
		t.Fatal(err)
	}
	client, localTrack := dialPair(t, mgr.API(), pub, "aud-reader-exit", "stream-reader-exit")
	for seq := uint16(0); seq < 5; seq++ {
		if err := localTrack.WriteRTP(makeOpusPacket(seq, 0xC3)); err != nil {
			t.Fatalf("write initial RTP: %v", err)
		}
	}
	waitForCount(t, pub.PublishedTracks, 1)

	published := pub.PublishedTracks()
	if len(published) != 1 {
		t.Fatalf("published tracks = %d, want 1", len(published))
	}
	fanout := pub.trackFanout(published[0].ID())
	if fanout == nil {
		t.Fatal("published track has no fanout")
	}
	if fanout.isClosed() {
		t.Fatal("freshly published fanout reports closed")
	}
	if pub.IsClosed() || pub.ConnectionState() != webrtc.PeerConnectionStateConnected || client.ConnectionState() != webrtc.PeerConnectionStateConnected {
		t.Fatal("participant and publisher PC must remain connected before forcing the track read error")
	}

	// Stopping this receiver makes its active remote track reader exit without
	// closing the participant's PeerConnection.
	var receiver *webrtc.RTPReceiver
	for _, candidate := range pub.currentPeerConnection().GetReceivers() {
		if candidate.Track() == published[0] {
			receiver = candidate
			break
		}
	}
	if receiver == nil {
		t.Fatal("published track has no matching RTP receiver")
	}
	if err := receiver.Stop(); err != nil {
		t.Fatalf("stop published track receiver: %v", err)
	}
	if pub.IsClosed() || pub.ConnectionState() != webrtc.PeerConnectionStateConnected || client.ConnectionState() != webrtc.PeerConnectionStateConnected {
		t.Fatal("stopping the track receiver unexpectedly closed the participant connection")
	}
	deadline := time.NewTimer(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for !fanout.isClosed() {
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("fanout stayed open after its RTP reader exited")
		}
	}

	if pub.IsClosed() || pub.ConnectionState() != webrtc.PeerConnectionStateConnected || client.ConnectionState() != webrtc.PeerConnectionStateConnected {
		t.Fatal("forcing the track read error unexpectedly closed the participant connection")
	}

	lateOutput := makeTestTrack(t, "late-reader-exit-audio", "late-reader-exit-stream")
	if fanout.add("late-reader-exit-key", lateOutput) {
		t.Fatal("fanout accepted an output after its RTP reader exited")
	}
}
