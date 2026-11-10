package handler

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestMeetingASRLegacyChunkUsesSomni(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/login", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"somni-test-token","access_token_expires_in":3600}}`))
	})
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var start map[string]interface{}
		if json.Unmarshal(payload, &start) != nil || start["event"] != "start" {
			t.Errorf("Somni session did not start with the expected event: %s", payload)
			return
		}
		if err := conn.WriteJSON(map[string]interface{}{"event": "started"}); err != nil {
			return
		}
		binaryFrames := 0
		for {
			messageType, payload, readErr := conn.ReadMessage()
			if readErr != nil {
				return
			}
			if messageType == websocket.BinaryMessage {
				binaryFrames++
				continue
			}
			var event map[string]interface{}
			if json.Unmarshal(payload, &event) != nil || event["event"] != "stop" {
				continue
			}
			if binaryFrames == 0 {
				t.Error("legacy chunk request did not upload PCM to Somni")
			}
			_ = conn.WriteJSON(map[string]interface{}{
				"event": "result",
				"data":  map[string]interface{}{"text": "Somni transcript", "isFinal": true},
			})
			_ = conn.WriteJSON(map[string]interface{}{"event": "stopped"})
			return
		}
	})
	upstream := httptest.NewServer(mux)
	t.Cleanup(upstream.Close)
	wsBase := "ws" + strings.TrimPrefix(upstream.URL, "http") + "/stream"

	ts, host, _, _, roomID := meetingChatFlow(t)
	ts.handler.SetMeetingASRStreamService(NewMeetingASRStreamService(MeetingASRStreamConfig{
		HTTPBase: upstream.URL, WSBase: wsBase, Email: "test@example.test", Password: "test",
	}))
	subscribeCaption(t, host, roomID, true)
	if _, err := waitMinutesKind(host, "captions", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := waitMinutesKind(host, "asr-capabilities", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	sendMinutes(t, host, roomID, map[string]interface{}{"action": "consent", "accepted": true})
	if _, err := waitMinutesKind(host, "consent", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	pcm := make([]byte, 4096)
	sendMinutes(t, host, roomID, map[string]interface{}{
		"action": "asr-chunk", "pcm": base64.StdEncoding.EncodeToString(pcm), "language": "en",
	})
	frame, err := waitMinutesKind(host, "segments", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	segments := segmentsFromBatch(t, frame)
	if len(segments) != 1 || segments[0]["text"] != "Somni transcript" || segments[0]["language"] != "en-US" {
		t.Fatalf("legacy chunk request did not return Somni text: %#v", segments)
	}
}
