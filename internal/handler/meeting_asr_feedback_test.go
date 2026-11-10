package handler

import (
	"strings"
	"testing"
	"time"
)

// A deployment without recognizer credentials used to swallow every audio
// frame, so the caption bar waited forever with no text and no error. The
// speaker must be told explicitly which engine is unavailable.
func TestMeetingASR_StreamUnavailableIsReported(t *testing.T) {
	ts := newMeetingTestServerWithCaptions(t)
	t.Cleanup(ts.Close)

	host := newWSRPC(t, ts.srv, "host:asr-fb-1")
	roomID := meetingMinutesRoom(t, ts, host)
	subscribeCaption(t, host, roomID, true)
	if _, err := waitMinutesKind(host, "captions", 3*time.Second); err != nil {
		t.Fatal(err)
	}

	sendMinutes(t, host, roomID, map[string]interface{}{
		"action": "asr-stream", "event": "start", "language": "zh",
	})
	frame, err := waitMinutesKind(host, "asr-error", 3*time.Second)
	if err != nil {
		t.Fatalf("streaming ASR without a configured provider must answer with asr-error: %v", err)
	}
	payload := minutesPayload(t, frame)
	if payload["engine"] != "server-stream" {
		t.Fatalf("asr-error must name the engine, got %#v", payload)
	}
	if message, _ := payload["message"].(string); !strings.Contains(message, "Somni") {
		t.Fatalf("asr-error must name the active Somni service, got %q", message)
	}
}

// The same silence existed on the chunked path: one server chunk per six
// seconds was dropped without ever telling the speaker.
func TestMeetingASR_ChunkUnavailableIsReported(t *testing.T) {
	ts := newMeetingTestServerWithCaptions(t)
	t.Cleanup(ts.Close)

	host := newWSRPC(t, ts.srv, "host:asr-fb-2")
	roomID := meetingMinutesRoom(t, ts, host)
	subscribeCaption(t, host, roomID, true)
	if _, err := waitMinutesKind(host, "captions", 3*time.Second); err != nil {
		t.Fatal(err)
	}

	sendMinutes(t, host, roomID, map[string]interface{}{
		"action": "asr-chunk", "pcm": strings.Repeat("A", 8192), "language": "zh",
	})
	frame, err := waitMinutesKind(host, "asr-error", 3*time.Second)
	if err != nil {
		t.Fatalf("chunked ASR without a configured provider must answer with asr-error: %v", err)
	}
	if payload := minutesPayload(t, frame); payload["engine"] != "server-asr" {
		t.Fatalf("asr-error must name the chunked engine, got %#v", payload)
	}
}

// Clients need to know which server engines exist before they pick one. Both
// wire paths are backed by the same Somni recognizer.
func TestMeetingASR_CapabilitiesFollowSomniConfiguration(t *testing.T) {
	ts := newMeetingTestServerWithCaptions(t)
	t.Cleanup(ts.Close)

	host := newWSRPC(t, ts.srv, "host:asr-fb-3")
	roomID := meetingMinutesRoom(t, ts, host)

	subscribeCaption(t, host, roomID, true)
	frame, err := waitMinutesKind(host, "asr-capabilities", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	payload := minutesPayload(t, frame)
	for _, key := range []string{"stream", "chunk", "ai"} {
		if payload[key] != false {
			t.Fatalf("unconfigured %s capability must report false, got %#v", key, payload)
		}
	}

	// A fully configured Somni recognizer enables both wire contracts.
	ts.handler.SetMeetingASRStreamService(NewMeetingASRStreamService(MeetingASRStreamConfig{
		HTTPBase: "https://somni.test", WSBase: "wss://somni.test", Email: "test@example.test", Password: "test-password",
	}))
	subscribeCaption(t, host, roomID, true)
	frame, err = waitMinutesKind(host, "asr-capabilities", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	payload = minutesPayload(t, frame)
	if payload["chunk"] != true || payload["stream"] != true {
		t.Fatalf("both capabilities must use the attached Somni service, got %#v", payload)
	}
}
