package handler

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"letshare-server/internal/model"
)

func waitMinutes(r *wsRPC, timeout time.Duration) (model.WebSocketMessage, error) {
	select {
	case message := <-r.minutes:
		return message, nil
	case <-time.After(timeout):
		return model.WebSocketMessage{}, fmt.Errorf("wait %s timeout", "meeting:minutes")
	}
}

func waitMinutesKind(r *wsRPC, kind string, timeout time.Duration) (model.WebSocketMessage, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		message, err := waitMinutes(r, time.Until(deadline))
		if err != nil {
			return model.WebSocketMessage{}, err
		}
		if minutesPayloadKind(message) == kind {
			return message, nil
		}
	}
	return model.WebSocketMessage{}, fmt.Errorf("wait meeting:minutes kind=%s timeout", kind)
}

func minutesPayloadKind(message model.WebSocketMessage) string {
	var payload map[string]interface{}
	_ = json.Unmarshal(message.Data, &payload)
	kind, _ := payload["kind"].(string)
	return kind
}

func minutesPayload(t *testing.T, message model.WebSocketMessage) map[string]interface{} {
	t.Helper()
	var payload map[string]interface{}
	if err := json.Unmarshal(message.Data, &payload); err != nil {
		t.Fatalf("decode meeting:minutes: %v", err)
	}
	return payload
}

func sendMinutes(t *testing.T, r *wsRPC, meetingID string, payload map[string]interface{}) {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode meeting:minutes: %v", err)
	}
	if err := r.sendJSON(model.WebSocketMessage{Type: model.MessageTypeMeetingMinutes, Channel: meetingID, Data: data}); err != nil {
		t.Fatalf("send meeting:minutes: %v", err)
	}
}

func TestMeetingMinutes_HostConfigAndSegmentIsolation(t *testing.T) {
	_, host, member, outsider, meetingID := meetingChatFlow(t)

	// Only an SFU participant can use the minutes plane; a source-room
	// subscriber must not be able to inject transcript data.
	sendMinutes(t, outsider, meetingID, map[string]interface{}{"action": "segment", "text": "not allowed"})
	if message, err := outsider.waitError(3 * time.Second); err != nil || !strings.Contains(message, "meeting:minutes") {
		t.Fatalf("outsider should be rejected, message=%q err=%v", message, err)
	}

	// A key-shaped field is deliberately ignored by the server and must never
	// be reflected in a public state frame.
	sendMinutes(t, host, meetingID, map[string]interface{}{
		"action": "configure", "requireConsent": true,
		"asrSource": "browser-speech", "asrModel": "browser-network",
		"summaryProvider": "mimo", "summaryModel": "mimo-v2.5-pro",
		"apiKey": "must-not-leak",
	})
	configured, err := waitMinutes(member, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if payload := minutesPayload(t, configured); payload["kind"] != "configured" {
		t.Fatalf("expected configured frame, got %#v", payload)
	} else if strings.Contains(string(configured.Data), "must-not-leak") {
		t.Fatal("meeting:minutes echoed an API key-shaped field")
	}

	sendMinutes(t, host, meetingID, map[string]interface{}{"action": "start"})
	started, err := waitMinutes(member, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if payload := minutesPayload(t, started); payload["kind"] != "started" {
		t.Fatalf("expected started frame, got %#v", payload)
	}

	sendMinutes(t, member, meetingID, map[string]interface{}{
		"action": "segment", "segmentId": "client-controlled-id", "text": "决定下周完成验收", "startMs": 10, "endMs": 20, "final": true,
	})
	if message, err := member.waitError(3 * time.Second); err != nil || !strings.Contains(message, "鍚屾剰") {
		t.Fatalf("member without consent should be rejected, message=%q err=%v", message, err)
	}
	sendMinutes(t, member, meetingID, map[string]interface{}{"action": "consent", "accepted": true})
	if _, err := waitMinutesKind(host, "consent", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	sendMinutes(t, member, meetingID, map[string]interface{}{
		"action": "segment", "segmentId": "client-controlled-id", "text": "鍐冲畾涓嬪懆瀹屾垚楠屾敹", "startMs": 10, "endMs": 20, "final": true,
	})
	segment, err := waitMinutesKind(host, "segment", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	segmentPayload := minutesPayload(t, segment)
	if segmentPayload["kind"] != "segment" || segmentPayload["from"] != "bob:chat-2" {
		t.Fatalf("unexpected segment payload: %#v", segmentPayload)
	}
	if segmentPayload["speakerName"] != "bob" {
		t.Fatalf("server should canonicalize speakerName, got %#v", segmentPayload["speakerName"])
	}

	sendMinutes(t, host, meetingID, map[string]interface{}{"action": "summary", "summary": "验收安排已确认"})
	summary, err := waitMinutesKind(member, "summary", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if payload := minutesPayload(t, summary); payload["kind"] != "summary" || payload["summary"] != "验收安排已确认" {
		t.Fatalf("unexpected summary payload: %#v", payload)
	}
}

func TestMeetingMinutes_NonHostCannotConfigureOrStart(t *testing.T) {
	_, host, member, _, meetingID := meetingChatFlow(t)

	sendMinutes(t, member, meetingID, map[string]interface{}{
		"action": "configure", "asrSource": "browser-speech", "summaryProvider": "mimo",
	})
	if message, err := member.waitError(3 * time.Second); err != nil || !strings.Contains(message, "主持人") {
		t.Fatalf("non-host configure should be rejected, message=%q err=%v", message, err)
	}

	sendMinutes(t, host, meetingID, map[string]interface{}{"action": "consent", "accepted": true})
	// Consent before configure is a valid no-op state transition and must not
	// panic or disconnect the WebSocket.
	if _, err := waitMinutes(member, 3*time.Second); err != nil {
		t.Fatal(err)
	}
}
