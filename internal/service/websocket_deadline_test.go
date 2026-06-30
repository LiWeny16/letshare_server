package service

import (
	"bytes"
	"encoding/json"
	"letshare-server/internal/model"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func newDeadlineTestClient(t *testing.T, ws *WebSocketService, clientID, userID, room string) (*websocket.Conn, *websocket.Conn) {
	t.Helper()

	serverConnCh := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		serverConnCh <- conn
	}))

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("dial failed: %v", err)
	}

	var serverConn *websocket.Conn
	select {
	case serverConn = <-serverConnCh:
	case <-time.After(time.Second):
		clientConn.Close()
		srv.Close()
		t.Fatal("timed out waiting for server websocket")
	}

	ws.AddClient(&model.Client{
		ID:         clientID,
		UserID:     userID,
		Connection: serverConn,
		Rooms:      map[string]bool{room: true},
		Events:     map[string]bool{"signal:all": true},
		LastPing:   time.Now(),
		Metadata:   map[string]interface{}{},
	})

	t.Cleanup(func() {
		ws.RemoveClient(clientID)
		clientConn.Close()
		serverConn.Close()
		srv.Close()
	})

	return clientConn, serverConn
}

func TestSendMessageToUserRefreshesExpiredWriteDeadline(t *testing.T) {
	ws := NewWebSocketService(10)
	fts := NewFileTransferService(ws, 500*1024*1024, 65536, 50*1024*1024, "")
	clientConn, serverConn := newDeadlineTestClient(t, ws, "receiver-client", "receiver-user", "room1")

	if err := serverConn.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("set stale deadline: %v", err)
	}

	msg := model.NewWebSocketMessage("probe", "room1", "", map[string]string{"ok": "true"})
	if err := fts.SendMessageToUser("receiver-user", "room1", msg); err != nil {
		t.Fatalf("SendMessageToUser should refresh stale write deadline: %v", err)
	}

	clientConn.SetReadDeadline(time.Now().Add(time.Second))
	_, raw, err := clientConn.ReadMessage()
	if err != nil {
		t.Fatalf("read message: %v", err)
	}

	var got model.WebSocketMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	if got.Type != "probe" {
		t.Fatalf("message type = %q, want probe", got.Type)
	}
}

func TestForwardChunkToReceiverRefreshesExpiredWriteDeadline(t *testing.T) {
	ws := NewWebSocketService(10)
	fts := NewFileTransferService(ws, 500*1024*1024, 65536, 50*1024*1024, "")
	senderConn, _ := newDeadlineTestClient(t, ws, "sender-client", "sender-user", "room1")
	receiverConn, receiverServerConn := newDeadlineTestClient(t, ws, "receiver-client", "receiver-user", "room1")

	session, err := fts.CreateTransferSession(&model.FileTransferRequest{
		TransferID: "transfer-deadline",
		FileName:   "deadline.bin",
		FileSize:   6,
		FileType:   "application/octet-stream",
		ChunkSize:  6,
		FromUserID: "sender-user",
		ToUserID:   "receiver-user",
		RoomName:   "room1",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := fts.UpdateSessionClients(session.TransferID, "sender-client", "receiver-client"); err != nil {
		t.Fatalf("update clients: %v", err)
	}

	chunk := &model.FileTransferChunk{
		TransferID:  session.TransferID,
		ChunkIndex:  0,
		ChunkSize:   6,
		TotalChunks: 1,
	}
	payload := []byte("ABCDEF")
	meta, _ := json.Marshal(chunk)
	frame := make([]byte, 256+len(payload))
	copy(frame[:256], meta)
	copy(frame[256:], payload)

	if err := receiverServerConn.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("set stale deadline: %v", err)
	}

	if err := fts.ForwardChunkToReceiver(session.TransferID, frame, len(payload), chunk); err != nil {
		t.Fatalf("ForwardChunkToReceiver should refresh stale write deadline: %v", err)
	}

	receiverConn.SetReadDeadline(time.Now().Add(time.Second))
	messageType, raw, err := receiverConn.ReadMessage()
	if err != nil {
		t.Fatalf("read forwarded chunk: %v", err)
	}
	if messageType != websocket.BinaryMessage {
		t.Fatalf("message type = %d, want binary", messageType)
	}
	if !bytes.Equal(raw, frame) {
		t.Fatalf("forwarded frame mismatch")
	}

	senderConn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := senderConn.ReadMessage(); err != nil {
		t.Fatalf("sender should receive progress after forwarded chunk: %v", err)
	}
}
