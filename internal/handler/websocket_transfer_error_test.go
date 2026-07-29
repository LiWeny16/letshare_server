package handler

import (
	"encoding/json"
	"letshare-server/internal/model"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNotifyTransferErrorBypassesGenericRateLimiter(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	sender := dialTestClient(t, ts.srv, "sender-rate-limited")
	receiver := dialTestClient(t, ts.srv, "receiver-rate-limited")
	defer sender.Close()
	defer receiver.Close()

	subscribeRoom(t, sender, "RL")
	subscribeRoom(t, receiver, "RL")

	session, err := ts.fts.CreateTransferSession(&model.FileTransferRequest{
		TransferID:  "tf-rate-limit-bypass",
		FileName:    "rate.bin",
		FileSize:    6,
		FileType:    "application/octet-stream",
		ChunkSize:   6,
		FromUserID:  "sender-rate-limited",
		ToUserID:    "receiver-rate-limited",
		RoomName:    "RL",
		TotalChunks: 1,
	}, false)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := ts.fts.UpdateSessionClients(session.TransferID, "client-sender-rate-limited", "client-receiver-rate-limited"); err != nil {
		t.Fatalf("update clients: %v", err)
	}

	for i := 0; i < 4; i++ {
		ts.handler.errorRateLimiter.Allow("client-sender-rate-limited")
	}

	ts.handler.notifyTransferError(session, "数据转发失败")

	sender.SetReadDeadline(time.Now().Add(time.Second))
	_, raw, err := sender.ReadMessage()
	if err != nil {
		t.Fatalf("sender should receive protocol-critical transfer error despite generic limiter: %v", err)
	}

	var msg model.WebSocketMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("decode transfer error: %v", err)
	}
	if msg.Type != model.MessageTypeFileTransferError {
		t.Fatalf("message type = %s, want %s", msg.Type, model.MessageTypeFileTransferError)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(msg.Data, &payload); err != nil {
		t.Fatalf("decode transfer error payload: %v", err)
	}
	if payload["transfer_id"] != session.TransferID {
		t.Fatalf("transfer_id = %v, want %s", payload["transfer_id"], session.TransferID)
	}
}

func TestParseRequestedChunkIndexesValidatesBoundsAndCount(t *testing.T) {
	indexes, err := parseRequestedChunkIndexes(
		[]interface{}{float64(0), float64(2), float64(2)},
		float64(2),
		3,
	)
	if err != nil {
		t.Fatalf("valid chunk indexes: %v", err)
	}
	if !reflect.DeepEqual(indexes, []int{0, 2}) {
		t.Fatalf("indexes = %v, want [0 2]", indexes)
	}

	tooMany := make([]interface{}, maxRelayResendChunkIndexes+1)
	for i := range tooMany {
		tooMany[i] = float64(0)
	}

	tests := []struct {
		name         string
		value        interface{}
		missingCount interface{}
		totalChunks  int
		want         string
	}{
		{name: "negative index", value: []interface{}{float64(-1)}, missingCount: float64(1), totalChunks: 3, want: "out of range"},
		{name: "fractional index", value: []interface{}{1.5}, missingCount: float64(1), totalChunks: 3, want: "finite integer"},
		{name: "out of range", value: []interface{}{float64(3)}, missingCount: float64(1), totalChunks: 3, want: "out of range"},
		{name: "too many", value: tooMany, missingCount: float64(len(tooMany)), totalChunks: 3, want: "exceeds limit"},
		{name: "missing count mismatch", value: []interface{}{float64(0), float64(1)}, missingCount: float64(1), totalChunks: 3, want: "missing_count mismatch"},
		{name: "invalid missing count", value: []interface{}{float64(0)}, missingCount: "1", totalChunks: 3, want: "invalid missing_count"},
		{name: "empty array", value: []interface{}{}, missingCount: float64(0), totalChunks: 3, want: "cannot be empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseRequestedChunkIndexes(tt.value, tt.missingCount, tt.totalChunks)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected error containing %q, got %v", tt.want, err)
			}
		})
	}
}
