package handler

import (
	"encoding/json"
	"testing"
	"time"

	"letshare-server/internal/model"
)

func TestMeetingTitleUpdateSyncAndHostAuthorization(t *testing.T) {
	_, host, member, _, room := meetingChatFlow(t)
	data := []byte(`{"title":"产品讨论 😀"}`)
	if err := host.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingUpdate, Channel: room, Data: data}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case message := <-member.info:
			var payload struct {
				Title string `json:"title"`
			}
			if err := json.Unmarshal(message.Data, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Title == "产品讨论 😀" {
				goto updated
			}
		case <-deadline:
			t.Fatal("member did not receive updated meeting title")
		}
	}
updated:
	if err := member.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingUpdate, Channel: room, Data: []byte(`{"title":"not allowed"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := member.waitError(5 * time.Second); err != nil {
		t.Fatalf("member title update should be rejected: %v", err)
	}
}
