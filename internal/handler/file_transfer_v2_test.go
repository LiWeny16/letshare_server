package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"letshare-server/internal/model"
	"letshare-server/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func TestFileTransferV2WebSocketControlAndBinaryRelay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("LETSHARE_FILE_V2_SPOOL_PER_TRANSFER", strconv.FormatInt(64*1024*1024, 10))
	t.Setenv("LETSHARE_FILE_V2_SPOOL_GLOBAL", strconv.FormatInt(512*1024*1024, 10))
	wsService := service.NewWebSocketService(10)
	fileService := service.NewFileTransferService(wsService, 3*1024*1024*1024, 64*1024)
	fileService.V2().SetRoot(t.TempDir())
	authService := service.NewAuthService()
	handler := NewWebSocketHandler(wsService, authService, fileService, nil)
	router := gin.New()
	router.GET("/ws", handler.HandleWebSocket)
	server := httptest.NewServer(router)
	defer server.Close()
	t.Cleanup(fileService.Shutdown)
	t.Cleanup(wsService.Shutdown)

	connect := func(identity string) *websocket.Conn {
		t.Helper()
		endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws?token=" + url.QueryEscape(testToken()) + "&userId=" + url.QueryEscape(identity) + "&uniqId=" + url.QueryEscape(identity)
		conn, response, err := websocket.DefaultDialer.Dial(endpoint, nil)
		if err != nil {
			if response != nil {
				_ = response.Body.Close()
			}
			t.Fatalf("connect %s: %v", identity, err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		return conn
	}
	readControl := func(conn *websocket.Conn, wantKind string) model.FileTransferV2Control {
		t.Helper()
		for {
			messageType, data, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("read %s control: %v", wantKind, err)
			}
			if messageType != websocket.TextMessage {
				continue
			}
			var message model.WebSocketMessage
			if json.Unmarshal(data, &message) != nil || message.Type != model.MessageTypeFileTransferV2Control {
				continue
			}
			var control model.FileTransferV2Control
			if json.Unmarshal(message.Data, &control) == nil && control.Kind == wantKind {
				return control
			}
		}
	}
	int64Pointer := func(value int64) *int64 { return &value }
	rangesPointer := func(value [][]int) *[][]int { return &value }
	subscribe := func(conn *websocket.Conn) {
		t.Helper()
		message := model.NewWebSocketMessage(model.MessageTypeSubscribe, "v2-room", "", nil)
		if err := conn.WriteJSON(message); err != nil {
			t.Fatal(err)
		}
	}

	sender := connect("alice-device")
	receiver := connect("bob-device")
	defer sender.Close()
	defer receiver.Close()

	hello := model.NewWebSocketMessage(model.MessageTypeFileTransferV2Control, "", "", model.FileTransferV2Control{Version: 2, Kind: "hello", From: "alice-device", To: "server"})
	if err := sender.WriteJSON(hello); err != nil {
		t.Fatal(err)
	}
	ack := readControl(sender, "hello-ack")
	if ack.From != "server" || ack.To != "alice-device" || ack.Capabilities == nil || ack.Capabilities.MaxFileSize != service.NonProSizeLimit || ack.Capabilities.MaxFrameBytes != 17414 || ack.Capabilities.MaxCreditBytes != 8388608 {
		t.Fatalf("unexpected v2 capability response: %+v", ack)
	}
	subscribe(sender)
	subscribe(receiver)

	contentLength := service.FileTransferV2BlockSize + 37
	content := bytes.Repeat([]byte("authenticated relay block"), contentLength/len("authenticated relay block")+1)[:contentLength]
	fileHash := sha256.Sum256(content)
	manifest := model.FileTransferV2Manifest{
		TransferID: "relay_test_0001", Name: "relay.txt", Size: int64(len(content)), Type: "text/plain",
		BlockSize: service.FileTransferV2BlockSize, TotalBlocks: (len(content) + service.FileTransferV2BlockSize - 1) / service.FileTransferV2BlockSize,
		FileHash: hex.EncodeToString(fileHash[:]), CreatedAt: time.Now().UnixMilli(),
	}
	offer := model.FileTransferV2Control{Version: 2, Kind: "offer", TransferID: manifest.TransferID, From: "spoofed-sender", To: "bob-device", Manifest: &manifest}
	if err := sender.WriteJSON(model.NewWebSocketMessage(model.MessageTypeFileTransferV2Control, "v2-room", "", offer)); err != nil {
		t.Fatal(err)
	}
	forwardedOffer := readControl(receiver, "offer")
	if forwardedOffer.From != "alice-device" || forwardedOffer.To != "bob-device" {
		t.Fatalf("offer identity was not bound to authenticated socket: %+v", forwardedOffer)
	}
	// Retries over an attached route are idempotent and remain reachable through the WS read loop.
	if err := sender.WriteJSON(model.NewWebSocketMessage(model.MessageTypeFileTransferV2Control, "v2-room", "", offer)); err != nil {
		t.Fatal(err)
	}
	if duplicate := readControl(receiver, "offer"); duplicate.TransferID != manifest.TransferID {
		t.Fatalf("duplicate offer mismatch: %+v", duplicate)
	}
	if err := sender.WriteJSON(model.NewWebSocketMessage(model.MessageTypeFileTransferV2Control, "v2-room", "", model.FileTransferV2Control{Version: 2, Kind: "accept", TransferID: manifest.TransferID, From: "alice-device", To: "bob-device"})); err != nil {
		t.Fatal(err)
	}
	if unauthorized := readControl(sender, "error"); !strings.Contains(unauthorized.Reason, "only the receiver") {
		t.Fatalf("sender acceptance was not rejected: %+v", unauthorized)
	}
	accept := model.FileTransferV2Control{Version: 2, Kind: "accept", TransferID: manifest.TransferID, From: "spoofed-receiver", To: "alice-device"}
	if err := receiver.WriteJSON(model.NewWebSocketMessage(model.MessageTypeFileTransferV2Control, "v2-room", "", accept)); err != nil {
		t.Fatal(err)
	}
	if accepted := readControl(sender, "accept"); accepted.From != "bob-device" || accepted.CreditBytes != nil || accepted.Ranges != nil {
		t.Fatalf("receiver identity or omitted optional fields were not preserved: %+v", accepted)
	}

	writeAndReadBlock := func(index int, block []byte) {
		t.Helper()
		blockHash := sha256.Sum256(block)
		hash := hex.EncodeToString(blockHash[:])
		for offset := 0; offset < len(block); offset += service.FileTransferV2MaxFramePayload {
			end := offset + service.FileTransferV2MaxFramePayload
			if end > len(block) {
				end = len(block)
			}
			header, err := json.Marshal(fileTransferV2FrameHeader{
				Version: 2, TransferID: manifest.TransferID, Index: index,
				Offset: offset, TotalLength: len(block), Hash: hash,
			})
			if err != nil {
				t.Fatal(err)
			}
			frame := make([]byte, 6+len(header)+end-offset)
			copy(frame[:4], "LSF2")
			binary.BigEndian.PutUint16(frame[4:6], uint16(len(header)))
			copy(frame[6:], header)
			copy(frame[6+len(header):], block[offset:end])
			if err := sender.WriteMessage(websocket.BinaryMessage, frame); err != nil {
				t.Fatal(err)
			}
		}
		for offset := 0; offset < len(block); offset += service.FileTransferV2MaxFramePayload {
			end := offset + service.FileTransferV2MaxFramePayload
			if end > len(block) {
				end = len(block)
			}
			messageType, relayedFrame, err := receiver.ReadMessage()
			if err != nil || messageType != websocket.BinaryMessage || len(relayedFrame) < 6 || string(relayedFrame[:4]) != "LSF2" {
				t.Fatalf("invalid forwarded LSF2 frame: type=%d err=%v data=%x", messageType, err, relayedFrame)
			}
			headerLength := int(binary.BigEndian.Uint16(relayedFrame[4:6]))
			if headerLength == 0 || len(relayedFrame) < 6+headerLength {
				t.Fatalf("invalid forwarded LSF2 header length: %d", headerLength)
			}
			var relayedHeader fileTransferV2FrameHeader
			if err := json.Unmarshal(relayedFrame[6:6+headerLength], &relayedHeader); err != nil {
				t.Fatalf("decode forwarded LSF2 header: %v", err)
			}
			if relayedHeader.Index != index || relayedHeader.Offset != offset || relayedHeader.TotalLength != len(block) || !bytes.Equal(relayedFrame[6+headerLength:], block[offset:end]) {
				t.Fatalf("invalid forwarded block fragment: header=%+v payload=%d expectedOffset=%d", relayedHeader, len(relayedFrame)-6-headerLength, offset)
			}
		}
	}

	firstBlock := content[:service.FileTransferV2BlockSize]
	writeAndReadBlock(0, firstBlock)
	transferring, err := fileService.V2().Get(manifest.TransferID)
	if err != nil || transferring.Status != "transferring" || len(transferring.Blocks) != 1 {
		t.Fatalf("first relay block did not persist transferring state: task=%+v err=%v", transferring, err)
	}
	// A reconnect or second route may replay both controls during an active
	// transfer. The offer is forwarded idempotently and accept must not downgrade
	// the task to accepted or interrupt the next relay block.
	if err := sender.WriteJSON(model.NewWebSocketMessage(model.MessageTypeFileTransferV2Control, "v2-room", "", offer)); err != nil {
		t.Fatal(err)
	}
	if duplicate := readControl(receiver, "offer"); duplicate.TransferID != manifest.TransferID {
		t.Fatalf("mid-transfer duplicate offer mismatch: %+v", duplicate)
	}
	zeroCredit := int64(0)
	emptyRanges := make([][]int, 0)
	repeatedAccept := accept
	repeatedAccept.CreditBytes = &zeroCredit
	repeatedAccept.Ranges = &emptyRanges
	if err := receiver.WriteJSON(model.NewWebSocketMessage(model.MessageTypeFileTransferV2Control, "v2-room", "", repeatedAccept)); err != nil {
		t.Fatal(err)
	}
	if forwardedAccept := readControl(sender, "accept"); forwardedAccept.From != "bob-device" || forwardedAccept.CreditBytes == nil || *forwardedAccept.CreditBytes != 0 || forwardedAccept.Ranges == nil || *forwardedAccept.Ranges == nil || len(*forwardedAccept.Ranges) != 0 {
		t.Fatalf("mid-transfer accept lost explicit zero credit or empty ranges: %+v", forwardedAccept)
	}
	stillTransferring, err := fileService.V2().Get(manifest.TransferID)
	if err != nil || stillTransferring.Status != "transferring" || len(stillTransferring.Blocks) != 1 {
		t.Fatalf("repeated acceptance downgraded active transfer: task=%+v err=%v", stillTransferring, err)
	}
	writeAndReadBlock(1, content[service.FileTransferV2BlockSize:])

	wrongCheckpoint := model.FileTransferV2Control{Version: 2, Kind: "checkpoint", TransferID: manifest.TransferID, From: "alice-device", To: "bob-device", Ranges: rangesPointer([][]int{{0, 1}})}
	if err := sender.WriteJSON(model.NewWebSocketMessage(model.MessageTypeFileTransferV2Control, "v2-room", "", wrongCheckpoint)); err != nil {
		t.Fatal(err)
	}
	if unauthorized := readControl(sender, "error"); !strings.Contains(unauthorized.Reason, "only the receiver") {
		t.Fatalf("sender checkpoint was not rejected: %+v", unauthorized)
	}
	emptyCheckpoint := model.FileTransferV2Control{Version: 2, Kind: "checkpoint", TransferID: manifest.TransferID, From: "spoofed-receiver", To: "alice-device", Ranges: &emptyRanges, CreditBytes: int64Pointer(0)}
	if err := receiver.WriteJSON(model.NewWebSocketMessage(model.MessageTypeFileTransferV2Control, "v2-room", "", emptyCheckpoint)); err != nil {
		t.Fatal(err)
	}
	forwardedEmptyCheckpoint := readControl(sender, "checkpoint")
	if forwardedEmptyCheckpoint.From != "bob-device" || forwardedEmptyCheckpoint.CreditBytes == nil || *forwardedEmptyCheckpoint.CreditBytes != 0 || forwardedEmptyCheckpoint.Ranges == nil || *forwardedEmptyCheckpoint.Ranges == nil || len(*forwardedEmptyCheckpoint.Ranges) != 0 {
		t.Fatalf("checkpoint lost explicit zero credit or empty ranges: %+v", forwardedEmptyCheckpoint)
	}
	checkpoint := model.FileTransferV2Control{Version: 2, Kind: "checkpoint", TransferID: manifest.TransferID, From: "spoofed-receiver", To: "alice-device", Ranges: rangesPointer([][]int{{0, 1}})}
	if err := receiver.WriteJSON(model.NewWebSocketMessage(model.MessageTypeFileTransferV2Control, "v2-room", "", checkpoint)); err != nil {
		t.Fatal(err)
	}
	if forwarded := readControl(sender, "checkpoint"); forwarded.From != "bob-device" {
		t.Fatalf("checkpoint was not forwarded from receiver: %+v", forwarded)
	}
	task, err := fileService.V2().Get(manifest.TransferID)
	if err != nil || len(task.Blocks) != 0 || len(task.Checkpoint) != 1 {
		t.Fatalf("receiver checkpoint did not release spool: task=%+v err=%v", task, err)
	}
	complete := model.FileTransferV2Control{Version: 2, Kind: "complete", TransferID: manifest.TransferID, From: "bob-device", To: "alice-device"}
	for retry := 0; retry < 2; retry++ {
		if err := receiver.WriteJSON(model.NewWebSocketMessage(model.MessageTypeFileTransferV2Control, "v2-room", "", complete)); err != nil {
			t.Fatal(err)
		}
		if forwarded := readControl(sender, "complete"); forwarded.From != "bob-device" {
			t.Fatalf("completion retry was not forwarded idempotently: %+v", forwarded)
		}
	}
	completed, err := fileService.V2().Get(manifest.TransferID)
	if err != nil || completed.Status != "completed" || len(completed.Blocks) != 0 {
		t.Fatalf("terminal receipt was not retained without relay payload: task=%+v err=%v", completed, err)
	}
	if err := receiver.WriteJSON(model.NewWebSocketMessage(model.MessageTypeFileTransferV2Control, "v2-room", "", model.FileTransferV2Control{
		Version: 2, Kind: "checkpoint", TransferID: manifest.TransferID, From: "bob-device", To: "alice-device", Ranges: rangesPointer([][]int{{0, 1}}),
	})); err != nil {
		t.Fatal(err)
	}
	if rejected := readControl(receiver, "error"); !strings.Contains(rejected.Reason, "current state") {
		t.Fatalf("late completed checkpoint was not rejected: %+v", rejected)
	}
	if err := receiver.WriteJSON(model.NewWebSocketMessage(model.MessageTypeFileTransferV2Control, "v2-room", "", model.FileTransferV2Control{
		Version: 2, Kind: "cancel", TransferID: manifest.TransferID, From: "bob-device", To: "alice-device",
	})); err != nil {
		t.Fatal(err)
	}
	if rejected := readControl(receiver, "error"); !strings.Contains(rejected.Reason, "current state") {
		t.Fatalf("late completed cancellation was not rejected: %+v", rejected)
	}
	completedAfterLateControls, err := fileService.V2().Get(manifest.TransferID)
	if err != nil || completedAfterLateControls.Status != "completed" || len(completedAfterLateControls.Checkpoint) != 1 || completedAfterLateControls.Checkpoint[0][0] != 0 || completedAfterLateControls.Checkpoint[0][1] != 1 || len(completedAfterLateControls.Blocks) != 0 {
		t.Fatalf("late WS controls mutated completed receipt: task=%+v err=%v", completedAfterLateControls, err)
	}

	// Exercise a stale handler snapshot: the offer is persisted and forwarded,
	// cancellation wins, then a delayed receiver acceptance must not revive it.
	cancelManifest := manifest
	cancelManifest.TransferID = "relay_test_cancel_01"
	cancelOffer := model.FileTransferV2Control{Version: 2, Kind: "offer", TransferID: cancelManifest.TransferID, From: "alice-device", To: "bob-device", Manifest: &cancelManifest}
	if err := sender.WriteJSON(model.NewWebSocketMessage(model.MessageTypeFileTransferV2Control, "v2-room", "", cancelOffer)); err != nil {
		t.Fatal(err)
	}
	if forwarded := readControl(receiver, "offer"); forwarded.TransferID != cancelManifest.TransferID {
		t.Fatalf("cancel race offer mismatch: %+v", forwarded)
	}
	if err := sender.WriteJSON(model.NewWebSocketMessage(model.MessageTypeFileTransferV2Control, "v2-room", "", model.FileTransferV2Control{
		Version: 2, Kind: "cancel", TransferID: cancelManifest.TransferID, From: "alice-device", To: "bob-device",
	})); err != nil {
		t.Fatal(err)
	}
	if cancelled := readControl(receiver, "cancel"); cancelled.TransferID != cancelManifest.TransferID {
		t.Fatalf("cancel was not forwarded: %+v", cancelled)
	}
	if err := receiver.WriteJSON(model.NewWebSocketMessage(model.MessageTypeFileTransferV2Control, "v2-room", "", model.FileTransferV2Control{
		Version: 2, Kind: "accept", TransferID: cancelManifest.TransferID, From: "bob-device", To: "alice-device",
	})); err != nil {
		t.Fatal(err)
	}
	if rejected := readControl(receiver, "error"); !strings.Contains(rejected.Reason, "current state") {
		t.Fatalf("late acceptance resurrected cancelled offer: %+v", rejected)
	}
	cancelledTask, err := fileService.V2().Get(cancelManifest.TransferID)
	if err != nil || cancelledTask.Status != "cancelled" || len(cancelledTask.Blocks) != 0 {
		t.Fatalf("late acceptance changed cancelled receipt: task=%+v err=%v", cancelledTask, err)
	}
	// Shut down with the test-owned WebSocket clients still registered so
	// service cleanup owns their disconnect callbacks before the HTTP server's
	// deferred Close races the handler goroutines.
	wsService.Shutdown()
}
