package handler

import (
	"encoding/json"
	"testing"
	"time"

	"letshare-server/internal/model"
)

func TestMeetingMediaState_IsMeetingOwnedAndIncludedInSnapshot(t *testing.T) {
	_, host, guest, bystander, meetingID := meetingChatFlow(t)

	data, _ := json.Marshal(map[string]interface{}{
		"muted":         false,
		"cameraOn":      true,
		"screenOn":      false,
		"cameraTrackId": "camera-track-a",
	})
	if err := host.sendJSON(model.WebSocketMessage{Type: "meeting:media-state", Channel: meetingID, Data: data}); err != nil {
		t.Fatalf("send meeting media state: %v", err)
	}

	select {
	case message := <-guest.mediaState:
		var payload map[string]interface{}
		if err := json.Unmarshal(message.Data, &payload); err != nil {
			t.Fatalf("decode meeting media state: %v", err)
		}
		if payload["uniqId"] != "alice:chat-1" || payload["cameraOn"] != true || payload["muted"] != false {
			t.Fatalf("unexpected media state: %#v", payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("meeting member did not receive media state")
	}

	select {
	case message := <-bystander.mediaState:
		t.Fatalf("non-member received media state: %#v", message)
	case <-time.After(300 * time.Millisecond):
	}

	if got, ok := hostStateForTest(t, host, meetingID, "bob:chat-2"); !ok || got["cameraOn"] != false || got["muted"] != true {
		t.Fatalf("join snapshot must include default remote media state, got %#v", got)
	}
}

func hostStateForTest(t *testing.T, receiver *wsRPC, meetingID, memberID string) (map[string]interface{}, bool) {
	t.Helper()
	data, _ := json.Marshal(map[string]interface{}{"roomId": meetingID})
	if err := receiver.sendJSON(model.WebSocketMessage{Type: "meeting:join", Channel: meetingID, Data: data}); err != nil {
		t.Fatalf("refresh meeting snapshot: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case message := <-receiver.membership:
			if message.Type != model.MessageTypeMeetingMembershipSnapshot {
				continue
			}
			var payload struct {
				Members []map[string]interface{} `json:"members"`
			}
			if err := json.Unmarshal(message.Data, &payload); err != nil {
				t.Fatalf("decode meeting snapshot: %v", err)
			}
			for _, member := range payload.Members {
				if member["uniqId"] == memberID {
					media, _ := member["media"].(map[string]interface{})
					return media, media != nil
				}
			}
		case <-deadline:
			return nil, false
		}
	}
}
