package handler

import (
	"encoding/json"
	"testing"
	"time"

	"letshare-server/internal/model"
)

func TestMeetingEndDoesNotLeakToSubscribedNonMember(t *testing.T) {
	ts := newMeetingTestServer(t)
	t.Cleanup(ts.Close)
	host := newWSRPC(t, ts.srv, "alice:end-1")
	observer := newWSRPC(t, ts.srv, "carol:end-3")

	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingCreate, Data: []byte(`{}`)}); err != nil {
		t.Fatalf("send meeting:create: %v", err)
	}
	meetingID, err := host.waitCreate(5 * time.Second)
	if err != nil {
		t.Fatalf("wait meeting:create: %v", err)
	}
	for _, r := range []*wsRPC{host, observer} {
		if err := r.sendJSON(model.WebSocketMessage{Type: "subscribe", Channel: meetingID, Event: "signal:all"}); err != nil {
			t.Fatalf("subscribe meeting room: %v", err)
		}
		if err := r.waitSubscribed(5 * time.Second); err != nil {
			t.Fatalf("wait subscribed: %v", err)
		}
	}
	joinData, _ := json.Marshal(map[string]string{"roomId": meetingID})
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingJoin, Channel: meetingID, Data: joinData}); err != nil {
		t.Fatalf("send meeting:join: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		room, ok := ts.handler.sfuManager.GetRoom(meetingID)
		if ok && len(meetingParticipantIDs(room)) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("wait host participant timeout")
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingLeave, Channel: meetingID, Data: []byte(`{}`)}); err != nil {
		t.Fatalf("send meeting:leave: %v", err)
	}
	select {
	case <-host.ended:
	case <-time.After(2 * time.Second):
		t.Fatal("host did not receive meeting:ended")
	}
	select {
	case msg := <-observer.ended:
		t.Fatalf("subscribed non-member received meeting:ended: %#v", msg)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestMeetingBreakoutEnforcesMainMembersAndChildAllowlist(t *testing.T) {
	ts, host, member, outsider, meetingID := meetingChatFlow(t)

	invalidData, _ := json.Marshal(map[string]interface{}{
		"action":      "create",
		"assignments": []map[string]interface{}{{"room": meetingID + "B1", "members": []string{"carol:chat-3"}}},
	})
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingBreakout, Channel: meetingID, Data: invalidData}); err != nil {
		t.Fatalf("send invalid breakout assignment: %v", err)
	}
	if _, err := host.waitError(5 * time.Second); err != nil {
		t.Fatalf("invalid breakout assignment should be rejected: %v", err)
	}
	if _, ok := ts.handler.activeMeetingRooms.Load(meetingID + "B1"); ok {
		t.Fatal("invalid breakout assignment created a child room")
	}
	validData, _ := json.Marshal(map[string]interface{}{
		"action":      "create",
		"assignments": []map[string]interface{}{{"room": meetingID + "B1", "members": []string{"bob:chat-2"}}},
	})
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingBreakout, Channel: meetingID, Data: validData}); err != nil {
		t.Fatalf("send valid breakout assignment: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := ts.handler.activeMeetingRooms.Load(meetingID + "B1"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("valid breakout child room was not registered")
		}
		time.Sleep(25 * time.Millisecond)
	}
	joinData, _ := json.Marshal(map[string]string{"roomId": meetingID + "B1"})
	if err := outsider.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingJoin, Channel: meetingID + "B1", Data: joinData}); err != nil {
		t.Fatalf("send unauthorized child join: %v", err)
	}
	if _, err := outsider.waitError(5 * time.Second); err != nil {
		t.Fatalf("unauthorized child join should be rejected: %v", err)
	}
	child, ok := ts.handler.sfuManager.GetRoom(meetingID + "B1")
	if ok && len(meetingParticipantIDs(child)) != 0 {
		t.Fatal("unauthorized child join registered a participant")
	}
	select {
	case <-member.breakout:
	case <-time.After(2 * time.Second):
		t.Fatal("assigned member did not receive breakout invite")
	}
}
