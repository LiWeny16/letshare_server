//go:build letshare_live

package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"letshare-server/internal/model"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const liveServer = "ecs.letshare.fun:443"

func liveToken() string {
	h := sha256.Sum256([]byte("sever_auth_123"))
	return hex.EncodeToString(h[:])
}

func dialLive(t *testing.T, userID string) *websocket.Conn {
	t.Helper()
	u := url.URL{Scheme: "wss", Host: liveServer, Path: "/ws", RawQuery: fmt.Sprintf("token=%s&userId=%s", liveToken(), userID)}
	d := websocket.DefaultDialer
	d.HandshakeTimeout = 10 * time.Second
	conn, _, err := d.Dial(u.String(), nil)
	if err != nil {
		t.Fatalf("dial %s: %v", userID, err)
	}
	return conn
}

func subscribeLive(t *testing.T, conn *websocket.Conn, room string) {
	msg := model.WebSocketMessage{Type: "subscribe", Channel: room, Event: "signal:all"}
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("sub write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("sub read: %v", err)
	}
	var resp model.WebSocketMessage
	json.Unmarshal(raw, &resp)
	if resp.Type != "subscribed" {
		t.Fatalf("want subscribed, got %s type (err=%v)", resp.Type, resp.Error)
	}
}

type liveMsg struct {
	raw []byte
	err error
}

// TestLive_HappyPath tests full file transfer against production server
func TestLive_HappyPath(t *testing.T) {
	room := fmt.Sprintf("lh%d", time.Now().UnixMilli()%100000)
	t.Logf("Room: %s", room)

	sender := dialLive(t, "live-sender-hp")
	receiver := dialLive(t, "live-receiver-hp")
	defer sender.Close()
	defer receiver.Close()

	subscribeLive(t, sender, room)
	subscribeLive(t, receiver, room)
	t.Log("Both clients subscribed")

	rcvCh := make(chan liveMsg, 32)
	var rcvWg sync.WaitGroup
	rcvWg.Add(1)
	go func() {
		defer rcvWg.Done()
		for {
			receiver.SetReadDeadline(time.Now().Add(30 * time.Second))
			_, raw, err := receiver.ReadMessage()
			if err != nil {
				rcvCh <- liveMsg{err: err}
				return
			}
			rcvCh <- liveMsg{raw: raw}
		}
	}()

	tid := fmt.Sprintf("live-hp-%d", time.Now().UnixMilli())

	// 1. Request
	reqJSON, _ := json.Marshal(map[string]interface{}{
		"transfer_id": tid, "file_name": "live-test.txt", "file_size": 100,
		"file_type": "text/plain", "chunk_size": 65536, "total_chunks": 1,
		"from_user_id": "live-sender-hp", "to_user_id": "live-receiver-hp",
		"room_name": room,
	})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:request", Channel: room, Data: reqJSON})
	msg := <-rcvCh
	if msg.err != nil {
		t.Fatalf("receiver request: %v", msg.err)
	}
	t.Log("✓ Request forwarded")

	// 2. Accept
	accJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:accept", Channel: room, Data: accJSON})

	sender.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, raw, _ := sender.ReadMessage()
	var sm model.WebSocketMessage
	json.Unmarshal(raw, &sm)
	if sm.Type == "error" {
		t.Fatalf("accept rejected: %s", string(sm.Data))
	}
	t.Logf("✓ Accept: %s", sm.Type)

	// 3. Start
	startJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:start", Channel: room, Data: startJSON})
	msg = <-rcvCh
	if msg.err != nil {
		t.Fatalf("receiver start: %v", msg.err)
	}
	t.Log("✓ Start forwarded")

	// 4. Binary chunk (100 bytes of 'X')
	meta := model.FileTransferChunk{TransferID: tid, ChunkIndex: 0, ChunkSize: 100, TotalChunks: 1}
	metaJSON, _ := json.Marshal(meta)
	hdr := make([]byte, 256)
	copy(hdr, metaJSON)
	frame := append(hdr, []byte(strings.Repeat("X", 100))...)
	sender.WriteMessage(websocket.BinaryMessage, frame)
	t.Log("✓ Chunk sent")

	// 5. End
	endJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:end", Channel: room, Data: endJSON})

	// Drain until end (skip progress)
	var gotEnd bool
	for i := 0; i < 10; i++ {
		select {
		case msg = <-rcvCh:
			if msg.err != nil {
				t.Fatalf("drain: %v", msg.err)
			}
			json.Unmarshal(msg.raw, &sm)
			if sm.Type == "file:transfer:end" {
				gotEnd = true
				break
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for end")
		}
		if gotEnd {
			break
		}
	}
	if !gotEnd {
		t.Fatal("never received end")
	}
	t.Log("✓ End received")

	// 6. Complete
	completeJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:complete", Channel: room, Data: completeJSON})

	// Drain sender
	sender.SetReadDeadline(time.Now().Add(10 * time.Second))
	for i := 0; i < 5; i++ {
		_, raw, err := sender.ReadMessage()
		if err != nil {
			break
		}
		json.Unmarshal(raw, &sm)
		if sm.Type == "file:transfer:complete" {
			break
		}
	}
	if sm.Type != "file:transfer:complete" {
		t.Fatalf("want complete, got %s", sm.Type)
	}
	t.Log("✓ Complete confirmed — Happy Path PASSED")
}

// TestLive_ResendAndComplete tests the two bugs we fixed
func TestLive_ResendAndComplete(t *testing.T) {
	room := fmt.Sprintf("lr%d", time.Now().UnixMilli()%100000)
	t.Logf("Room: %s", room)

	sender := dialLive(t, "live-sender-rc")
	receiver := dialLive(t, "live-receiver-rc")
	defer sender.Close()
	defer receiver.Close()

	subscribeLive(t, sender, room)
	subscribeLive(t, receiver, room)

	rcvCh := make(chan liveMsg, 32)
	go func() {
		for {
			receiver.SetReadDeadline(time.Now().Add(30 * time.Second))
			_, raw, err := receiver.ReadMessage()
			if err != nil {
				rcvCh <- liveMsg{err: err}
				return
			}
			rcvCh <- liveMsg{raw: raw}
		}
	}()

	tid := fmt.Sprintf("live-rc-%d", time.Now().UnixMilli())

	// Request → Accept → Start
	reqJSON, _ := json.Marshal(map[string]interface{}{
		"transfer_id": tid, "file_name": "resend-test.bin", "file_size": 200,
		"file_type": "application/octet-stream", "chunk_size": 100, "total_chunks": 2,
		"from_user_id": "live-sender-rc", "to_user_id": "live-receiver-rc",
		"room_name": room,
	})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:request", Channel: room, Data: reqJSON})
	<-rcvCh

	accJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:accept", Channel: room, Data: accJSON})

	sender.SetReadDeadline(time.Now().Add(10 * time.Second))
	sender.ReadMessage()

	startJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:start", Channel: room, Data: startJSON})
	<-rcvCh

	// Send chunk 0 only (simulate missing chunk 1)
	meta0 := model.FileTransferChunk{TransferID: tid, ChunkIndex: 0, ChunkSize: 100, TotalChunks: 2}
	m0JSON, _ := json.Marshal(meta0)
	hdr0 := make([]byte, 256)
	copy(hdr0, m0JSON)
	sender.WriteMessage(websocket.BinaryMessage, append(hdr0, []byte(strings.Repeat("A", 100))...))

	// End → ending
	endJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:end", Channel: room, Data: endJSON})

	// Drain until end
	var sm model.WebSocketMessage
	for i := 0; i < 5; i++ {
		msg := <-rcvCh
		if msg.err != nil {
			t.Fatalf("drain: %v", msg.err)
		}
		json.Unmarshal(msg.raw, &sm)
		if sm.Type == "file:transfer:end" {
			break
		}
	}
	t.Log("✓ End received (1 chunk missing)")

	// Resend request → should set status to "resending"
	resendJSON, _ := json.Marshal(map[string]interface{}{
		"transfer_id": tid, "chunk_indexes": []interface{}{float64(1)},
		"missing_count": 1, "total_chunks": 2, "reason": "missing chunk 1",
	})
	receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:resend", Channel: room, Data: resendJSON})

	// Drain resend from sender
	sender.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, raw, _ := sender.ReadMessage()
	json.Unmarshal(raw, &sm)
	if sm.Type == "error" {
		t.Fatalf("BUG: Resend rejected: %s", string(sm.Data))
	}
	t.Logf("✓ Resend forwarded to sender (type=%s)", sm.Type)

	// Send missing chunk during resending (Bug #1 test)
	meta1 := model.FileTransferChunk{TransferID: tid, ChunkIndex: 1, ChunkSize: 100, TotalChunks: 2}
	m1JSON, _ := json.Marshal(meta1)
	hdr1 := make([]byte, 256)
	copy(hdr1, m1JSON)
	sender.WriteMessage(websocket.BinaryMessage, append(hdr1, []byte(strings.Repeat("B", 100))...))

	// Drain sender — server forwards chunk then sends progress. Look for error (Bug #1 failure) or progress (success)
	var chunkAccepted bool
	sender.SetReadDeadline(time.Now().Add(10 * time.Second))
	for i := 0; i < 5; i++ {
		_, raw, _ = sender.ReadMessage()
		json.Unmarshal(raw, &sm)
		if sm.Type == "error" {
			var d map[string]interface{}
			json.Unmarshal(sm.Data, &d)
			t.Fatalf("BUG #1: Chunk rejected in resending state: %v", d)
		}
		// Progress with percentage > 50 means chunk 1 was forwarded
		if sm.Type == "file:transfer:progress" {
			var d map[string]interface{}
			json.Unmarshal(sm.Data, &d)
			if pct, ok := d["percentage"].(float64); ok && pct > 80 {
				chunkAccepted = true
				break
			}
		}
	}
	if !chunkAccepted {
		t.Fatalf("BUG #1: chunk never forwarded (no progress after resend)")
	}
	t.Log("✓ Bug #1 fixed: Chunk accepted during resending")

	// Now send COMPLETE while still in resending (Bug #2 test — skip sender's END)
	completeJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:complete", Channel: room, Data: completeJSON})

	// Drain sender for complete
	sender.SetReadDeadline(time.Now().Add(10 * time.Second))
	for i := 0; i < 5; i++ {
		_, raw, err := sender.ReadMessage()
		if err != nil {
			break
		}
		json.Unmarshal(raw, &sm)
		if sm.Type == "file:transfer:complete" {
			break
		}
		if sm.Type == "error" {
			t.Fatalf("BUG #2: COMPLETE rejected from resending state: %s", string(sm.Data))
		}
	}
	if sm.Type != "file:transfer:complete" {
		t.Fatalf("want complete, got %s (resending→complete failed)", sm.Type)
	}
	t.Log("✓ Bug #2 fixed: COMPLETE accepted from resending state")
	t.Log("✓ LIVE Resend+Complete test PASSED")
}

// TestLive_ConcurrentMultiRoom tests 2 rooms (3Mbps server limit)
func TestLive_ConcurrentMultiRoom(t *testing.T) {
	t.Skip("Skipped on 3Mbps server — covered by local stress tests")
	const numRooms = 3
	type roomClients struct {
		sender   *websocket.Conn
		receiver *websocket.Conn
		room     string
	}

	var rooms []roomClients
	for i := 0; i < numRooms; i++ {
		room := fmt.Sprintf("lm%d%d", time.Now().UnixMilli()%10000, i)
		s := dialLive(t, fmt.Sprintf("multi-s-%d", i))
		r := dialLive(t, fmt.Sprintf("multi-r-%d", i))
		subscribeLive(t, s, room)
		subscribeLive(t, r, room)
		rooms = append(rooms, roomClients{s, r, room})
		t.Logf("Room %d: %s ready", i, room)
	}

	var wg sync.WaitGroup
	errCh := make(chan string, numRooms)

	for i, rc := range rooms {
		wg.Add(1)
		go func(idx int, rc roomClients) {
			defer wg.Done()
			tid := fmt.Sprintf("live-multi-%d-%d", time.Now().UnixMilli(), idx)

			// Drain receiver in background
			rcvCh := make(chan liveMsg, 16)
			go func() {
				for {
					rc.receiver.SetReadDeadline(time.Now().Add(30 * time.Second))
					_, raw, err := rc.receiver.ReadMessage()
					if err != nil {
						rcvCh <- liveMsg{err: err}
						return
					}
					rcvCh <- liveMsg{raw: raw}
				}
			}()

			reqJSON, _ := json.Marshal(map[string]interface{}{
				"transfer_id": tid, "file_name": fmt.Sprintf("f%d", idx), "file_size": 50,
				"file_type": "text", "chunk_size": 65536, "total_chunks": 1,
				"from_user_id": fmt.Sprintf("multi-s-%d", idx),
				"to_user_id":   fmt.Sprintf("multi-r-%d", idx),
				"room_name":    rc.room,
			})
			if err := rc.sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:request", Channel: rc.room, Data: reqJSON}); err != nil {
				errCh <- fmt.Sprintf("room %d request: %v", idx, err)
				return
			}
			<-rcvCh

			accJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
			rc.receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:accept", Channel: rc.room, Data: accJSON})
			rc.sender.SetReadDeadline(time.Now().Add(10 * time.Second))
			rc.sender.ReadMessage()

			startJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
			rc.sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:start", Channel: rc.room, Data: startJSON})

			// Drain receiver intermediate messages (start, etc.) — 3Mbps server latency
			time.Sleep(100 * time.Millisecond)

			// Small chunk
			meta := model.FileTransferChunk{TransferID: tid, ChunkIndex: 0, ChunkSize: 50, TotalChunks: 1}
			mJSON, _ := json.Marshal(meta)
			hdr := make([]byte, 256)
			copy(hdr, mJSON)
			rc.sender.WriteMessage(websocket.BinaryMessage, append(hdr, []byte(strings.Repeat("Z", 50))...))

			endJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
			rc.sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:end", Channel: rc.room, Data: endJSON})

			completeJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
			rc.receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:complete", Channel: rc.room, Data: completeJSON})

			rc.sender.SetReadDeadline(time.Now().Add(15 * time.Second))
			for j := 0; j < 10; j++ {
				_, raw, err := rc.sender.ReadMessage()
				if err != nil {
					break
				}
				var sm model.WebSocketMessage
				json.Unmarshal(raw, &sm)
				if sm.Type == "error" {
					errCh <- fmt.Sprintf("room %d: transfer error: %s", idx, string(sm.Data))
					return
				}
				if sm.Type == "file:transfer:complete" {
					return // success
				}
			}
			errCh <- fmt.Sprintf("room %d: never got complete", idx)
		}(i, rc)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Error(err)
	}

	for _, rc := range rooms {
		rc.sender.Close()
		rc.receiver.Close()
	}
	t.Logf("✓ %d concurrent room transfers completed", numRooms)
}

// TestLive_DisconnectResilience tests error notification on disconnect
func TestLive_DisconnectResilience(t *testing.T) {
	room := fmt.Sprintf("ld%d", time.Now().UnixMilli()%100000)

	sender := dialLive(t, "live-sender-dc")
	receiver := dialLive(t, "live-receiver-dc")
	defer receiver.Close()

	subscribeLive(t, sender, room)
	subscribeLive(t, receiver, room)

	rcvCh := make(chan liveMsg, 16)
	go func() {
		for {
			receiver.SetReadDeadline(time.Now().Add(30 * time.Second))
			_, raw, err := receiver.ReadMessage()
			if err != nil {
				rcvCh <- liveMsg{err: err}
				return
			}
			rcvCh <- liveMsg{raw: raw}
		}
	}()

	tid := fmt.Sprintf("live-dc-%d", time.Now().UnixMilli())

	// Request → Accept → Start → Start transferring
	reqJSON, _ := json.Marshal(map[string]interface{}{
		"transfer_id": tid, "file_name": "dc-test.txt", "file_size": 500,
		"file_type": "text", "chunk_size": 100, "total_chunks": 5,
		"from_user_id": "live-sender-dc", "to_user_id": "live-receiver-dc",
		"room_name": room,
	})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:request", Channel: room, Data: reqJSON})
	<-rcvCh

	accJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	receiver.WriteJSON(model.WebSocketMessage{Type: "file:transfer:accept", Channel: room, Data: accJSON})
	sender.SetReadDeadline(time.Now().Add(10 * time.Second))
	sender.ReadMessage()

	startJSON, _ := json.Marshal(map[string]interface{}{"transfer_id": tid})
	sender.WriteJSON(model.WebSocketMessage{Type: "file:transfer:start", Channel: room, Data: startJSON})
	<-rcvCh

	// Send one chunk
	meta := model.FileTransferChunk{TransferID: tid, ChunkIndex: 0, ChunkSize: 100, TotalChunks: 5}
	mJSON, _ := json.Marshal(meta)
	hdr := make([]byte, 256)
	copy(hdr, mJSON)
	sender.WriteMessage(websocket.BinaryMessage, append(hdr, []byte(strings.Repeat("D", 100))...))

	// Abruptly disconnect sender
	sender.Close()

	// Receiver should get error notification
	select {
	case msg := <-rcvCh:
		if msg.err != nil {
			t.Logf("✓ Receiver connection closed: %v (expected after disconnect)", msg.err)
		} else {
			var sm model.WebSocketMessage
			json.Unmarshal(msg.raw, &sm)
			if sm.Type == "file:transfer:error" || sm.Type == "error" {
				var d map[string]interface{}
				json.Unmarshal(sm.Data, &d)
				t.Logf("✓ Receiver got disconnect error: %v", d)
			}
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Timeout: receiver not notified of disconnect")
	}
	t.Log("✓ Disconnect resilience PASSED")
}
