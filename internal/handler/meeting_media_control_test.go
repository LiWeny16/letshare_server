package handler

import (
	"encoding/json"
	"testing"
	"time"

	"letshare-server/internal/model"
)

func mediaControlPayload(t *testing.T, message model.WebSocketMessage) map[string]interface{} {
	t.Helper()
	var payload map[string]interface{}
	if err := json.Unmarshal(message.Data, &payload); err != nil {
		t.Fatalf("parse meeting:media-control payload: %v", err)
	}
	return payload
}

func sendMediaControl(t *testing.T, client *wsRPC, meetingID, action string) {
	t.Helper()
	data, _ := json.Marshal(map[string]string{"action": action})
	if err := client.sendJSON(model.WebSocketMessage{
		Type:    model.MessageTypeMeetingMediaControl,
		Channel: meetingID,
		Data:    data,
	}); err != nil {
		t.Fatalf("send meeting:media-control: %v", err)
	}
}

func TestMeetingMediaControl_HostBroadcastsAndNonHostIsRejected(t *testing.T) {
	_, host, guest, bystander, meetingID := meetingChatFlow(t)

	sendMediaControl(t, host, meetingID, "mute-all")
	select {
	case message := <-guest.mediaControl:
		payload := mediaControlPayload(t, message)
		if payload["action"] != "mute-all" || payload["from"] != "alice:chat-1" {
			t.Fatalf("unexpected mute-all payload: %#v", payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("guest did not receive host mute-all control")
	}

	select {
	case message := <-bystander.mediaControl:
		t.Fatalf("non-member received media control: %#v", message)
	case <-time.After(300 * time.Millisecond):
	}

	sendMediaControl(t, host, meetingID, "request-unmute")
	select {
	case message := <-guest.mediaControl:
		payload := mediaControlPayload(t, message)
		if payload["action"] != "request-unmute" {
			t.Fatalf("unexpected request-unmute payload: %#v", payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("guest did not receive host request-unmute control")
	}

	sendMediaControl(t, guest, meetingID, "mute-all")
	if _, err := guest.waitError(5 * time.Second); err != nil {
		t.Fatalf("non-host media control should be rejected: %v", err)
	}
}
