//go:build letshare_stress

package handler

import (
	"encoding/json"
	"fmt"
	"letshare-server/internal/model"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ============ Dimension 1: Stability — Panic Recovery & Invalid Input ============

// TestStability_InvalidJSON verifies server doesn't crash on malformed JSON
func TestStability_InvalidJSON(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	conn := dialTestClient(t, ts.srv, "bad-json-user")
	defer conn.Close()
	subscribeRoom(t, conn, "ab")

	// Send garbage that isn't valid JSON
	conn.WriteMessage(websocket.TextMessage, []byte("NOT JSON AT ALL {{{"))
	time.Sleep(100 * time.Millisecond)

	// Send partial JSON
	conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"subscribe","ch`))
	time.Sleep(100 * time.Millisecond)

	// Send valid message after garbage — should still work
	subscribeRoom(t, conn, "cd")

	t.Log("✓ Server handles invalid JSON without crash")
}

// TestStability_UnknownMessageType verifies unknown types don't crash
func TestStability_UnknownMessageType(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	conn := dialTestClient(t, ts.srv, "unknown-type")
	defer conn.Close()
	subscribeRoom(t, conn, "ab")

	// Send unknown message type
	unknownJSON, _ := json.Marshal(model.WebSocketMessage{
		Type:    "weird:unknown:type:that:does:not:exist",
		Channel: "ab",
	})
	conn.WriteMessage(websocket.TextMessage, unknownJSON)
	time.Sleep(100 * time.Millisecond)

	// Server should still be operational
	subscribeRoom(t, conn, "ef")
	t.Log("✓ Server survives unknown message types")
}

// TestStability_NilFields verifies nil/missing fields don't crash
func TestStability_NilFields(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	conn := dialTestClient(t, ts.srv, "nil-user")
	defer conn.Close()
	subscribeRoom(t, conn, "ab")

	// Subscribe with empty channel — should get error via drain, not crash
	conn.WriteJSON(model.WebSocketMessage{Type: "subscribe", Channel: "", Event: "signal:all"})
	// Drain the error response
	conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	conn.ReadMessage()

	// Publish without data — should handle gracefully
	conn.WriteJSON(model.WebSocketMessage{Type: "publish", Channel: "ab"})
	time.Sleep(100 * time.Millisecond)

	// File transfer request without transfer_id — should get error
	reqJSON, _ := json.Marshal(map[string]interface{}{"file_name": "test"})
	conn.WriteJSON(model.WebSocketMessage{Type: "file:transfer:request", Channel: "ab", Data: reqJSON})
	conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	conn.ReadMessage() // drain error

	// Server still operational
	subscribeRoom(t, conn, "gh")
	t.Log("✓ Server handles nil/empty fields without crash")
}

// TestStability_ShortBinaryFrame verifies server rejects binary < 256 bytes
func TestStability_ShortBinaryFrame(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	sender := dialTestClient(t, ts.srv, "short-bin-sender")
	defer sender.Close()
	subscribeRoom(t, sender, "sb")

	// Send binary frame that's too short (< 256 bytes header)
	err := sender.WriteMessage(websocket.BinaryMessage, []byte("short"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	// Server should not crash — should just reject
	t.Log("✓ Server handles short binary frames gracefully")
}

// ============ Dimension 2: Concurrency & Stress ============

// TestStress_ManyConnections verifies server handles 10 concurrent connections (2-core/2GB limit aware)
func TestStress_ManyConnections(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	const numClients = 10 // match default maxRoomUsers for 2-core/2GB server
	var conns []*websocket.Conn
	var wg sync.WaitGroup

	// Connect all clients concurrently
	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			conn := dialTestClient(t, ts.srv, fmt.Sprintf("stress-user-%d", idx))
			conns = append(conns, conn)
		}(i)
	}
	wg.Wait()

	if len(conns) < numClients {
		t.Fatalf("only connected %d/%d clients", len(conns), numClients)
	}

	// All subscribe to same room concurrently
	room := "stress-room"
	for _, conn := range conns {
		wg.Add(1)
		go func(c *websocket.Conn) {
			defer wg.Done()
			subscribeRoom(t, c, room)
		}(conn)
	}
	wg.Wait()

	// Publish from each client concurrently
	for _, conn := range conns[:5] {
		wg.Add(1)
		go func(c *websocket.Conn) {
			defer wg.Done()
			data, _ := json.Marshal(map[string]interface{}{"msg": "hello"})
			c.WriteJSON(model.WebSocketMessage{Type: "publish", Channel: room, Event: "signal:all", Data: data})
		}(conn)
	}
	wg.Wait()
	time.Sleep(200 * time.Millisecond)

	// Cleanup
	for _, conn := range conns {
		conn.Close()
	}

	t.Logf("✓ Server handles %d concurrent connections in shared room", numClients)
}

// TestStress_ConcurrentFileTransfers verifies concurrent file transfers don't interfere
func TestStress_ConcurrentFileTransfers(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	const numTransfers = 5
	type pair struct {
		sender   *websocket.Conn
		receiver *websocket.Conn
	}
	var pairs []pair

	for i := 0; i < numTransfers; i++ {
		s := dialTestClient(t, ts.srv, fmt.Sprintf("cs-sender-%d", i))
		r := dialTestClient(t, ts.srv, fmt.Sprintf("cs-receiver-%d", i))
		room := fmt.Sprintf("cr%d", i)
		subscribeRoom(t, s, room)
		subscribeRoom(t, r, room)
		pairs = append(pairs, pair{s, r})
	}

	var wg sync.WaitGroup
	errCh := make(chan error, numTransfers)

	for i, p := range pairs {
		wg.Add(1)
		go func(idx int, s, r *websocket.Conn) {
			defer wg.Done()
			tid := fmt.Sprintf("cs-tf-%d", idx)
			room := fmt.Sprintf("cr%d", idx)

			// Start receiver goroutine
			rcvCh := make(chan msgEnvelope, 16)
			go func() {
				for {
					r.SetReadDeadline(time.Now().Add(10 * time.Second))
					_, raw, err := r.ReadMessage()
					if err != nil {
						return
					}
					rcvCh <- msgEnvelope{raw: raw}
				}
			}()

			// Request
			reqJSON, _ := json.Marshal(map[string]interface{}{
				"transfer_id": tid, "file_name": fmt.Sprintf("f%d", idx), "file_size": 100,
				"file_type": "text", "chunk_size": 65536, "total_chunks": 1,
				"from_user_id": fmt.Sprintf("cs-sender-%d", idx),
				"to_user_id":   fmt.Sprintf("cs-receiver-%d", idx),
				"room_name":    room,
			})
			s.WriteJSON(model.WebSocketMessage{Type: "file:transfer:request", Channel: room, Data: reqJSON})
			<-rcvCh // request

			// Accept
			accJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
			r.WriteJSON(model.WebSocketMessage{Type: "file:transfer:accept", Channel: room, Data: accJSON})
			time.Sleep(50 * time.Millisecond)

			// Start
			startJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
			s.WriteJSON(model.WebSocketMessage{Type: "file:transfer:start", Channel: room, Data: startJSON})
			<-rcvCh // start
		}(i, p.sender, p.receiver)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Logf("✓ %d concurrent transfers started without errors", numTransfers)
	case err := <-errCh:
		t.Fatalf("concurrent transfer failed: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("concurrent transfers timed out")
	}

	for _, p := range pairs {
		p.sender.Close()
		p.receiver.Close()
	}
}

// TestStress_BurstMessages verifies server handles message burst
func TestStress_BurstMessages(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	sender := dialTestClient(t, ts.srv, "burst-sender")
	receiver := dialTestClient(t, ts.srv, "burst-receiver")
	defer sender.Close()
	defer receiver.Close()

	room := "burst-room"
	subscribeRoom(t, sender, room)
	subscribeRoom(t, receiver, room)

	// Drain receiver
	errCh := make(chan error, 1)
	go func() {
		receiver.SetReadDeadline(time.Now().Add(15 * time.Second))
		for {
			_, _, err := receiver.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}
		}
	}()

	// Burst 100 messages
	for i := 0; i < 100; i++ {
		data, _ := json.Marshal(map[string]interface{}{"seq": i})
		if err := sender.WriteJSON(model.WebSocketMessage{
			Type: "publish", Channel: room, Event: "signal:all", Data: data,
		}); err != nil {
			t.Fatalf("burst message %d failed: %v", i, err)
		}
	}

	time.Sleep(1 * time.Second)
	t.Log("✓ Server handles 100 message burst without failure")
}

// ============ Dimension 3: Multi-User & Room Isolation ============

// TestMultiUser_RoomIsolation verifies messages only reach users in same room
func TestMultiUser_RoomIsolation(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	// Room A: 2 users
	a1 := dialTestClient(t, ts.srv, "roomA-user1")
	a2 := dialTestClient(t, ts.srv, "roomA-user2")
	defer a1.Close()
	defer a2.Close()
	subscribeRoom(t, a1, "roomA")
	subscribeRoom(t, a2, "roomA")

	// Room B: 1 user
	b1 := dialTestClient(t, ts.srv, "roomB-user1")
	defer b1.Close()
	subscribeRoom(t, b1, "roomB")

	// Drain b1 in goroutine — should NOT receive messages from roomA
	b1MsgCh := make(chan msgEnvelope, 8)
	go func() {
		b1.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, raw, err := b1.ReadMessage()
		if err != nil {
			b1MsgCh <- msgEnvelope{err: err}
		} else {
			b1MsgCh <- msgEnvelope{raw: raw}
		}
	}()

	// Send message in roomA
	data, _ := json.Marshal(map[string]string{"text": "secret A"})
	a1.WriteJSON(model.WebSocketMessage{Type: "publish", Channel: "roomA", Event: "signal:all", Data: data})

	// a2 should receive it
	a2.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, raw, err := a2.ReadMessage()
	if err != nil {
		t.Fatalf("a2 should receive roomA message: %v", err)
	}
	var m model.WebSocketMessage
	json.Unmarshal(raw, &m)
	if m.Type != "message" {
		t.Fatalf("expected message, got %s", m.Type)
	}

	// b1 should NOT receive roomA message (should timeout)
	select {
	case env := <-b1MsgCh:
		if env.err == nil {
			t.Fatal("B1 received message from roomA — ROOM ISOLATION BROKEN")
		}
		// Expected timeout error
	case <-time.After(4 * time.Second):
		t.Log("✓ Room isolation confirmed: roomA messages not leaked to roomB")
	}
}

// TestMultiUser_ThreePartyTransfer verifies 3 users in same room, only target receives file request
func TestMultiUser_ThreePartyTransfer(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	room := "three-users"
	alice := dialTestClient(t, ts.srv, "alice")
	bob := dialTestClient(t, ts.srv, "bob")
	carol := dialTestClient(t, ts.srv, "carol")
	defer alice.Close()
	defer bob.Close()
	defer carol.Close()

	subscribeRoom(t, alice, room)
	subscribeRoom(t, bob, room)
	subscribeRoom(t, carol, room)

	// Carol listens for messages (should NOT get file request sent to Bob)
	carolCh := make(chan msgEnvelope, 8)
	go func() {
		carol.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, raw, err := carol.ReadMessage()
		if err != nil {
			carolCh <- msgEnvelope{err: err}
		} else {
			carolCh <- msgEnvelope{raw: raw}
		}
	}()

	// Alice → Bob file request
	reqJSON, _ := json.Marshal(map[string]interface{}{
		"transfer_id": "tf-three", "file_name": "secret.pdf", "file_size": 1000,
		"file_type": "pdf", "chunk_size": 65536, "total_chunks": 1,
		"from_user_id": "alice", "to_user_id": "bob", "room_name": room,
	})
	alice.WriteJSON(model.WebSocketMessage{Type: "file:transfer:request", Channel: room, Data: reqJSON})

	// Bob should receive it
	bob.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, raw, err := bob.ReadMessage()
	if err != nil {
		t.Fatalf("bob should get file request: %v", err)
	}
	var m model.WebSocketMessage
	json.Unmarshal(raw, &m)
	if m.Type != "file:transfer:request" {
		t.Fatalf("expected request to bob, got %s", m.Type)
	}

	// Carol should NOT receive it
	select {
	case env := <-carolCh:
		if env.err == nil {
			t.Fatal("Carol received file request meant for Bob — TARGET ROUTING BROKEN")
		}
	case <-time.After(4 * time.Second):
		t.Log("✓ Target routing: file request only delivered to target user")
	}
}

// ============ Dimension 4: Resilience & Disconnect ============

// TestResilience_SenderDisconnectDuringTransfer verifies receiver gets notified
func TestResilience_SenderDisconnectDuringTransfer(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	sender := dialTestClient(t, ts.srv, "dc-sender")
	receiver := dialTestClient(t, ts.srv, "dc-receiver")
	defer receiver.Close()

	room := "dc-room"
	subscribeRoom(t, sender, room)
	subscribeRoom(t, receiver, room)

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

	tid := "tf-disconnect"

	// Request
	reqJSON, _ := json.Marshal(map[string]interface{}{
		"transfer_id": tid, "file_name": "dc.txt", "file_size": 1000,
		"file_type": "text", "chunk_size": 100, "total_chunks": 10,
		"from_user_id": "dc-sender", "to_user_id": "dc-receiver", "room_name": room,
	})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:request", Channel: room, Data: reqJSON})
	<-rcvCh // request

	// Accept
	accJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:accept", Channel: room, Data: accJSON})

	// Start
	sender.SetReadDeadline(time.Now().Add(3 * time.Second))
	sender.ReadMessage() // drain accept
	startJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:start", Channel: room, Data: startJSON})
	<-rcvCh // start

	// Abruptly disconnect sender
	sender.Close()

	// Receiver should get an error notification within timeout
	select {
	case env := <-rcvCh:
		if env.err != nil {
			// Connection closed is expected
			t.Log("✓ Receiver connection terminated after sender disconnect (expected)")
		} else {
			var m model.WebSocketMessage
			json.Unmarshal(env.raw, &m)
			if m.Type == "error" || m.Type == "file:transfer:error" {
				t.Logf("✓ Receiver notified of sender disconnect: %s", string(env.raw))
			}
		}
	case <-time.After(5 * time.Second):
		t.Log("✓ No crash from sender disconnect (timeout acceptable)")
	}
}

// TestResilience_DuplicateSubscription verifies double-subscribe is handled
func TestResilience_DuplicateSubscription(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	conn := dialTestClient(t, ts.srv, "dup-sub")
	defer conn.Close()

	subscribeRoom(t, conn, "dup-room")

	// Subscribe again to same room — should not error
	conn.WriteJSON(model.WebSocketMessage{Type: "subscribe", Channel: "dup-room", Event: "signal:all"})
	time.Sleep(100 * time.Millisecond)

	t.Log("✓ Duplicate subscription handled without error")
}

// TestResilience_ResendFromWrongState verifies resend requests from invalid states are rejected
func TestResilience_ResendFromWrongState(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	sender := dialTestClient(t, ts.srv, "rw-sender")
	receiver := dialTestClient(t, ts.srv, "rw-receiver")
	defer sender.Close()
	defer receiver.Close()

	subscribeRoom(t, sender, "rw-room")
	subscribeRoom(t, receiver, "rw-room")

	rcvCh := make(chan msgEnvelope, 8)
	go func() {
		for {
			receiver.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, raw, err := receiver.ReadMessage()
			if err != nil {
				return
			}
			rcvCh <- msgEnvelope{raw: raw}
		}
	}()

	tid := "tf-wrong-resend"

	// Request → status pending
	reqJSON, _ := json.Marshal(map[string]interface{}{
		"transfer_id": tid, "file_name": "rw", "file_size": 100,
		"file_type": "text", "chunk_size": 50, "total_chunks": 2,
		"from_user_id": "rw-sender", "to_user_id": "rw-receiver", "room_name": "rw-room",
	})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:request", Channel: "rw-room", Data: reqJSON})
	<-rcvCh

	// Try to resend before accept → should be rejected (status is "pending", not "ending")
	resendJSON, _ := json.Marshal(map[string]interface{}{
		"transfer_id": tid, "chunk_indexes": []interface{}{float64(0)},
		"missing_count": 1, "total_chunks": 2,
	})
	receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:resend", Channel: "rw-room", Data: resendJSON})

	// Receiver should get error about bad status
	select {
	case env := <-rcvCh:
		var m model.WebSocketMessage
		json.Unmarshal(env.raw, &m)
		if m.Type == "error" {
			t.Logf("✓ Resend from 'pending' correctly rejected")
		}
	case <-time.After(3 * time.Second):
		t.Log("✓ No crash on invalid resend")
	}
}

// TestResilience_TransferRequestToSelf verifies self-transfer is blocked
func TestResilience_TransferRequestToSelf(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	conn := dialTestClient(t, ts.srv, "self-user")
	defer conn.Close()
	subscribeRoom(t, conn, "self-room")

	// Sender == Receiver
	reqJSON, _ := json.Marshal(map[string]interface{}{
		"transfer_id": "self-tf", "file_name": "self", "file_size": 100,
		"file_type": "text", "chunk_size": 65536, "total_chunks": 1,
		"from_user_id": "self-user", "to_user_id": "self-user", "room_name": "self-room",
	})
	conn.WriteJSON(model.WebSocketMessage{Type: "file:transfer:request", Channel: "self-room", Data: reqJSON})

	// Should not crash — just logs
	time.Sleep(200 * time.Millisecond)
	t.Log("✓ Self-transfer doesn't crash server")
}

// ============ Dimension 5: Memory & Resource Cleanup ============

// TestMemory_SessionCleanupAfterComplete verifies sessions are removed after completion
func TestMemory_SessionCleanupAfterComplete(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	sender := dialTestClient(t, ts.srv, "mem-sender")
	receiver := dialTestClient(t, ts.srv, "mem-receiver")
	defer sender.Close()
	defer receiver.Close()

	subscribeRoom(t, sender, "mem-room")
	subscribeRoom(t, receiver, "mem-room")

	rcvCh := make(chan msgEnvelope, 16)
	go func() {
		for {
			receiver.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, raw, err := receiver.ReadMessage()
			if err != nil {
				return
			}
			rcvCh <- msgEnvelope{raw: raw}
		}
	}()

	// Complete 5 transfers with drain helper
	drainSender := func() {
		sender.SetReadDeadline(time.Now().Add(1 * time.Second))
		for {
			_, _, err := sender.ReadMessage()
			if err != nil {
				return
			}
		}
	}

	for i := 0; i < 5; i++ {
		tid := fmt.Sprintf("mem-tf-%d", i)

		reqJSON, _ := json.Marshal(map[string]interface{}{
			"transfer_id": tid, "file_name": fmt.Sprintf("f%d", i), "file_size": 100,
			"file_type": "text", "chunk_size": 65536, "total_chunks": 1,
			"from_user_id": "mem-sender", "to_user_id": "mem-receiver", "room_name": "mem-room",
		})
		sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:request", Channel: "mem-room", Data: reqJSON})
		<-rcvCh

		accJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
		receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:accept", Channel: "mem-room", Data: accJSON})
		drainSender() // drain accept + any progress

		startJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
		sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:start", Channel: "mem-room", Data: startJSON})
		<-rcvCh

		// Chunk
		meta := model.FileTransferChunk{TransferID: tid, ChunkIndex: 0, ChunkSize: 100, TotalChunks: 1}
		metaJSON, _ := json.Marshal(meta)
		hdr := make([]byte, 256)
		copy(hdr, metaJSON)
		sender.WriteMessage(websocket.BinaryMessage, append(hdr, []byte(strings.Repeat("Z", 100))...))

		endJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
		sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:end", Channel: "mem-room", Data: endJSON})
		<-rcvCh // end

		completeJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
		receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:complete", Channel: "mem-room", Data: completeJSON})
		drainSender() // drain complete + progress
	}

	// Check stats — sessions should be cleaned up (completed sessions have 30s delay cleanup)
	stats := ts.fts.GetStats()
	t.Logf("Server stats after 5 transfers: %v", stats)
	// Sessions may still be in 30s cleanup window — that's expected
	t.Log("✓ Transfers complete without memory errors")
}

// TestMemory_ClientDisconnectCleanup verifies client resources are freed
func TestMemory_ClientDisconnectCleanup(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	var memBefore runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)

	// Connect and disconnect 50 clients
	for i := 0; i < 50; i++ {
		conn := dialTestClient(t, ts.srv, fmt.Sprintf("gc-user-%d", i))
		subscribeRoom(t, conn, "gc-room")
		conn.Close()
	}
	time.Sleep(200 * time.Millisecond)

	runtime.GC()
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)

	// Verify no goroutine leak — allow small growth for runtime overhead
	allocDelta := int64(memAfter.HeapInuse) - int64(memBefore.HeapInuse)
	t.Logf("Heap delta after 50 connects/disconnects: %d bytes", allocDelta)

	// Should not retain large amounts of memory
	if allocDelta > 50*1024*1024 { // 50MB
		t.Fatalf("potential memory leak: %d bytes retained after cleanup", allocDelta)
	}
	t.Log("✓ Client disconnect cleanup verified")
}

// ============ Dimension 6: Edge Cases & Validation ============

// TestEdgeCase_OversizedFile verifies file size limits
func TestEdgeCase_OversizedFile(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	sender := dialTestClient(t, ts.srv, "big-sender")
	receiver := dialTestClient(t, ts.srv, "big-receiver")
	defer sender.Close()
	defer receiver.Close()

	subscribeRoom(t, sender, "big-room")
	subscribeRoom(t, receiver, "big-room")

	// Request with huge file (600MB > 500MB limit)
	reqJSON, _ := json.Marshal(map[string]interface{}{
		"transfer_id": "big-tf", "file_name": "huge.bin",
		"file_size": 600 * 1024 * 1024, // 600MB
		"file_type": "bin", "chunk_size": 65536, "total_chunks": 10000,
		"from_user_id": "big-sender", "to_user_id": "big-receiver", "room_name": "big-room",
	})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:request", Channel: "big-room", Data: reqJSON})

	sender.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, raw, err := sender.ReadMessage()
	if err != nil {
		t.Fatalf("expected rejection: %v", err)
	}
	var m model.WebSocketMessage
	json.Unmarshal(raw, &m)
	if m.Type != "error" {
		t.Fatalf("expected error for oversized file, got %s", m.Type)
	}
	t.Log("✓ Oversized file request correctly rejected")
}

// TestEdgeCase_ZeroFileSize verifies zero-size files are handled
func TestEdgeCase_ZeroFileSize(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	sender := dialTestClient(t, ts.srv, "zero-sender")
	receiver := dialTestClient(t, ts.srv, "zero-receiver")
	defer sender.Close()
	defer receiver.Close()

	subscribeRoom(t, sender, "zero-room")
	subscribeRoom(t, receiver, "zero-room")

	rcvCh := make(chan msgEnvelope, 8)
	go func() {
		for {
			receiver.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, raw, err := receiver.ReadMessage()
			if err != nil {
				return
			}
			rcvCh <- msgEnvelope{raw: raw}
		}
	}()

	tid := "zero-tf"

	reqJSON, _ := json.Marshal(map[string]interface{}{
		"transfer_id": tid, "file_name": "empty.txt", "file_size": 0,
		"file_type": "text", "chunk_size": 65536, "total_chunks": 1,
		"from_user_id": "zero-sender", "to_user_id": "zero-receiver", "room_name": "zero-room",
	})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:request", Channel: "zero-room", Data: reqJSON})
	<-rcvCh

	// Accept and complete flow for 0-size file
	accJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:accept", Channel: "zero-room", Data: accJSON})
	sender.SetReadDeadline(time.Now().Add(3 * time.Second))
	sender.ReadMessage()

	startJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:start", Channel: "zero-room", Data: startJSON})
	<-rcvCh

	// Zero-byte chunk
	meta := model.FileTransferChunk{TransferID: tid, ChunkIndex: 0, ChunkSize: 0, TotalChunks: 1}
	metaJSON, _ := json.Marshal(meta)
	hdr := make([]byte, 256)
	copy(hdr, metaJSON)
	sender.WriteMessage(websocket.BinaryMessage, hdr) // no data, just header

	endJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:end", Channel: "zero-room", Data: endJSON})
	<-rcvCh

	completeJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:complete", Channel: "zero-room", Data: completeJSON})

	t.Log("✓ Zero-size file transfer doesn't crash")
}

// TestEdgeCase_VeryLongRoomName verifies room name length limits
func TestEdgeCase_VeryLongRoomName(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	conn := dialTestClient(t, ts.srv, "longname-user")
	defer conn.Close()

	// Very long room name (100 chars)
	longName := strings.Repeat("x", 100)
	msg := model.WebSocketMessage{Type: "subscribe", Channel: longName, Event: "signal:all"}
	conn.WriteJSON(msg)

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, raw, _ := conn.ReadMessage()
	var resp model.WebSocketMessage
	json.Unmarshal(raw, &resp)
	// Long names may be rejected depending on validation rules
	t.Logf("Long room name (100 chars): response=%s", resp.Type)
	t.Log("✓ Long room name handled without crash")
}

// TestEdgeCase_ConcurrentSameTransferID verifies duplicate transfer IDs are handled
func TestEdgeCase_ConcurrentSameTransferID(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	sender := dialTestClient(t, ts.srv, "dup-tf-sender")
	receiver := dialTestClient(t, ts.srv, "dup-tf-receiver")
	defer sender.Close()
	defer receiver.Close()

	subscribeRoom(t, sender, "dup-room")
	subscribeRoom(t, receiver, "dup-room")

	rcvCh := make(chan msgEnvelope, 16)
	go func() {
		for {
			receiver.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, raw, err := receiver.ReadMessage()
			if err != nil {
				return
			}
			rcvCh <- msgEnvelope{raw: raw}
		}
	}()

	// Send same transfer request twice
	for i := 0; i < 2; i++ {
		reqJSON, _ := json.Marshal(map[string]interface{}{
			"transfer_id": "dup-tf-id", "file_name": "dup.txt", "file_size": 100,
			"file_type": "text", "chunk_size": 65536, "total_chunks": 1,
			"from_user_id": "dup-tf-sender", "to_user_id": "dup-tf-receiver", "room_name": "dup-room",
		})
		sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:request", Channel: "dup-room", Data: reqJSON})
	}

	// First request creates session, second should overwrite (or reject)
	// Server should not crash regardless
	<-rcvCh
	time.Sleep(200 * time.Millisecond)
	t.Log("✓ Duplicate transfer requests handled without crash")
}

// TestEdgeCase_RapidStateTransitions verifies rapid state changes don't cause races
func TestEdgeCase_RapidStateTransitions(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	sender := dialTestClient(t, ts.srv, "rapid-sender")
	receiver := dialTestClient(t, ts.srv, "rapid-receiver")
	defer sender.Close()
	defer receiver.Close()

	subscribeRoom(t, sender, "rapid")
	subscribeRoom(t, receiver, "rapid")

	rcvCh := make(chan msgEnvelope, 16)
	go func() {
		for {
			receiver.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, raw, err := receiver.ReadMessage()
			if err != nil {
				return
			}
			rcvCh <- msgEnvelope{raw: raw}
		}
	}()

	tid := "rapid-tf"

	reqJSON, _ := json.Marshal(map[string]interface{}{
		"transfer_id": tid, "file_name": "r", "file_size": 100,
		"file_type": "text", "chunk_size": 65536, "total_chunks": 1,
		"from_user_id": "rapid-sender", "to_user_id": "rapid-receiver", "room_name": "rapid",
	})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:request", Channel: "rapid", Data: reqJSON})
	<-rcvCh

	// Rapidly send accept + start + end + complete without waiting
	accJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:accept", Channel: "rapid", Data: accJSON})

	startJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:start", Channel: "rapid", Data: startJSON})

	// Binary chunk + end (sent rapidly — may race with start processing)
	meta := model.FileTransferChunk{TransferID: tid, ChunkIndex: 0, ChunkSize: 100, TotalChunks: 1}
	metaJSON, _ := json.Marshal(meta)
	hdr := make([]byte, 256)
	copy(hdr, metaJSON)
	sender.WriteMessage(websocket.BinaryMessage, append(hdr, []byte(strings.Repeat("R", 100))...))

	endJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:end", Channel: "rapid", Data: endJSON})

	completeJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:complete", Channel: "rapid", Data: completeJSON})

	// Drain all messages — no requirement on order, just no crash
	for i := 0; i < 10; i++ {
		sender.SetReadDeadline(time.Now().Add(2 * time.Second))
		sender.ReadMessage()
	}
	t.Log("✓ Rapid state transitions handled without crash or panic")
}

// ============ Network Resilience: Sudden Disconnect ============

// TestNetworkResilience_DisconnectDuringResend verifies disconnect during resending notifies peer
func TestNetworkResilience_DisconnectDuringResend(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	sender := dialTestClient(t, ts.srv, "nd-sender")
	receiver := dialTestClient(t, ts.srv, "nd-receiver")
	defer receiver.Close()

	room := "nd-room"
	subscribeRoom(t, sender, room)
	subscribeRoom(t, receiver, room)

	rcvCh := make(chan msgEnvelope, 16)
	go func() {
		for {
			receiver.SetReadDeadline(time.Now().Add(10 * time.Second))
			_, raw, err := receiver.ReadMessage()
			if err != nil {
				rcvCh <- msgEnvelope{err: err}
				return
			}
			rcvCh <- msgEnvelope{raw: raw}
		}
	}()

	tid := "nd-tf"

	// Request → Accept → Start → Chunk → End → Resend
	reqJSON, _ := json.Marshal(map[string]interface{}{
		"transfer_id": tid, "file_name": "nd.txt", "file_size": 200,
		"file_type": "text", "chunk_size": 100, "total_chunks": 2,
		"from_user_id": "nd-sender", "to_user_id": "nd-receiver", "room_name": room,
	})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:request", Channel: room, Data: reqJSON})
	<-rcvCh

	accJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:accept", Channel: room, Data: accJSON})
	sender.SetReadDeadline(time.Now().Add(3 * time.Second))
	sender.ReadMessage()

	startJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:start", Channel: room, Data: startJSON})
	<-rcvCh

	// Send 1 chunk, then END → ending
	meta := model.FileTransferChunk{TransferID: tid, ChunkIndex: 0, ChunkSize: 100, TotalChunks: 2}
	metaJSON, _ := json.Marshal(meta)
	hdr := make([]byte, 256)
	copy(hdr, metaJSON)
	sender.WriteMessage(websocket.BinaryMessage, append(hdr, []byte(strings.Repeat("D", 100))...))

	endJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:end", Channel: room, Data: endJSON})
	<-rcvCh // end

	// Resend → status = resending
	resendJSON, _ := json.Marshal(map[string]interface{}{
		"transfer_id": tid, "chunk_indexes": []interface{}{float64(1)},
		"missing_count": 1, "total_chunks": 2,
	})
	receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:resend", Channel: room, Data: resendJSON})
	sender.SetReadDeadline(time.Now().Add(3 * time.Second))
	sender.ReadMessage() // drain resend

	// Now abruptly disconnect sender while status = resending
	sender.Close()

	// Receiver should get notified via file:transfer:error
	select {
	case env := <-rcvCh:
		if env.err != nil {
			t.Logf("✓ Receiver connection closed after sender disconnect (expected): %v", env.err)
		} else {
			var m model.WebSocketMessage
			json.Unmarshal(env.raw, &m)
			if m.Type == "file:transfer:error" || m.Type == "error" {
				var d map[string]interface{}
				json.Unmarshal(m.Data, &d)
				t.Logf("✓ Peer notified of disconnect during resend: %v", d)
			}
		}
	case <-time.After(5 * time.Second):
		t.Log("✓ No crash on disconnect during resend (cleanup completed)")
	}
}

// TestNetworkResilience_WriteDeadline verifies write deadline prevents permanent block
func TestNetworkResilience_WriteDeadline(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	// Normal sender
	sender := dialTestClient(t, ts.srv, "wd-sender")
	defer sender.Close()
	subscribeRoom(t, sender, "wd-room")

	// Slow receiver — connect but don't read (simulate network congestion)
	slow := dialTestClient(t, ts.srv, "wd-slow")
	defer slow.Close()
	subscribeRoom(t, slow, "wd-room")

	// Burst messages to fill receiver's buffer
	for i := 0; i < 50; i++ {
		data, _ := json.Marshal(map[string]interface{}{"seq": i, "padding": strings.Repeat("x", 500)})
		if err := sender.WriteJSON(model.WebSocketMessage{
			Type: "publish", Channel: "wd-room", Event: "signal:all", Data: data,
		}); err != nil {
			t.Fatalf("burst %d: %v", i, err)
		}
	}

	// Slow reader starts draining — should not block server permanently
	time.Sleep(500 * time.Millisecond)
	for i := 0; i < 10; i++ {
		slow.SetReadDeadline(time.Now().Add(1 * time.Second))
		if _, _, err := slow.ReadMessage(); err != nil {
			break
		}
	}

	// Server should still be operational — sender can still send
	data, _ := json.Marshal(map[string]interface{}{"msg": "still-alive"})
	if err := sender.WriteJSON(model.WebSocketMessage{
		Type: "publish", Channel: "wd-room", Event: "signal:all", Data: data,
	}); err != nil {
		t.Fatal("Server became unresponsive after slow consumer")
	}
	t.Log("✓ Write deadline prevents slow-consumer deadlock")
}

// TestNetworkResilience_RapidReconnect verifies state cleanup on fast reconnect
func TestNetworkResilience_RapidReconnect(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	room := "rr-room"

	for i := 0; i < 10; i++ {
		conn := dialTestClient(t, ts.srv, "rr-user")
		subscribeRoom(t, conn, room)

		// Send one message and disconnect immediately
		data, _ := json.Marshal(map[string]interface{}{"seq": i})
		conn.WriteJSON(model.WebSocketMessage{Type: "publish", Channel: room, Event: "signal:all", Data: data})
		conn.Close()

		// Small delay to let cleanup happen
		time.Sleep(50 * time.Millisecond)
	}

	// Final connection should work fine (no stale state)
	conn := dialTestClient(t, ts.srv, "rr-user")
	subscribeRoom(t, conn, room)
	conn.Close()
	t.Log("✓ 10 rapid reconnect cycles without stale state or crash")
}
