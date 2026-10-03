package sfu

import "testing"

func TestFanoutOutputKeyUniquePerSubscriber(t *testing.T) {
	participant := &Participant{ID: "subscriber"}
	first := &Subscriber{publisherID: "publisher", forParticipant: participant}
	second := &Subscriber{publisherID: "publisher", forParticipant: participant}

	firstKey := fanoutOutputKey(first, "audio-track")
	secondKey := fanoutOutputKey(second, "audio-track")
	if firstKey == secondKey {
		t.Fatalf("separate subscriber connections must have separate fanout keys, got %q", firstKey)
	}
}
