package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"letshare-server/internal/model"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWriteErrorClassificationTreatsResetConnectionsAsClosed(t *testing.T) {
	cases := []error{
		errors.New("write tcp 172.21.200.75:443->58.22.7.114:52290: write: connection reset by peer"),
		errors.New("write tcp 127.0.0.1:443->127.0.0.1:50000: wsasend: An established connection was aborted by the software in your host machine."),
		errors.New("write tcp 127.0.0.1:443->127.0.0.1:50000: write: broken pipe"),
		errors.New("i/o timeout"),
	}

	for _, err := range cases {
		if !isConnClosedError(err) {
			t.Fatalf("expected %q to be treated as a closed/unusable websocket", err)
		}
	}
}

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
	fts := NewFileTransferService(ws, 3*1024*1024*1024, 65536)
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

func TestSendMessageToUserRemovesClosedClient(t *testing.T) {
	ws := NewWebSocketService(10)
	fts := NewFileTransferService(ws, 3*1024*1024*1024, 65536)
	_, serverConn := newDeadlineTestClient(t, ws, "receiver-closed-client", "receiver-closed-user", "room1")

	serverConn.Close()

	msg := model.NewWebSocketMessage("probe", "room1", "", map[string]string{"ok": "true"})
	if err := fts.SendMessageToUser("receiver-closed-user", "room1", msg); err == nil {
		t.Fatal("SendMessageToUser should fail when the selected websocket is closed")
	}
	if _, exists := ws.GetClient("receiver-closed-client"); exists {
		t.Fatal("closed websocket client should be removed after write failure")
	}
}

func TestSendMessageToUserRetriesSameUserAfterClosedClient(t *testing.T) {
	ws := NewWebSocketService(10)
	fts := NewFileTransferService(ws, 3*1024*1024*1024, 65536)
	_, closedServerConn := newDeadlineTestClient(t, ws, "receiver-newer-closed", "receiver-retry-user", "room1")
	activeConn, _ := newDeadlineTestClient(t, ws, "receiver-older-active", "receiver-retry-user", "room1")

	if client, exists := ws.GetClient("receiver-newer-closed"); exists {
		client.LastPing = time.Now().Add(time.Second)
	}
	if client, exists := ws.GetClient("receiver-older-active"); exists {
		client.LastPing = time.Now()
	}
	closedServerConn.Close()

	msg := model.NewWebSocketMessage("probe", "room1", "", map[string]string{"ok": "true"})
	if err := fts.SendMessageToUser("receiver-retry-user", "room1", msg); err != nil {
		t.Fatalf("SendMessageToUser should retry another same-user client after closed write: %v", err)
	}

	activeConn.SetReadDeadline(time.Now().Add(time.Second))
	_, raw, err := activeConn.ReadMessage()
	if err != nil {
		t.Fatalf("active same-user client should receive retried message: %v", err)
	}
	var got model.WebSocketMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode retried message: %v", err)
	}
	if got.Type != "probe" {
		t.Fatalf("message type = %q, want probe", got.Type)
	}
	if _, exists := ws.GetClient("receiver-newer-closed"); exists {
		t.Fatal("closed same-user client should be removed after retry")
	}
}

func TestForwardChunkToReceiverRefreshesExpiredWriteDeadline(t *testing.T) {
	ws := NewWebSocketService(10)
	fts := NewFileTransferService(ws, 3*1024*1024*1024, 65536)
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
	}, false)
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

func TestForwardChunkToReceiverRemovesClosedReceiverClient(t *testing.T) {
	ws := NewWebSocketService(10)
	fts := NewFileTransferService(ws, 3*1024*1024*1024, 65536)
	_, _ = newDeadlineTestClient(t, ws, "sender-client", "sender-user", "room1")
	_, receiverServerConn := newDeadlineTestClient(t, ws, "receiver-closed-client", "receiver-user", "room1")

	session, err := fts.CreateTransferSession(&model.FileTransferRequest{
		TransferID:  "transfer-closed-receiver",
		FileName:    "closed.bin",
		FileSize:    6,
		FileType:    "application/octet-stream",
		ChunkSize:   6,
		FromUserID:  "sender-user",
		ToUserID:    "receiver-user",
		RoomName:    "room1",
		TotalChunks: 1,
	}, false)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := fts.UpdateSessionClients(session.TransferID, "sender-client", "receiver-closed-client"); err != nil {
		t.Fatalf("update clients: %v", err)
	}
	if err := fts.UpdateSessionStatus(session.TransferID, "pending", "accepted"); err != nil {
		t.Fatalf("accept session: %v", err)
	}
	if err := fts.UpdateSessionStatus(session.TransferID, "accepted", "transferring"); err != nil {
		t.Fatalf("start session: %v", err)
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

	receiverServerConn.Close()

	if err := fts.ForwardChunkToReceiver(session.TransferID, frame, len(payload), chunk); err == nil {
		t.Fatal("ForwardChunkToReceiver should fail when receiver websocket is closed")
	}
	if _, exists := ws.GetClient("receiver-closed-client"); exists {
		t.Fatal("closed receiver websocket should be removed after binary write failure")
	}
}

func TestForwardChunkToReceiverRebindsClosedAcceptedClient(t *testing.T) {
	ws := NewWebSocketService(10)
	fts := NewFileTransferService(ws, 3*1024*1024*1024, 65536)
	_, _ = newDeadlineTestClient(t, ws, "sender-client", "sender-user", "room1")
	_, closedReceiverServerConn := newDeadlineTestClient(t, ws, "receiver-accepted-closed", "receiver-user", "room1")
	activeReceiverConn, _ := newDeadlineTestClient(t, ws, "receiver-active-new", "receiver-user", "room1")
	senderConn, _ := newDeadlineTestClient(t, ws, "sender-progress-client", "sender-user", "room1")
	_ = senderConn

	session, err := fts.CreateTransferSession(&model.FileTransferRequest{
		TransferID:  "transfer-rebind-receiver",
		FileName:    "rebind.bin",
		FileSize:    6,
		FileType:    "application/octet-stream",
		ChunkSize:   6,
		FromUserID:  "sender-user",
		ToUserID:    "receiver-user",
		RoomName:    "room1",
		TotalChunks: 1,
	}, false)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := fts.UpdateSessionClients(session.TransferID, "sender-client", "receiver-accepted-closed"); err != nil {
		t.Fatalf("update clients: %v", err)
	}
	if err := fts.UpdateSessionStatus(session.TransferID, "pending", "accepted"); err != nil {
		t.Fatalf("accept session: %v", err)
	}
	if err := fts.UpdateSessionStatus(session.TransferID, "accepted", "transferring"); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if client, exists := ws.GetClient("receiver-active-new"); exists {
		client.LastPing = time.Now().Add(time.Second)
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

	closedReceiverServerConn.Close()

	if err := fts.ForwardChunkToReceiver(session.TransferID, frame, len(payload), chunk); err != nil {
		t.Fatalf("ForwardChunkToReceiver should rebind to active same-user receiver after closed accepted client: %v", err)
	}

	activeReceiverConn.SetReadDeadline(time.Now().Add(time.Second))
	messageType, raw, err := activeReceiverConn.ReadMessage()
	if err != nil {
		t.Fatalf("active receiver should receive rebound chunk: %v", err)
	}
	if messageType != websocket.BinaryMessage {
		t.Fatalf("message type = %d, want binary", messageType)
	}
	if !bytes.Equal(raw, frame) {
		t.Fatalf("rebound frame mismatch")
	}
	updated, err := fts.GetSession(session.TransferID)
	if err != nil {
		t.Fatalf("get updated session: %v", err)
	}
	if updated.ToClientID != "receiver-active-new" {
		t.Fatalf("ToClientID = %s, want receiver-active-new", updated.ToClientID)
	}
}

func TestForwardChunkToReceiverRebindsClosedAcceptedClientWithDisconnectCallback(t *testing.T) {
	ws := NewWebSocketService(10)
	fts := NewFileTransferService(ws, 3*1024*1024*1024, 65536)
	ws.SetOnClientDisconnect(func(clientID string) {
		fts.HandleClientDisconnect(clientID)
	})

	_, _ = newDeadlineTestClient(t, ws, "sender-client", "sender-user", "room1")
	_, closedReceiverServerConn := newDeadlineTestClient(t, ws, "receiver-accepted-closed", "receiver-user", "room1")
	activeReceiverConn, _ := newDeadlineTestClient(t, ws, "receiver-active-new", "receiver-user", "room1")

	session, err := fts.CreateTransferSession(&model.FileTransferRequest{
		TransferID:  "transfer-rebind-receiver-with-callback",
		FileName:    "rebind-callback.bin",
		FileSize:    6,
		FileType:    "application/octet-stream",
		ChunkSize:   6,
		FromUserID:  "sender-user",
		ToUserID:    "receiver-user",
		RoomName:    "room1",
		TotalChunks: 1,
	}, false)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := fts.UpdateSessionClients(session.TransferID, "sender-client", "receiver-accepted-closed"); err != nil {
		t.Fatalf("update clients: %v", err)
	}
	if err := fts.UpdateSessionStatus(session.TransferID, "pending", "accepted"); err != nil {
		t.Fatalf("accept session: %v", err)
	}
	if err := fts.UpdateSessionStatus(session.TransferID, "accepted", "transferring"); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if client, exists := ws.GetClient("receiver-active-new"); exists {
		client.LastPing = time.Now().Add(time.Second)
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

	closedReceiverServerConn.Close()

	if err := fts.ForwardChunkToReceiver(session.TransferID, frame, len(payload), chunk); err != nil {
		t.Fatalf("ForwardChunkToReceiver should rebind to active same-user receiver after closed accepted client: %v", err)
	}

	activeReceiverConn.SetReadDeadline(time.Now().Add(time.Second))
	messageType, raw, err := activeReceiverConn.ReadMessage()
	if err != nil {
		t.Fatalf("active receiver should receive rebound chunk: %v", err)
	}
	if messageType != websocket.BinaryMessage {
		t.Fatalf("message type = %d, want binary", messageType)
	}
	if !bytes.Equal(raw, frame) {
		t.Fatalf("rebound frame mismatch")
	}

	updated, err := fts.GetSession(session.TransferID)
	if err != nil {
		t.Fatalf("get updated session: %v", err)
	}
	if updated.Status != "transferring" {
		t.Fatalf("Status = %s, want transferring", updated.Status)
	}
	if updated.ToClientID != "receiver-active-new" {
		t.Fatalf("ToClientID = %s, want receiver-active-new", updated.ToClientID)
	}
}

func TestReplaySpoolChunksToReceiverReplaysAvailableChunks(t *testing.T) {
	ws := NewWebSocketService(10)
	fts := NewFileTransferService(ws, 3*1024*1024*1024, 65536)
	fts.spoolDir = t.TempDir()
	receiverConn, _ := newDeadlineTestClient(t, ws, "receiver-client", "receiver-user", "room1")

	session, err := fts.CreateTransferSession(&model.FileTransferRequest{
		TransferID: "transfer-spool-replay",
		FileName:   "replay.bin",
		FileSize:   12,
		FileType:   "application/octet-stream",
		ChunkSize:  6,
		FromUserID: "sender-user",
		ToUserID:   "receiver-user",
		RoomName:   "room1",
	}, false)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := fts.UpdateSessionClients(session.TransferID, "sender-client", "receiver-client"); err != nil {
		t.Fatalf("update clients: %v", err)
	}
	if err := fts.UpdateSessionStatus(session.TransferID, "pending", "transferring"); err != nil {
		t.Fatalf("start session: %v", err)
	}

	chunk := &model.FileTransferChunk{
		TransferID:  session.TransferID,
		ChunkIndex:  0,
		ChunkSize:   6,
		TotalChunks: 2,
	}
	payload := []byte("ABCDEF")
	meta, _ := json.Marshal(chunk)
	frame := make([]byte, 256+len(payload))
	copy(frame[:256], meta)
	copy(frame[256:], payload)

	updated, err := fts.GetSession(session.TransferID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if _, err := fts.recordChunkReceipt(updated, chunk, frame, len(payload)); err != nil {
		t.Fatalf("record chunk: %v", err)
	}

	replayed, missing, err := fts.ReplaySpoolChunksToReceiver(session.TransferID, []int{0, 1})
	if err != nil {
		t.Fatalf("replay spool chunks: %v", err)
	}
	if len(replayed) != 1 || replayed[0] != 0 {
		t.Fatalf("replayed = %v, want [0]", replayed)
	}
	if len(missing) != 1 || missing[0] != 1 {
		t.Fatalf("missing = %v, want [1]", missing)
	}

	receiverConn.SetReadDeadline(time.Now().Add(time.Second))
	messageType, raw, err := receiverConn.ReadMessage()
	if err != nil {
		t.Fatalf("receiver should receive replayed chunk: %v", err)
	}
	if messageType != websocket.BinaryMessage {
		t.Fatalf("message type = %d, want binary", messageType)
	}
	if !bytes.Equal(raw, frame) {
		t.Fatalf("replayed frame mismatch")
	}
}
