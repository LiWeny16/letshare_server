package sfu

import (
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"
)

func TestParticipantRecoversAfterInvalidInitialOffer(t *testing.T) {
	mgr := MustNewManager(OfflineSettingEngine())
	room := mgr.JoinRoom("recovery-room")
	part, err := room.AddParticipant("recovery-user")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = room.Close() })

	client, err := mgr.API().NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
	}, "recovery-audio", "recovery-stream")
	if err != nil {
		t.Fatal(err)
	}
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
	<-webrtc.GatheringCompletePromise(client)
	offer = *client.LocalDescription()

	badOffer := offer
	badOffer.SDP = strings.ReplaceAll(badOffer.SDP, "a=ice-ufrag:", "a=x-removed-ice-ufrag:")
	badOffer.SDP = strings.ReplaceAll(badOffer.SDP, "a=ice-pwd:", "a=x-removed-ice-pwd:")
	if _, err := part.Offer(badOffer); err == nil || !strings.Contains(err.Error(), "no ice-ufrag") {
		t.Fatalf("bad offer error = %v, want no ice-ufrag", err)
	}

	answer, err := part.Offer(offer)
	if err != nil {
		t.Fatalf("valid offer retry failed after bad offer: %v", err)
	}
	if answer.Type != webrtc.SDPTypeAnswer || !strings.Contains(answer.SDP, "a=ice-ufrag:") {
		t.Fatalf("invalid retry answer: type=%s hasICE=%t", answer.Type, strings.Contains(answer.SDP, "a=ice-ufrag:"))
	}
	if err := client.SetRemoteDescription(answer); err != nil {
		t.Fatalf("client rejected retry answer: %v", err)
	}
}
