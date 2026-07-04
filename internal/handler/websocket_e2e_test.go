package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"letshare-server/internal/model"
	"letshare-server/internal/service"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func testToken() string {
	hash := sha256.Sum256([]byte("sever_auth_123"))
	return hex.EncodeToString(hash[:])
}

// testServer tests the WebSocket handler by upgrading connections directly
// and testing message processing through the handler
type testServer struct {
	srv       *httptest.Server
	wsService *service.WebSocketService
	fts       *service.FileTransferService
	handler   *WebSocketHandler
	authSvc   *service.AuthService
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()

	wsService := service.NewWebSocketService(10)
	authService := service.NewAuthService()
	fts := service.NewFileTransferService(wsService, 3*1024*1024*1024, 65536)

	wsService.SetOnClientDisconnect(func(clientID string) {
		fts.HandleClientDisconnect(clientID)
	})

	handler := NewWebSocketHandler(wsService, authService, fts, nil)

	ts := &testServer{
		wsService: wsService,
		fts:       fts,
		handler:   handler,
		authSvc:   authService,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		userID := r.URL.Query().Get("userId")

		if token == "" {
			http.Error(w, "missing token", 401)
			return
		}
		if err := authService.ValidateAuthToken(token); err != nil {
			http.Error(w, "invalid token", 401)
			return
		}

		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("upgrade failed: %v", err)
			return
		}

		clientID := fmt.Sprintf("client-%s", userID)
		client := model.NewClient(clientID, userID, conn)
		client.Metadata["authenticated"] = true
		wsService.AddClient(client)

		// Set a short write deadline for conn
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))

		go func() {
			defer func() {
				recover()
				conn.Close()
				wsService.RemoveClient(clientID)
			}()
			for {
				messageType, data, err := conn.ReadMessage()
				if err != nil {
					return
				}
				client.LastPing = time.Now()

				switch messageType {
				case websocket.TextMessage:
					var msg model.WebSocketMessage
					if err := json.Unmarshal(data, &msg); err != nil {
						continue
					}
					ts.dispatchMessage(client, &msg)
				case websocket.BinaryMessage:
					ts.dispatchBinaryMessage(client, data)
				}
			}
		}()
	}))

	ts.srv = srv
	return ts
}

func (ts *testServer) dispatchMessage(client *model.Client, msg *model.WebSocketMessage) {
	switch msg.Type {
	case model.MessageTypeSubscribe:
		ts.subscribeAndConfirm(client, msg)
	case model.MessageTypePublish:
		if msg.Channel != "" {
			event := msg.Event
			if event == "" {
				event = "signal:all"
			}
			ts.wsService.PublishToRoom(client.ID, msg.Channel, event, msg.Data)
		}
	// File transfer messages — delegate to handler methods
	case model.MessageTypeFileTransferRequest:
		ts.sendFileTransferMessage(client, msg, "request")
	case model.MessageTypeFileTransferAccept:
		ts.sendFileTransferMessage(client, msg, "accept")
	case model.MessageTypeFileTransferReject:
		ts.sendFileTransferMessage(client, msg, "reject")
	case model.MessageTypeFileTransferStart:
		ts.sendFileTransferMessage(client, msg, "start")
	case model.MessageTypeFileTransferEnd:
		ts.sendFileTransferMessage(client, msg, "end")
	case model.MessageTypeFileTransferComplete:
		ts.sendFileTransferMessage(client, msg, "complete")
	case model.MessageTypeFileTransferResend:
		ts.sendFileTransferMessage(client, msg, "resend")
	case model.MessageTypeFileTransferCancel:
		ts.sendFileTransferMessage(client, msg, "cancel")
	}
}

func (ts *testServer) dispatchBinaryMessage(client *model.Client, data []byte) {
	// Simplified version of processBinaryMessage
	if len(data) < 256 {
		ts.sendError(client, 400, "binary too short")
		return
	}

	var chunkMeta model.FileTransferChunk
	metaBytes := make([]byte, 256)
	copy(metaBytes, data[:256])
	// Trim trailing zeros
	trimmed := strings.TrimRight(string(metaBytes), "\x00")
	if err := json.Unmarshal([]byte(trimmed), &chunkMeta); err != nil {
		ts.sendError(client, 400, "parse chunk meta failed: "+err.Error())
		return
	}

	ts.handleChunk(client, &chunkMeta, data[256:], data)
}

// handleChunk — mirrors handler.handleFileChunk
func (ts *testServer) handleChunk(client *model.Client, chunkMeta *model.FileTransferChunk, chunkData []byte, framedData []byte) {
	session, err := ts.fts.GetSession(chunkMeta.TransferID)
	if err != nil {
		ts.sendError(client, 404, "session not found")
		return
	}

	if session.FromUserID != client.UserID {
		ts.sendError(client, 403, "not sender")
		return
	}

	// Bug#1 fix: allow resending state
	if session.Status == "completed" {
		return
	}
	if session.Status != "transferring" && session.Status != "resending" {
		ts.sendError(client, 400, "bad status: "+session.Status)
		return
	}

	if err := ts.fts.ForwardChunkToReceiver(chunkMeta.TransferID, framedData, len(chunkData), chunkMeta); err != nil {
		ts.sendError(client, 500, "forward failed: "+err.Error())
	}
}

// sendFileTransferMessage dispatches file transfer messages matching handler logic
func (ts *testServer) sendFileTransferMessage(client *model.Client, msg *model.WebSocketMessage, msgType string) {
	var data map[string]interface{}
	if err := json.Unmarshal(msg.Data, &data); err != nil {
		ts.sendError(client, 400, "json parse error")
		return
	}

	transferID, _ := data["transfer_id"].(string)

	switch msgType {
	case "request":
		var req model.FileTransferRequest
		json.Unmarshal(msg.Data, &req)
		req.FromUserID = client.UserID
		session, err := ts.fts.CreateTransferSession(&req, false)
		if err != nil {
			ts.sendError(client, 400, err.Error())
			return
		}
		ts.fts.UpdateSessionClients(session.TransferID, client.ID, "")
		// Forward to receiver
		fwdMsg := model.NewWebSocketMessage(model.MessageTypeFileTransferRequest, req.RoomName, "", req)
		ts.fts.SendMessageToUser(req.ToUserID, req.RoomName, fwdMsg)

	case "accept":
		session, err := ts.fts.GetSession(transferID)
		if err != nil {
			ts.sendError(client, 404, "session not found")
			return
		}
		if session.ToUserID != client.UserID {
			ts.sendError(client, 403, "not receiver")
			return
		}
		if session.Status != "pending" {
			ts.sendError(client, 400, "bad status: "+session.Status)
			return
		}
		ts.fts.UpdateSessionStatus(transferID, "pending", "accepted")
		ts.fts.UpdateSessionClients(transferID, session.FromClientID, client.ID)
		fwdMsg := model.NewWebSocketMessage(model.MessageTypeFileTransferAccept, session.RoomName, "", map[string]interface{}{"transfer_id": transferID})
		ts.fts.SendMessageToUser(session.FromUserID, session.RoomName, fwdMsg)

	case "start":
		session, err := ts.fts.GetSession(transferID)
		if err != nil {
			ts.sendError(client, 404, "session not found")
			return
		}
		if session.FromUserID != client.UserID {
			ts.sendError(client, 403, "not sender")
			return
		}
		if session.Status != "accepted" && session.Status != "resending" {
			ts.sendError(client, 400, "bad status: "+session.Status)
			return
		}
		ts.fts.UpdateSessionStatus(transferID, session.Status, "transferring")
		fwdMsg := model.NewWebSocketMessage(model.MessageTypeFileTransferStart, session.RoomName, "", map[string]interface{}{
			"transfer_id": transferID, "file_name": session.FileName,
			"file_size": session.FileSize, "total_chunks": session.TotalChunks,
		})
		ts.fts.SendMessageToUser(session.ToUserID, session.RoomName, fwdMsg)

	case "end":
		session, err := ts.fts.GetSession(transferID)
		if err != nil {
			ts.sendError(client, 404, "session not found")
			return
		}
		if session.FromUserID != client.UserID {
			ts.sendError(client, 403, "not sender")
			return
		}
		if session.Status == "completed" {
			return
		}
		if session.Status != "transferring" && session.Status != "resending" {
			ts.sendError(client, 400, "bad status: "+session.Status)
			return
		}
		ts.fts.UpdateSessionStatus(transferID, session.Status, "ending")
		fwdMsg := model.NewWebSocketMessage(model.MessageTypeFileTransferEnd, session.RoomName, "", map[string]interface{}{
			"transfer_id": transferID, "file_name": session.FileName, "file_size": session.FileSize,
		})
		ts.fts.SendMessageToUser(session.ToUserID, session.RoomName, fwdMsg)

	case "complete":
		session, err := ts.fts.GetSession(transferID)
		if err != nil {
			ts.sendError(client, 404, "session not found")
			return
		}
		if session.ToUserID != client.UserID {
			ts.sendError(client, 403, "not receiver")
			return
		}
		// COMPLETE may arrive after receiver assembly but before sender END.
		if session.Status == "completed" {
			return
		}
		if session.Status != "transferring" && session.Status != "ending" && session.Status != "resending" {
			ts.sendError(client, 400, "bad status: "+session.Status)
			return
		}
		if err := ts.fts.UpdateSessionStatus(transferID, session.Status, "completed"); err != nil {
			ts.sendError(client, 409, "status changed: "+err.Error())
			return
		}
		fwdMsg := model.NewWebSocketMessage(model.MessageTypeFileTransferComplete, session.RoomName, "", map[string]interface{}{
			"transfer_id": transferID, "file_name": session.FileName, "file_size": session.FileSize,
		})
		ts.fts.SendMessageToUser(session.FromUserID, session.RoomName, fwdMsg)

	case "resend":
		session, err := ts.fts.GetSession(transferID)
		if err != nil {
			ts.sendError(client, 404, "session not found")
			return
		}
		if session.ToUserID != client.UserID {
			ts.sendError(client, 403, "not receiver")
			return
		}
		if session.Status == "completed" {
			return
		}
		if session.Status != "ending" {
			ts.sendError(client, 400, "bad status: "+session.Status)
			return
		}
		ts.fts.UpdateSessionStatus(transferID, "ending", "resending")
		fwdMsg := model.NewWebSocketMessage(model.MessageTypeFileTransferResend, session.RoomName, "", data)
		ts.fts.SendMessageToUser(session.FromUserID, session.RoomName, fwdMsg)

	case "cancel":
		session, err := ts.fts.GetSession(transferID)
		if err != nil {
			// Already cleaned up — OK
			return
		}
		if session.Status == "completed" || session.Status == "cancelled" || session.Status == "error" {
			return
		}
		ts.fts.UpdateSessionStatus(transferID, session.Status, "cancelled")
	}

}

func (ts *testServer) subscribeAndConfirm(client *model.Client, msg *model.WebSocketMessage) {
	event := msg.Event
	if err := ts.wsService.SubscribeToRoom(client.ID, msg.Channel, event); err != nil {
		ts.sendError(client, 400, err.Error())
		return
	}

	confirm := model.NewWebSocketMessage("subscribed", msg.Channel, event, map[string]interface{}{
		"status": "subscribed", "room": msg.Channel, "event": event,
	})
	if conn, ok := client.Connection.(*websocket.Conn); ok {
		client.ConnMutex.Lock()
		conn.WriteJSON(confirm)
		client.ConnMutex.Unlock()
	}
}

func (ts *testServer) sendError(client *model.Client, code int, message string) {
	errMsg := model.NewErrorMessage(code, message)
	if conn, ok := client.Connection.(*websocket.Conn); ok {
		client.ConnMutex.Lock()
		conn.WriteJSON(errMsg)
		client.ConnMutex.Unlock()
	}
}

func (ts *testServer) Close() { ts.srv.Close(); ts.wsService.Shutdown() }

// ============ Tests ============

type msgEnvelope struct {
	raw []byte
	err error
}

func dialTestClient(t *testing.T, srv *httptest.Server, userID string) *websocket.Conn {
	t.Helper()
	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1)
	fullURL := fmt.Sprintf("%s/ws?token=%s&userId=%s", wsURL, testToken(), userID)
	conn, _, err := websocket.DefaultDialer.Dial(fullURL, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", userID, err)
	}
	return conn
}

func subscribeRoom(t *testing.T, conn *websocket.Conn, room string) {
	msg := model.WebSocketMessage{Type: "subscribe", Channel: room, Event: "signal:all"}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("subscribe write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("subscribe read: %v", err)
	}
	var resp model.WebSocketMessage
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("subscribe unmarshal: %v (raw=%s)", err, string(raw))
	}
	if resp.Type != "subscribed" {
		var errData map[string]interface{}
		json.Unmarshal(resp.Data, &errData)
		t.Fatalf("expected subscribed, got %s (error=%v, data=%s)", resp.Type, resp.Error, rawMsg(resp.Data))
	}
}

func rawMsg(d json.RawMessage) string {
	var m map[string]interface{}
	if err := json.Unmarshal(d, &m); err == nil {
		b, _ := json.Marshal(m)
		return string(b)
	}
	return string(d)
}

// ---------- Test: Happy path ----------
func TestFileTransferE2E_HappyPath(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	sender := dialTestClient(t, ts.srv, "sender-1")
	receiver := dialTestClient(t, ts.srv, "receiver-1")
	defer sender.Close()
	defer receiver.Close()

	subscribeRoom(t, sender, "123")
	subscribeRoom(t, receiver, "123")

	// Receiver goroutine
	rcvCh := make(chan msgEnvelope, 32)
	go func() {
		for {
			receiver.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, raw, err := receiver.ReadMessage()
			if err != nil {
				rcvCh <- msgEnvelope{err: err}
				return
			}
			rcvCh <- msgEnvelope{raw: raw}
		}
	}()

	// Request
	reqJSON, _ := json.Marshal(map[string]interface{}{
		"transfer_id": "tf-happy", "file_name": "a.txt", "file_size": 100,
		"file_type": "text", "chunk_size": 65536, "total_chunks": 1,
		"from_user_id": "sender-1", "to_user_id": "receiver-1", "room_name": "123",
	})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:request", Channel: "123", Data: reqJSON})
	e := <-rcvCh
	if e.err != nil {
		t.Fatal(e.err)
	}
	var m model.WebSocketMessage
	json.Unmarshal(e.raw, &m)
	if m.Type != "file:transfer:request" {
		t.Fatalf("want request, got %s", m.Type)
	}

	// Accept
	accJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": "tf-happy"})
	receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:accept", Channel: "123", Data: accJSON})
	sender.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, raw, _ := sender.ReadMessage()
	json.Unmarshal(raw, &m)
	if m.Type != "file:transfer:accept" {
		t.Fatalf("want accept, got %s", m.Type)
	}

	// Start
	startJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": "tf-happy"})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:start", Channel: "123", Data: startJSON})
	e = <-rcvCh
	json.Unmarshal(e.raw, &m)
	if m.Type != "file:transfer:start" {
		t.Fatalf("want start, got %s", m.Type)
	}

	// Chunk (binary frame — the receiver goroutine can't distinguish binary from text easily)
	meta := model.FileTransferChunk{TransferID: "tf-happy", ChunkIndex: 0, ChunkSize: 100, TotalChunks: 1}
	metaJSON, _ := json.Marshal(meta)
	hdr := make([]byte, 256)
	copy(hdr, metaJSON)
	chunkFrame := append(hdr, []byte(strings.Repeat("X", 100))...)
	sender.WriteMessage(websocket.BinaryMessage, chunkFrame)

	// End — drain progress messages first
	endJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": "tf-happy"})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:end", Channel: "123", Data: endJSON})

	// Drain until we get "end" (skip progress and binary chunk frames)
	for i := 0; i < 5; i++ {
		e = <-rcvCh
		if e.err != nil {
			t.Fatalf("rcv after chunk: %v", e.err)
		}
		if err := json.Unmarshal(e.raw, &m); err != nil {
			// Binary frame (chunk) — skip
			continue
		}
		if m.Type == "file:transfer:end" {
			break
		}
		// Skip progress/chunk metadata/etc
	}
	if m.Type != "file:transfer:end" {
		t.Fatalf("want end, got %s", m.Type)
	}

	// Complete — drain progress messages (server sends progress after every chunk)
	completeJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": "tf-happy"})
	receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:complete", Channel: "123", Data: completeJSON})
	for i := 0; i < 5; i++ {
		sender.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, raw, err := sender.ReadMessage()
		if err != nil {
			t.Fatalf("sender read complete: %v", err)
		}
		json.Unmarshal(raw, &m)
		if m.Type == "file:transfer:complete" {
			break
		}
		// skip progress messages
	}
	if m.Type != "file:transfer:complete" {
		t.Fatalf("want complete, got %s", m.Type)
	}

	t.Log("✓ Happy path: request→accept→start→chunk→end→complete")
}

// ---------- Test: Bug #1 — Binary chunks in resending state ----------
func TestFileTransferE2E_ResendChunks(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	sender := dialTestClient(t, ts.srv, "sender-R1")
	receiver := dialTestClient(t, ts.srv, "receiver-R1")
	defer sender.Close()
	defer receiver.Close()

	subscribeRoom(t, sender, "RR")
	subscribeRoom(t, receiver, "RR")

	rcvCh := make(chan msgEnvelope, 32)
	go func() {
		for {
			receiver.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, raw, err := receiver.ReadMessage()
			if err != nil {
				rcvCh <- msgEnvelope{err: err}
				return
			}
			rcvCh <- msgEnvelope{raw: raw}
		}
	}()

	tid := "tf-resend"

	// Request→Accept→Start
	reqJSON, _ := json.Marshal(map[string]interface{}{
		"transfer_id": tid, "file_name": "b", "file_size": 200,
		"file_type": "text", "chunk_size": 100, "total_chunks": 2,
		"from_user_id": "sender-R1", "to_user_id": "receiver-R1", "room_name": "RR",
	})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:request", Channel: "RR", Data: reqJSON})
	<-rcvCh // request

	accJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:accept", Channel: "RR", Data: accJSON})
	sender.SetReadDeadline(time.Now().Add(5 * time.Second))
	sender.ReadMessage() // accept (can be Progress too)

	startJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:start", Channel: "RR", Data: startJSON})
	<-rcvCh // start

	// Send chunk 0, then END → ending
	meta0 := model.FileTransferChunk{TransferID: tid, ChunkIndex: 0, ChunkSize: 100, TotalChunks: 2}
	meta0JSON, _ := json.Marshal(meta0)
	hdr0 := make([]byte, 256)
	copy(hdr0, meta0JSON)
	sender.WriteMessage(websocket.BinaryMessage, append(hdr0, []byte(strings.Repeat("A", 100))...))

	// Drain progress messages until we get the chunk
	for {
		e := <-rcvCh
		if e.err != nil {
			t.Fatal(e.err)
		}
		var m model.WebSocketMessage
		json.Unmarshal(e.raw, &m)
		if m.Type == "file:transfer:progress" {
			continue // Skip progress updates
		}
		// Got the chunk (binary would have failed json.Unmarshal, so it's a JSON message)
		// Actually binary frames don't json.Unmarshal; they come through raw bytes
		break
	}

	endJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:end", Channel: "RR", Data: endJSON})
	<-rcvCh // end → ending

	// Resend → resending
	resendJSON, _ := json.Marshal(map[string]interface{}{
		"transfer_id": tid, "chunk_indexes": []interface{}{float64(1)},
		"missing_count": 1, "total_chunks": 2, "reason": "missing",
	})
	receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:resend", Channel: "RR", Data: resendJSON})
	sender.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, _ := sender.ReadMessage()
	var sm model.WebSocketMessage
	json.Unmarshal(raw, &sm)
	if sm.Type == "error" {
		t.Fatalf("resend rejected: %s", string(sm.Data))
	}
	// Now status should be "resending"

	// Send chunk 1 while status is resending — THIS IS BUG #1
	meta1 := model.FileTransferChunk{TransferID: tid, ChunkIndex: 1, ChunkSize: 100, TotalChunks: 2}
	meta1JSON, _ := json.Marshal(meta1)
	hdr1 := make([]byte, 256)
	copy(hdr1, meta1JSON)
	sender.WriteMessage(websocket.BinaryMessage, append(hdr1, []byte(strings.Repeat("B", 100))...))

	// Should NOT get an error. Wait a bit for potential error, and then verify we got the chunk.
	select {
	case e := <-rcvCh:
		if e.err != nil {
			t.Fatalf("BUG #1: chunk rejected in resending: %v", e.err)
		}
		var m model.WebSocketMessage
		json.Unmarshal(e.raw, &m)
		if m.Type == "error" {
			t.Fatalf("BUG #1: server rejected chunk in resending state: %v", string(m.Data))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("BUG #1: timeout — chunk never arrived (likely rejected silently)")
	}
	t.Log("✓ Bug #1 fixed: chunk accepted in resending state")
}

// ---------- Test: Bug #2 — COMPLETE from resending state ----------
func TestFileTransferE2E_CompleteFromTransferringBeforeSenderEnd(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	sender := dialTestClient(t, ts.srv, "sender-early-complete")
	receiver := dialTestClient(t, ts.srv, "receiver-early-complete")
	defer sender.Close()
	defer receiver.Close()

	subscribeRoom(t, sender, "CE")
	subscribeRoom(t, receiver, "CE")

	rcvCh := make(chan msgEnvelope, 16)
	go func() {
		for {
			receiver.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, raw, err := receiver.ReadMessage()
			if err != nil {
				rcvCh <- msgEnvelope{err: err}
				return
			}
			rcvCh <- msgEnvelope{raw: raw}
		}
	}()

	tid := "tf-complete-from-transferring"

	reqJSON, _ := json.Marshal(map[string]interface{}{
		"transfer_id": tid, "file_name": "early-complete.bin", "file_size": 100,
		"file_type": "application/octet-stream", "chunk_size": 100, "total_chunks": 1,
		"from_user_id": "sender-early-complete", "to_user_id": "receiver-early-complete",
		"room_name": "CE",
	})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:request", Channel: "CE", Data: reqJSON})
	<-rcvCh

	accJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:accept", Channel: "CE", Data: accJSON})
	sender.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := sender.ReadMessage(); err != nil {
		t.Fatalf("sender should receive accept: %v", err)
	}

	startJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:start", Channel: "CE", Data: startJSON})
	<-rcvCh

	completeJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:complete", Channel: "CE", Data: completeJSON})

	sender.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := sender.ReadMessage()
	if err != nil {
		t.Fatalf("sender should receive complete even before END: %v", err)
	}
	var sm model.WebSocketMessage
	if err := json.Unmarshal(raw, &sm); err != nil {
		t.Fatalf("parse sender message: %v", err)
	}
	if sm.Type != "file:transfer:complete" {
		t.Fatalf("COMPLETE from transferring should be forwarded, got %s data=%s", sm.Type, string(sm.Data))
	}

	session, err := ts.fts.GetSession(tid)
	if err != nil {
		t.Fatalf("completed transfer session should still be retained briefly: %v", err)
	}
	if session.Status != "completed" {
		t.Fatalf("COMPLETE from transferring should mark session completed, got %s", session.Status)
	}
}

func TestFileTransferE2E_CompleteFromResending(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	sender := dialTestClient(t, ts.srv, "sender-C1")
	receiver := dialTestClient(t, ts.srv, "receiver-C1")
	defer sender.Close()
	defer receiver.Close()

	subscribeRoom(t, sender, "CC")
	subscribeRoom(t, receiver, "CC")

	rcvCh := make(chan msgEnvelope, 32)
	go func() {
		for {
			receiver.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, raw, err := receiver.ReadMessage()
			if err != nil {
				rcvCh <- msgEnvelope{err: err}
				return
			}
			rcvCh <- msgEnvelope{raw: raw}
		}
	}()

	tid := "tf-complete-resend"

	// Request→Accept→Start
	reqJSON, _ := json.Marshal(map[string]interface{}{
		"transfer_id": tid, "file_name": "c", "file_size": 200,
		"file_type": "text", "chunk_size": 100, "total_chunks": 2,
		"from_user_id": "sender-C1", "to_user_id": "receiver-C1", "room_name": "CC",
	})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:request", Channel: "CC", Data: reqJSON})
	<-rcvCh
	accJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:accept", Channel: "CC", Data: accJSON})
	sender.SetReadDeadline(time.Now().Add(5 * time.Second))
	sender.ReadMessage()
	startJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:start", Channel: "CC", Data: startJSON})
	<-rcvCh

	// Chunk 0 + END → ending
	meta0 := model.FileTransferChunk{TransferID: tid, ChunkIndex: 0, ChunkSize: 100, TotalChunks: 2}
	m0JSON, _ := json.Marshal(meta0)
	h0 := make([]byte, 256)
	copy(h0, m0JSON)
	sender.WriteMessage(websocket.BinaryMessage, append(h0, []byte(strings.Repeat("A", 100))...))
	<-rcvCh // chunk or progress

	endJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:end", Channel: "CC", Data: endJSON})
	<-rcvCh // end → ending

	// Resend → resending
	resendJSON, _ := json.Marshal(map[string]interface{}{
		"transfer_id": tid, "chunk_indexes": []interface{}{float64(1)},
		"missing_count": 1, "total_chunks": 2,
	})
	receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:resend", Channel: "CC", Data: resendJSON})

	// Drain the forwarded resend from sender
	sender.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := sender.ReadMessage()
	if err != nil {
		t.Fatalf("drain resend from sender: %v", err)
	}

	// Now send COMPLETE from resending — THIS IS BUG #2
	completeJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:complete", Channel: "CC", Data: completeJSON})

	// Drain until we get complete (skip progress messages that server may send)
	var sm model.WebSocketMessage
	for i := 0; i < 5; i++ {
		sender.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, raw, err = sender.ReadMessage()
		if err != nil {
			t.Fatalf("sender read %d: %v", i, err)
		}
		json.Unmarshal(raw, &sm)
		if sm.Type == "file:transfer:complete" {
			break
		}
		if sm.Type == "error" {
			t.Fatalf("BUG #2: COMPLETE rejected from resending: %s", string(sm.Data))
		}
	}
	if sm.Type != "file:transfer:complete" {
		t.Fatalf("want complete, got %s", sm.Type)
	}
	session, err := ts.fts.GetSession(tid)
	if err != nil {
		t.Fatalf("completed transfer session should still be retained briefly: %v", err)
	}
	if session.Status != "completed" {
		t.Fatalf("COMPLETE from resending should mark session completed, got %s", session.Status)
	}
	t.Log("✓ Bug #2 fixed: COMPLETE accepted from resending state")
}
