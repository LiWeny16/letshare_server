package handler

import (
	"encoding/json"
	"letshare-server/internal/model"
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
