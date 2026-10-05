package sfu

import (
	"testing"

	"github.com/pion/webrtc/v4"
)

// AC-ZOMBIE-1: A closed participant must never remain observable in the room.
//
// Before the fix, Participant.Close() returned early when the underlying
// PeerConnection returned an error from Close() (routine once the connection
// has already failed), so room.forgetClosedParticipant was never reached. The
// participant stayed in the room map with closed=true, and every later
// rejoin/re-subscribe resolved to that zombie: its fanouts were already
// stopped, so a freshly negotiated subscriber received zero RTP forever.
func TestClosedParticipantIsRemovedFromRoomMap(t *testing.T) {
	mgr := MustNewManager(OfflineSettingEngine())
	room := mgr.JoinRoom("zombie-room")
	t.Cleanup(func() { _ = room.Close() })

	part, err := room.AddParticipant("zombie-user")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := room.GetParticipant("zombie-user"); !ok {
		t.Fatal("participant missing from room right after AddParticipant")
	}

	// Close twice on purpose: the second call must stay idempotent and must not
	// resurrect or re-error on an already-retired participant.
	_ = part.Close()
	_ = part.Close()

	if !part.IsClosed() {
		t.Fatal("participant should report itself closed")
	}
	if _, ok := room.GetParticipant("zombie-user"); ok {
		t.Fatal("closed participant is still observable via GetParticipant: " +
			"rejoin would reuse a participant whose fanouts are already stopped")
	}
	if room.Count() != 0 {
		t.Fatalf("room count = %d, want 0", room.Count())
	}

	// A rejoin must be able to install a brand-new, open participant under the
	// same uniqID. This is the exact recovery path used after a transient PC
	// failure, and it is what the zombie used to block.
	reborn, err := room.AddParticipant("zombie-user")
	if err != nil {
		t.Fatalf("rejoin after close failed: %v", err)
	}
	if reborn.IsClosed() {
		t.Fatal("rejoined participant is already closed")
	}
	if _, ok := room.GetParticipant("zombie-user"); !ok {
		t.Fatal("rejoined participant is not observable")
	}
	if room.Count() != 1 {
		t.Fatalf("room count after rejoin = %d, want 1", room.Count())
	}
}

// AC-ZOMBIE-3: Participant.Close() must complete self-removal regardless of
// whether the underlying PeerConnection reports an error from Close().
//
// Pion v4's close() returns util.FlattenErrs of interceptor/transceiver/DTLS/
// ICE/SCTP Stop() results, which is non-nil for connections that already
// failed. Participant.Close() used to `return err` on that path, skipping the
// self-removal and leaving a closed participant in the room map forever.
//
// This asserts the invariant directly (closed => not observable) so it holds
// whichever path pc.Close() takes.
func TestClosedParticipantIsNeverObservable(t *testing.T) {
	mgr := MustNewManager(OfflineSettingEngine())
	room := mgr.JoinRoom("close-error-room")
	t.Cleanup(func() { _ = room.Close() })

	part, err := room.AddParticipant("close-error-user")
	if err != nil {
		t.Fatal(err)
	}
	// Retire the underlying PC first so Close() exercises the already-failed
	// connection path (the real-world shape of this bug).
	_ = part.currentPeerConnection().Close()

	if err := part.Close(); err != nil {
		t.Logf("Participant.Close returned %v; self-removal must still have run", err)
	}

	if _, ok := room.GetParticipant("close-error-user"); ok {
		t.Fatal("closed participant remained observable after Close(): " +
			"rejoin and re-subscribe would bind to stopped fanouts")
	}
	if room.Count() != 0 {
		t.Fatalf("room count = %d, want 0", room.Count())
	}
}

// AC-ZOMBIE-4: The rejoin guard used by handleMeetingJoin is
// `GetParticipant(id)` + `!IsClosed()`. This locks in the property that a
// closed participant must not satisfy that guard, while a live one must.
func TestRejoinGuardRejectsClosedParticipant(t *testing.T) {
	mgr := MustNewManager(OfflineSettingEngine())
	room := mgr.JoinRoom("c_rejoin_room")
	t.Cleanup(func() { _ = room.Close() })

	live, err := room.AddParticipant("rejoin-user")
	if err != nil {
		t.Fatal(err)
	}

	// Same predicate handleMeetingJoin uses to decide "already joined".
	alreadyJoined := func(id string) bool {
		p, ok := room.GetParticipant(id)
		return ok && !p.IsClosed()
	}

	if !alreadyJoined("rejoin-user") {
		t.Fatal("live participant must be treated as already joined")
	}

	_ = live.Close()
	if alreadyJoined("rejoin-user") {
		t.Fatal("closed participant satisfied the rejoin guard: rejoin would " +
			"return early without rebuilding, leaving the call permanently silent")
	}

	// After the rebuild the guard must be true again.
	if _, err := room.AddParticipant("rejoin-user"); err != nil {
		t.Fatal(err)
	}
	if !alreadyJoined("rejoin-user") {
		t.Fatal("rebuilt participant must be treated as already joined")
	}
}

// AC-ZOMBIE-2: Once a track's fanout is closed it must refuse new outputs, so a
// subscriber can never be wired to a track whose read loop has exited. Such a
// subscription negotiates successfully but delivers no RTP forever.
func TestClosedFanoutRejectsNewOutputs(t *testing.T) {
	mgr := MustNewManager(OfflineSettingEngine())
	room := mgr.JoinRoom("fanout-room")
	t.Cleanup(func() { _ = room.Close() })

	pub, err := room.AddParticipant("fanout-pub")
	if err != nil {
		t.Fatal(err)
	}
	client := newOfflineClientPC(t, mgr.API())
	track := makeTestTrack(t, "aud-fanout", "stream-fanout")
	if _, err := client.AddTrack(track); err != nil {
		t.Fatal(err)
	}
	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	answer, err := pub.Offer(offer)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRemoteDescription(answer); err != nil {
		t.Fatal(err)
	}
	pub.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			_ = client.AddICECandidate(c.ToJSON())
		}
	})
	client.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			_ = pub.AddICECandidate(c.ToJSON())
		}
	})
	waitConnected(t, pub.ConnectionState)
	for seq := uint16(0); seq < 4; seq++ {
		_ = track.WriteRTP(makeOpusPacket(seq, 0xC1))
	}
	waitForTracks(t, pub, 1)

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

	fanout.close()
	if !fanout.isClosed() {
		t.Fatal("fanout still reports open after close")
	}

	local := makeTestTrack(t, "late-audio", "late-stream")
	if fanout.add("late-key", local) {
		t.Fatal("closed fanout accepted a new output: subscriber would attach to a dead track")
	}
}
