package handler

import (
	"testing"
	"time"

	"letshare-server/internal/model"
)

func TestMeetingReservationExpiresBeforeJoin(t *testing.T) {
	oldTTL := meetingReservationTTL
	meetingReservationTTL = 50 * time.Millisecond
	defer func() { meetingReservationTTL = oldTTL }()

	ts := newMeetingTestServer(t)
	defer ts.Close()
	host := newWSRPC(t, ts.srv, "reservation-host:uuid-1")

	if err := host.sendJSON(model.WebSocketMessage{
		Type: model.MessageTypeMeetingCreate,
		Data: []byte(`{"title":"reservation cleanup"}`),
	}); err != nil {
		t.Fatalf("send meeting:create: %v", err)
	}
	meetingID, err := host.waitCreate(5 * time.Second)
	if err != nil {
		t.Fatalf("wait meeting:create: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := ts.handler.activeMeetingRooms.Load(meetingID); !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("unjoined meeting reservation %s was not reclaimed", meetingID)
}
