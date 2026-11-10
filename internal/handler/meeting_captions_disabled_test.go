package handler

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func assertNoNonEmptyMeetingMinutesSegments(t *testing.T, client *wsRPC, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		message, err := waitMinutes(client, time.Until(deadline))
		if err != nil {
			return
		}
		if minutesPayloadKind(message) != "segments" {
			continue
		}
		segments, ok := minutesPayload(t, message)["segments"].([]interface{})
		if !ok {
			t.Fatal("meeting:minutes segments frame is malformed")
		}
		if len(segments) > 0 {
			t.Fatal("unexpected non-empty meeting:minutes segments frame")
		}
	}
}

func TestMeetingCaptionsServerEntryPointsDisabledByDefault(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if websocket.IsWebSocketUpgrade(r) {
			http.Error(w, "unexpected ASR stream", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"code":0,"data":{"access_token":"test-token","access_token_expires_in":3600}}`)
	}))
	t.Cleanup(upstream.Close)
	wsBase := "ws" + strings.TrimPrefix(upstream.URL, "http") + "/stream"
	ts := newMeetingTestServerWithSetup(t, func(h *WebSocketHandler) {
		h.SetMeetingASRStreamService(NewMeetingASRStreamService(MeetingASRStreamConfig{
			HTTPBase: upstream.URL, WSBase: wsBase, Email: "test@example.test", Password: "test-password",
		}))
	})
	t.Cleanup(ts.Close)
	host := newWSRPC(t, ts.srv, "host:captions-disabled")
	roomID := meetingMinutesRoom(t, ts, host)
	meta, ok := ts.handler.activeMeetingRoom(roomID)
	if !ok {
		t.Fatal("meeting was not registered")
	}

	// A legacy subscription request must be rejected without publishing a
	// captions-active state or exposing ASR capability frames.
	sendMinutes(t, host, roomID, map[string]interface{}{"action": "caption-subscribe", "enabled": true})
	if message, err := host.waitError(3 * time.Second); err != nil || !strings.Contains(message, "unavailable") {
		t.Fatalf("caption subscribe should be rejected while disabled, message=%q err=%v", message, err)
	}
	meta.mu.Lock()
	if meta.minutes.CaptionActive || len(meta.captionViewers) != 0 {
		meta.mu.Unlock()
		t.Fatal("rejected subscribe must not activate caption state")
	}
	meta.mu.Unlock()
	assertNoMinutesKind(t, host, "captions", 150*time.Millisecond)
	assertNoMinutesKind(t, host, "asr-capabilities", 150*time.Millisecond)

	// Simulate stale state left by an older session. It must not authorize any
	// caption-only audio or text path after the feature is disabled.
	clientID, ok := ts.handler.meetings.clientForUniqID(roomID, "host:captions-disabled")
	if !ok {
		t.Fatal("host membership was not registered")
	}
	client, ok := ts.wsService.GetClient(clientID)
	if !ok || client == nil {
		t.Fatal("host client was not registered")
	}
	meta.mu.Lock()
	meta.captionViewers = map[string]bool{client.ID: true}
	meta.minutes.CaptionActive = true
	meta.minutes.Consented = map[string]bool{client.UniqID: true}
	meta.mu.Unlock()

	pcm := make([]byte, 4096)
	sendMinutes(t, host, roomID, map[string]interface{}{
		"action": "asr-chunk", "pcm": base64.StdEncoding.EncodeToString(pcm), "language": "en",
	})
	if _, err := waitMinutesKind(host, "asr-error", 3*time.Second); err != nil {
		t.Fatalf("legacy chunk must be rejected while captions are disabled: %v", err)
	}
	sendMinutes(t, host, roomID, map[string]interface{}{"action": "asr-stream", "event": "start", "language": "en"})
	if _, err := waitMinutesKind(host, "asr-stream-error", 3*time.Second); err != nil {
		t.Fatalf("legacy stream start must be rejected while captions are disabled: %v", err)
	}
	sendMinutes(t, host, roomID, map[string]interface{}{
		"action": "segment", "segmentId": "disabled-caption-segment", "text": "must not relay", "startMs": 0, "endMs": 10, "final": true,
	})
	if message, err := host.waitError(3 * time.Second); err != nil || !strings.Contains(message, "关闭") {
		t.Fatalf("legacy caption segment must be rejected while disabled, message=%q err=%v", message, err)
	}
	assertNoMinutesKind(t, host, "segments", 200*time.Millisecond)

	// Stop and unsubscribe remain valid cleanup operations. Seed a stale stream
	// state to prove the old stop action removes it without contacting Somni.
	ctx, cancel := context.WithCancel(context.Background())
	state := &meetingASRStreamState{key: asrStreamKey(roomID, client.ID), meta: meta, cancel: cancel, client: client}
	ts.handler.meetingASRStreams.Store(state.key, state)
	sendMinutes(t, host, roomID, map[string]interface{}{"action": "asr-stream", "event": "stop"})
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("legacy stream stop did not cancel the stale stream")
	}
	if _, ok := ts.handler.meetingASRStreams.Load(state.key); ok {
		t.Fatal("legacy stream stop did not remove the stale stream")
	}
	sendMinutes(t, host, roomID, map[string]interface{}{"action": "caption-subscribe", "enabled": false})
	if _, err := waitMinutesKind(host, "captions", 3*time.Second); err != nil {
		t.Fatalf("caption unsubscribe cleanup should remain available: %v", err)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("disabled caption requests reached Somni %d times", upstreamCalls.Load())
	}
}

func TestMeetingCaptionsLegacyRoutesCannotDuplicateActiveServerMix(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if websocket.IsWebSocketUpgrade(r) {
			http.Error(w, "unexpected per-speaker ASR stream", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"code":0,"data":{"access_token":"test-token","access_token_expires_in":3600}}`)
	}))
	t.Cleanup(upstream.Close)
	wsBase := "ws" + strings.TrimPrefix(upstream.URL, "http") + "/stream"
	ts, host, member, _, roomID := meetingChatFlowWithSetup(t, func(h *WebSocketHandler) {
		h.SetMeetingASRStreamService(NewMeetingASRStreamService(MeetingASRStreamConfig{
			HTTPBase: upstream.URL, WSBase: wsBase, Email: "test@example.test", Password: "test-password",
		}))
		h.SetMeetingAIService(NewMeetingAIService("test-only", "https://gemini.test/v1", true, true, nil))
	})
	if ts.handler.captionsEnabled {
		t.Fatal("custom meeting test server should use the production default: captions disabled")
	}
	configureAndStartMinutesWithReadyServices(t, host, roomID, host, member)

	value, ok := ts.handler.meetingAudioSessions.Load(roomID)
	if !ok {
		t.Fatal("ready host start did not install the server mixer")
	}
	session, ok := value.(*meetingAudioMinutesSession)
	if !ok || session == nil || session.closed.Load() {
		t.Fatal("server mixer is not active after host start")
	}
	meta, ok := ts.handler.activeMeetingRoom(roomID)
	if !ok {
		t.Fatal("meeting metadata is missing")
	}
	memberClient := meetingASRTestClient(t, ts, roomID, "bob:chat-2")
	// Recreate stale state an older caption client may have left behind. The
	// active mixer must remain the sole audio and transcript path regardless.
	meta.mu.Lock()
	meta.captionViewers = map[string]bool{memberClient.ID: true}
	meta.minutes.CaptionActive = true
	meta.minutes.Consented = map[string]bool{memberClient.UniqID: true}
	meta.mu.Unlock()

	pcm := make([]byte, 4096)
	sendMinutes(t, member, roomID, map[string]interface{}{
		"action": "asr-chunk", "pcm": base64.StdEncoding.EncodeToString(pcm), "language": "en",
	})
	if _, err := waitMinutesKind(member, "asr-error", 3*time.Second); err != nil {
		t.Fatalf("legacy chunk should be suppressed while server mixing is active: %v", err)
	}
	sendMinutes(t, member, roomID, map[string]interface{}{"action": "asr-stream", "event": "start", "language": "en"})
	if _, err := waitMinutesKind(member, "asr-stream-error", 3*time.Second); err != nil {
		t.Fatalf("legacy stream should be suppressed while server mixing is active: %v", err)
	}
	sendMinutes(t, member, roomID, map[string]interface{}{
		"action": "segment", "segmentId": "duplicate-caption-segment", "text": "must not duplicate mix", "startMs": 0, "endMs": 10, "final": true,
	})
	if message, err := member.waitError(3 * time.Second); err != nil || !strings.Contains(message, "Server mixed minutes") {
		t.Fatalf("legacy segment should be rejected while server mixing is active, message=%q err=%v", message, err)
	}
	assertNoNonEmptyMeetingMinutesSegments(t, host, 200*time.Millisecond)
	assertNoNonEmptyMeetingMinutesSegments(t, member, 200*time.Millisecond)
	if upstreamCalls.Load() != 0 {
		t.Fatalf("legacy caption routes reached Somni %d times during server mixing", upstreamCalls.Load())
	}
	if current, ok := ts.handler.meetingAudioSessions.Load(roomID); !ok || current != session || session.closed.Load() {
		t.Fatal("legacy caption requests interrupted the active server mixer")
	}
	meta.mu.Lock()
	running := meta.minutes.Running
	meta.mu.Unlock()
	if !running {
		t.Fatal("legacy caption requests stopped mixed minutes")
	}

	sendMinutes(t, host, roomID, map[string]interface{}{"action": "stop"})
	if _, err := waitMinutesKind(host, "subscription", 3*time.Second); err != nil {
		t.Fatal("personal stop was not acknowledged")
	}
	meta.mu.Lock()
	running = meta.minutes.Running
	meta.mu.Unlock()
	if !running {
		t.Fatal("personal stop changed room-wide mixed recording state")
	}
	if current, ok := ts.handler.meetingAudioSessions.Load(roomID); !ok || current != session || session.closed.Load() {
		t.Fatal("personal stop interrupted the room mixer")
	}
}
