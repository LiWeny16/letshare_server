package sfu

import (
	"testing"

	"github.com/pion/webrtc/v4"
)

func TestSubscriberBuffersICECandidatesUntilCallbackIsRegistered(t *testing.T) {
	subscriber := &Subscriber{}
	candidate := &webrtc.ICECandidate{}
	subscriber.emitICECandidate(candidate)

	var received []*webrtc.ICECandidate
	subscriber.OnICECandidate(func(value *webrtc.ICECandidate) {
		received = append(received, value)
	})

	if len(received) != 1 || received[0] != candidate {
		t.Fatalf("buffered subscriber ICE candidate was not delivered: got %d candidates", len(received))
	}
}

func TestParticipantBuffersICECandidatesUntilCallbackIsRegistered(t *testing.T) {
	participant := &Participant{}
	candidate := &webrtc.ICECandidate{}
	participant.emitICECandidate(candidate)

	var received []*webrtc.ICECandidate
	participant.OnICECandidate(func(value *webrtc.ICECandidate) {
		received = append(received, value)
	})

	if len(received) != 1 || received[0] != candidate {
		t.Fatalf("buffered participant ICE candidate was not delivered: got %d candidates", len(received))
	}
}
