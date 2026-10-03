package sfu

import (
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"
)

// AC-001: the public SFU subscriber offer must include its gathered ICE
// candidates so a caller can establish the server-facing receive connection.
func TestAC001_SubscriberOfferCarriesICECandidates(t *testing.T) {
	api, err := NewAPI(OfflineSettingEngine())
	if err != nil {
		t.Fatalf("create offline SFU API: %v", err)
	}
	pc, err := api.newPeerConnection()
	if err != nil {
		t.Fatalf("create subscriber peer connection: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
		Channels:  2,
	}, "audio", "stream")
	if err != nil {
		t.Fatalf("create audio track: %v", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatalf("add audio track: %v", err)
	}

	subscriber := &Subscriber{pc: pc, locTracks: map[string]*subTrack{}, stop: make(chan struct{})}
	subscriber.offerMu.Lock()
	offer, err := subscriber.createOfferLocked()
	subscriber.offerMu.Unlock()
	if err != nil {
		t.Fatalf("create subscriber offer: %v", err)
	}
	if !strings.Contains(offer.SDP, "a=candidate:") {
		t.Fatal("AC-001: subscriber offer omitted all SFU ICE candidates")
	}
}
