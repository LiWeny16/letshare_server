package service

import (
	"testing"
	"time"
)

func TestWebSocketServiceShutdownStopsMaintenance(t *testing.T) {
	ws := NewWebSocketService(10)
	ws.Shutdown()

	select {
	case <-ws.maintenanceDone:
	case <-time.After(time.Second):
		t.Fatal("WebSocket maintenance worker did not stop")
	}

	// Shutdown is intentionally idempotent so signal handling and test cleanup
	// can safely converge on the same lifecycle boundary.
	ws.Shutdown()
}

func TestFileTransferServiceShutdownStopsCleanup(t *testing.T) {
	ws := NewWebSocketService(10)
	fts := NewFileTransferService(ws, 3*1024*1024*1024, 64*1024)
	fts.Shutdown()

	select {
	case <-fts.cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("file transfer cleanup worker did not stop")
	}

	fts.Shutdown()
	ws.Shutdown()
}
