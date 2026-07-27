package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandlerRefreshesReadDeadlineAfterAnyClientMessage(t *testing.T) {
	sourceBytes, err := os.ReadFile(filepath.Join("..", "handler", "websocket.go"))
	if err != nil {
		t.Fatalf("read websocket handler: %v", err)
	}
	source := string(sourceBytes)

	start := strings.Index(source, "func (h *WebSocketHandler) handleMessages")
	if start < 0 {
		t.Fatal("handleMessages should exist")
	}
	body := source[start:]

	readIndex := strings.Index(body, "conn.ReadMessage()")
	if readIndex < 0 {
		t.Fatal("handleMessages should read websocket messages")
	}
	activeIndex := strings.Index(body[readIndex:], "client.LastPing = time.Now()")
	if activeIndex < 0 {
		t.Fatal("handleMessages should update LastPing after successful reads")
	}
	afterActive := body[readIndex+activeIndex:]
	deadlineIndex := strings.Index(afterActive, "conn.SetReadDeadline(time.Now().Add(websocketReadTimeout))")
	if deadlineIndex < 0 || deadlineIndex > 200 {
		t.Fatal("successful client messages should refresh the websocket read deadline with the 120s mobile/background tolerance")
	}
}
