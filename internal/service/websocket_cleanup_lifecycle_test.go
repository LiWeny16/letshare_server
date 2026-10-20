package service

import (
	"sync/atomic"
	"testing"
	"time"

	"letshare-server/internal/model"
)

func TestWebSocketShutdownPreservesClaimedDisconnectCallback(t *testing.T) {
	ws := NewWebSocketService(10)
	t.Cleanup(ws.Shutdown)

	clientID := "shutdown-claimed-disconnect"
	client := model.NewClient(clientID, "shutdown-race-user", nil)
	ws.AddClient(client)

	var callbackCalls atomic.Int32
	callbackIDs := make(chan string, 2)
	ws.SetOnClientDisconnect(func(id string) {
		callbackCalls.Add(1)
		callbackIDs <- id
	})

	client.ConnMutex.Lock()
	locked := true
	removeDone := make(chan struct{})
	t.Cleanup(func() {
		if locked {
			client.ConnMutex.Unlock()
			locked = false
		}
		select {
		case <-removeDone:
		case <-time.After(time.Second):
			t.Error("RemoveClient goroutine did not finish during cleanup")
		}
	})

	go func() {
		defer close(removeDone)
		ws.RemoveClient(clientID)
	}()

	removed := time.NewTimer(5 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer removed.Stop()
	defer ticker.Stop()
	for {
		if _, exists := ws.GetClient(clientID); !exists {
			break
		}
		select {
		case <-ticker.C:
		case <-removed.C:
			t.Fatal("RemoveClient did not remove the client before blocking on ConnMutex")
		}
	}

	// Shutdown observes an empty client map while the already-claimed removal
	// is blocked on ConnMutex. The registered disconnect callback must survive.
	ws.Shutdown()
	client.ConnMutex.Unlock()
	locked = false

	select {
	case <-removeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("RemoveClient did not finish after ConnMutex was released")
	}

	select {
	case got := <-callbackIDs:
		if got != clientID {
			t.Fatalf("disconnect callback ID = %q, want %q", got, clientID)
		}
	case <-time.After(time.Second):
		t.Fatal("registered disconnect callback was not invoked")
	}
	if got := callbackCalls.Load(); got != 1 {
		t.Fatalf("disconnect callback invoked %d times, want exactly once", got)
	}
}
