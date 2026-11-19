package handler

import (
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"letshare-server/internal/sfu"

	"github.com/pion/webrtc/v4"
)

func TestMeetingAudioPublishedDuringSubscriptionCreation(t *testing.T) {
	var blockGathering atomic.Bool
	var started, released sync.Once
	gathering := make(chan struct{})
	allowGathering := make(chan struct{})
	defer released.Do(func() { close(allowGathering) })
	engine := sfu.OfflineSettingEngine()
	engine.SetIncludeLoopbackCandidate(true)
	engine.SetIPFilter(func(net.IP) bool {
		if blockGathering.Load() {
			started.Do(func() { close(gathering) })
			<-allowGathering
		}
		return true
	})
	manager := sfu.MustNewManager(engine)
	h := &WebSocketHandler{sfuManager: manager, meetings: newMeetingRegistry()}
	room := manager.JoinRoom("1234")
	t.Cleanup(func() { _ = room.Close() })
	addParticipant := func(id string) *sfu.Participant {
		part, err := room.AddParticipant(id)
		if err != nil {
			t.Fatal(err)
		}
		return part
	}
	publisher := addParticipant("publisher")
	client, err := manager.API().NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	video, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeVP8, ClockRate: 90000,
	}, "camera", "media")
	if err != nil {
		t.Fatal(err)
	}
	audio := makeOpusTrack(t, manager.API(), "microphone", "media")
	for _, track := range []*webrtc.TrackLocalStaticRTP{video, audio} {
		if _, err := client.AddTrack(track); err != nil {
			t.Fatal(err)
		}
	}
	await := func(label string, done <-chan struct{}) {
		t.Helper()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for %s", label)
		}
	}
	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	clientGathered := webrtc.GatheringCompletePromise(client)
	if err := client.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	await("publisher ICE gathering", clientGathered)
	answer, err := publisher.Offer(*client.LocalDescription())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRemoteDescription(answer); err != nil {
		t.Fatal(err)
	}
	publisher.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			_ = client.AddICECandidate(candidate.ToJSON())
		}
	})
	waitPCConnected(t, client)

	videoPublished, audioPublished, audioHandled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	room.SetOnTrackPublished(func(roomID, publisherID string, track *webrtc.TrackRemote) {
		if track.ID() == video.ID() {
			close(videoPublished)
			return
		}
		close(audioPublished)
		h.onMeetingTrackPublished(roomID, publisherID, track)
		close(audioHandled)
	})
	if err := video.WriteRTP(makeOpusRTPPacket(1, 1)); err != nil {
		t.Fatal(err)
	}
	await("initial video track", videoPublished)
	if _, _, err := addParticipant("existing-listener").SubscribeTo(publisher.ID); err != nil {
		t.Fatal(err)
	}
	newListener := addParticipant("third-participant")
	buildDone := make(chan struct{})
	var sub *sfu.Subscriber
	var subscribeErr error
	blockGathering.Store(true)
	go func() {
		defer close(buildDone)
		sub, _, subscribeErr = newListener.SubscribeTo(publisher.ID)
	}()
	await("third participant's initial video-only offer to start gathering", gathering)
	if err := audio.WriteRTP(makeOpusRTPPacket(1, 2)); err != nil {
		t.Fatal(err)
	}
	await("microphone registration during subscription creation", audioPublished)
	released.Do(func() { close(allowGathering) })
	await("initial subscription", buildDone)
	await("track publication handler", audioHandled)
	if subscribeErr != nil {
		t.Fatal(subscribeErr)
	}

	receiver, err := manager.API().NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = receiver.Close() })
	if offer := sub.Offer(); strings.Contains(offer.SDP, "m=audio ") {
		t.Fatal("initial offer must predate the audio track for this regression test")
	} else if err := receiver.SetRemoteDescription(offer); err != nil {
		t.Fatal(err)
	}
	videoAnswer, err := receiver.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.SetRemoteDescription(videoAnswer); err != nil {
		t.Fatal(err)
	}
	lateOffer, pending, err := sub.PendingOffer()
	if err != nil || !pending || !strings.Contains(lateOffer.SDP, "m=audio ") {
		t.Fatalf("expected a follow-up offer with the microphone track, pending=%v err=%v", pending, err)
	}
	if err := sub.SetRemoteDescription(videoAnswer); err != nil {
		t.Fatalf("duplicate answer must be harmless while a newer offer is pending: %v", err)
	}
	if retry, pending := sub.OfferForRetry(); !pending || retry.SDP != lateOffer.SDP {
		t.Fatal("an old answer must leave the newer audio offer pending")
	}
}
