package service

import (
	"fmt"
	"letshare-server/internal/model"
	"strings"
	"testing"
)

const (
	testMaxFileSize = 3 * 1024 * 1024 * 1024
	testChunkSize   = 65536
)

func newTestFTS() *FileTransferService {
	ws := NewWebSocketService(10)
	return NewFileTransferService(ws, testMaxFileSize, testChunkSize)
}

func validRequest() *model.FileTransferRequest {
	return &model.FileTransferRequest{
		TransferID: "tf-test-001",
		FileName:   "test.txt",
		FileSize:   10 * 1024 * 1024,
		FileType:   "text/plain",
		ChunkSize:  65536,
		FromUserID: "sender-1",
		ToUserID:   "receiver-1",
		RoomName:   "room-test",
	}
}

func createSession(t *testing.T, fts *FileTransferService, req *model.FileTransferRequest, isPro bool) *model.FileTransferSession {
	t.Helper()
	session, err := fts.CreateTransferSession(req, isPro)
	if err != nil {
		t.Fatalf("CreateTransferSession(%q): unexpected error: %v", req.TransferID, err)
	}
	return session
}

func TestCreateTransferSession_ProLimits(t *testing.T) {
	tests := []struct {
		name        string
		fileSize    int64
		isPro       bool
		wantErr     bool
		errContains string
	}{
		{name: "nonPRO 10MB allowed", fileSize: 10 * 1024 * 1024, isPro: false},
		{name: "nonPRO exactly 50MB", fileSize: 50 * 1024 * 1024, isPro: false},
		{name: "nonPRO 50MB+1 rejected", fileSize: 50*1024*1024 + 1, isPro: false, wantErr: true, errContains: "升级到 PRO"},
		{name: "nonPRO 100MB rejected", fileSize: 100 * 1024 * 1024, isPro: false, wantErr: true, errContains: "升级到 PRO"},
		{name: "nonPRO 1 byte", fileSize: 1, isPro: false},
		{name: "nonPRO 0 bytes", fileSize: 0, isPro: false},
		{name: "PRO 100MB allowed", fileSize: 100 * 1024 * 1024, isPro: true},
		{name: "PRO 500MB allowed", fileSize: 500 * 1024 * 1024, isPro: true},
		{name: "PRO 1GB allowed", fileSize: 1 * 1024 * 1024 * 1024, isPro: true},
		{name: "PRO exactly 3GB", fileSize: 3 * 1024 * 1024 * 1024, isPro: true},
		{name: "PRO 3.5GB rejected", fileSize: 3*1024*1024*1024 + 512*1024*1024, isPro: true, wantErr: true, errContains: "超过限制"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fts := newTestFTS()
			req := &model.FileTransferRequest{
				TransferID: "tf-pro-" + strings.ReplaceAll(tt.name, " ", "-"),
				FileName:   "pro-test.bin",
				FileSize:   tt.fileSize,
				FileType:   "application/octet-stream",
				ChunkSize:  65536,
				FromUserID: "sender-1",
				ToUserID:   "receiver-1",
				RoomName:   "room-pro",
			}
			session, err := fts.CreateTransferSession(req, tt.isPro)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), tt.errContains) {
					t.Fatalf("expected error containing %q, got: %v", tt.errContains, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if session == nil || session.TransferID != req.TransferID {
				t.Error("bad session")
			}
		})
	}
}

func TestUpdateSessionStatus_ResendFromTransferring(t *testing.T) {
	fts := newTestFTS()
	req := validRequest()
	req.TransferID = "tf-resend"
	createSession(t, fts, req, true)
	fts.UpdateSessionStatus("tf-resend", "pending", "accepted")
	fts.UpdateSessionStatus("tf-resend", "accepted", "transferring")
	if err := fts.UpdateSessionStatus("tf-resend", "transferring", "resending"); err != nil {
		t.Fatalf("transferring -> resending: %v", err)
	}
}

func TestUpdateSessionStatus_AllTransitions(t *testing.T) {
	fts := newTestFTS()
	tests := []struct{ setup, expected, new string; wantErr bool }{
		{"pending", "pending", "accepted", false},
		{"accepted", "accepted", "transferring", false},
		{"transferring", "transferring", "ending", false},
		{"ending", "ending", "resending", false},
		{"transferring", "transferring", "resending", false},
		{"resending", "resending", "transferring", false},
		{"resending", "resending", "completed", false},
		{"ending", "ending", "completed", false},
		{"transferring", "transferring", "cancelled", false},
		{"transferring", "transferring", "error", false},
		{"pending", "accepted", "transferring", true},
	}
	for _, tt := range tests {
		t.Run(tt.setup+"->"+tt.new, func(t *testing.T) {
			id := "tf-trans-" + tt.setup + "-" + tt.new
			req := validRequest()
			req.TransferID = id
			createSession(t, fts, req, true)
			if tt.setup != "pending" {
				fts.UpdateSessionStatus(id, "", tt.setup)
			}
			err := fts.UpdateSessionStatus(id, tt.expected, tt.new)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestUpdateSessionStatus_NonExistentSession(t *testing.T) {
	fts := newTestFTS()
	if err := fts.UpdateSessionStatus("nope", "pending", "accepted"); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatal("expected 'exists not' error")
	}
}

func TestFileTransferSession_Lifecycle(t *testing.T) {
	fts := newTestFTS()
	req := validRequest()
	req.TransferID = "tf-lifecycle"
	createSession(t, fts, req, true)
	fts.UpdateSessionStatus("tf-lifecycle", "pending", "accepted")
	fts.UpdateSessionClients("tf-lifecycle", "c1", "c2")
	s, _ := fts.GetSession("tf-lifecycle")
	if s.Status != "accepted" || s.FromClientID != "c1" {
		t.Fatal("state mismatch")
	}
	fts.RemoveSession("tf-lifecycle")
	if _, err := fts.GetSession("tf-lifecycle"); err == nil {
		t.Fatal("expected error after remove")
	}
}

func TestCreateTransferSession_RequiredFields(t *testing.T) {
	fts := newTestFTS()
	tests := []struct{ from, to, errMsg string }{
		{"", "", "发送者"},
		{"", "r", "发送者"},
		{"s", "", "接收者"},
	}
	for _, tt := range tests {
		req := validRequest()
		req.FromUserID = tt.from
		req.ToUserID = tt.to
		_, err := fts.CreateTransferSession(req, false)
		if err == nil || !strings.Contains(err.Error(), tt.errMsg) {
			t.Fatalf("expected %q, got: %v", tt.errMsg, err)
		}
	}
}

func TestCreateTransferSession_ChunkCalculation(t *testing.T) {
	fts := newTestFTS()
	tests := []struct {
		name       string
		fileSize   int64
		chunkSize  int
		wantChunks int
	}{
		{"exact", 65536, 65536, 1},
		{"one more", 65537, 65536, 2},
		{"half", 32768, 65536, 1},
		{"zero", 0, 65536, 1},
		{"default", 10 * 1024 * 1024, 0, int(10*1024*1024) / testChunkSize},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validRequest()
			req.FileSize = tt.fileSize
			req.ChunkSize = tt.chunkSize
			s := createSession(t, fts, req, false)
			if s.TotalChunks != tt.wantChunks {
				t.Fatalf("TotalChunks=%d want=%d", s.TotalChunks, tt.wantChunks)
			}
		})
	}
}

func TestGetStats_SessionCounts(t *testing.T) {
	fts := newTestFTS()
	for i := 0; i < 3; i++ {
		req := validRequest()
		req.TransferID = fmt.Sprintf("tf-stats-%d", i)
		createSession(t, fts, req, true)
	}
	s := fts.GetStats()
	if t_, _ := s["total_sessions"].(int); t_ != 3 {
		t.Fatalf("total=%d want=3", t_)
	}
}

func TestCreateTransferSession_Concurrent(t *testing.T) {
	fts := newTestFTS()
	ch := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func(idx int) {
			req := validRequest()
			req.TransferID = fmt.Sprintf("tf-cc-%d", idx)
			_, err := fts.CreateTransferSession(req, false)
			ch <- err
		}(i)
	}
	for i := 0; i < 20; i++ {
		if err := <-ch; err != nil {
			t.Error(err)
		}
	}
}

func TestHandleClientDisconnect_UsesCorrectErrorType(t *testing.T) {
	if model.MessageTypeFileTransferError != "file:transfer:error" {
		t.Fatal("wrong error type")
	}
}

func TestUpdateSessionStatus_SkipCAS(t *testing.T) {
	fts := newTestFTS()
	req := validRequest()
	req.TransferID = "tf-skip-cas"
	createSession(t, fts, req, true)
	fts.UpdateSessionStatus("tf-skip-cas", "", "cancelled")
	fts.UpdateSessionStatus("tf-skip-cas", "", "error")
	s, _ := fts.GetSession("tf-skip-cas")
	if s.Status != "error" {
		t.Fatal("status mismatch")
	}
}
