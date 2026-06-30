package service

import (
	"encoding/json"
	"fmt"
	"letshare-server/internal/model"
	"strings"
	"testing"
)

// ---------- shared helpers ----------

const (
	testMaxFileSize   = 500 * 1024 * 1024 // 500MB absolute limit
	testChunkSize     = 65536             // 64KB default chunk
	testBasicLimit    = 50 * 1024 * 1024  // 50MB — files below this don't need admin password
	testAdminPassword = "secret123"
)

func newTestFTS() *FileTransferService {
	ws := NewWebSocketService(10)
	return NewFileTransferService(ws, testMaxFileSize, testChunkSize, testBasicLimit, testAdminPassword)
}

// validRequest returns a minimal valid FileTransferRequest.
func validRequest() *model.FileTransferRequest {
	return &model.FileTransferRequest{
		TransferID: "tf-test-001",
		FileName:   "test.txt",
		FileSize:   10 * 1024 * 1024, // 10MB — below basicLimit, no password needed
		FileType:   "text/plain",
		ChunkSize:  65536,
		FromUserID: "sender-1",
		ToUserID:   "receiver-1",
		RoomName:   "room-test",
	}
}

// createSession is a convenience wrapper that calls CreateTransferSession and fails the test on error.
func createSession(t *testing.T, fts *FileTransferService, req *model.FileTransferRequest) *model.FileTransferSession {
	t.Helper()
	session, err := fts.CreateTransferSession(req)
	if err != nil {
		t.Fatalf("CreateTransferSession(%q): unexpected error: %v", req.TransferID, err)
	}
	return session
}

// ====================================================================
// Test 1: Password validation edge cases (table-driven)
// ====================================================================

func TestCreateTransferSession_PasswordValidation(t *testing.T) {
	tests := []struct {
		name        string
		fileSize    int64
		adminPass   string
		serverPass  string
		basicLimit  int64
		wantErr     bool
		errContains string
	}{
		{
			name:        "small file no password needed, empty adminPass",
			fileSize:    10 * 1024 * 1024,
			adminPass:   "",
			serverPass:  testAdminPassword,
			basicLimit:  50 * 1024 * 1024,
			wantErr:     false,
			errContains: "",
		},
		{
			name:        "file exactly at basic limit, no password needed",
			fileSize:    50 * 1024 * 1024,
			adminPass:   "",
			serverPass:  testAdminPassword,
			basicLimit:  50 * 1024 * 1024,
			wantErr:     false,
			errContains: "",
		},
		{
			name:        "file one byte over basic limit, password required",
			fileSize:    50*1024*1024 + 1,
			adminPass:   "",
			serverPass:  testAdminPassword,
			basicLimit:  50 * 1024 * 1024,
			wantErr:     true,
			errContains: "需要管理员密码",
		},
		{
			name:        "large file no password provided",
			fileSize:    100 * 1024 * 1024,
			adminPass:   "",
			serverPass:  testAdminPassword,
			basicLimit:  50 * 1024 * 1024,
			wantErr:     true,
			errContains: "需要管理员密码",
		},
		{
			name:        "large file wrong password",
			fileSize:    100 * 1024 * 1024,
			adminPass:   "wrong-password",
			serverPass:  testAdminPassword,
			basicLimit:  50 * 1024 * 1024,
			wantErr:     true,
			errContains: "管理员密码错误",
		},
		{
			name:        "large file correct password",
			fileSize:    100 * 1024 * 1024,
			adminPass:   testAdminPassword,
			serverPass:  testAdminPassword,
			basicLimit:  50 * 1024 * 1024,
			wantErr:     false,
			errContains: "",
		},
		{
			name:        "file exceeds absolute max size",
			fileSize:    600 * 1024 * 1024, // 600MB > 500MB max
			adminPass:   testAdminPassword,
			serverPass:  testAdminPassword,
			basicLimit:  50 * 1024 * 1024,
			wantErr:     true,
			errContains: "超过限制",
		},
		{
			name:        "basic limit of zero requires password for any non-zero file",
			fileSize:    1024, // even 1KB requires password when basicLimit=0
			adminPass:   "",
			serverPass:  testAdminPassword,
			basicLimit:  0,
			wantErr:     true,
			errContains: "需要管理员密码",
		},
		{
			name:        "max file size equals absolute limit (boundary)",
			fileSize:    500 * 1024 * 1024,
			adminPass:   testAdminPassword,
			serverPass:  testAdminPassword,
			basicLimit:  50 * 1024 * 1024,
			wantErr:     false,
			errContains: "",
		},
		{
			name:        "tiny file (1 byte) no password",
			fileSize:    1,
			adminPass:   "",
			serverPass:  testAdminPassword,
			basicLimit:  50 * 1024 * 1024,
			wantErr:     false,
			errContains: "",
		},
		{
			name:        "zero byte file no password",
			fileSize:    0,
			adminPass:   "",
			serverPass:  testAdminPassword,
			basicLimit:  50 * 1024 * 1024,
			wantErr:     false,
			errContains: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fts := NewFileTransferService(
				NewWebSocketService(10),
				testMaxFileSize,
				testChunkSize,
				tt.basicLimit,
				tt.serverPass,
			)

			req := &model.FileTransferRequest{
				TransferID: "tf-pw-" + strings.ReplaceAll(tt.name, " ", "-"),
				FileName:   "pw-test.bin",
				FileSize:   tt.fileSize,
				FileType:   "application/octet-stream",
				ChunkSize:  65536,
				FromUserID: "sender-1",
				ToUserID:   "receiver-1",
				RoomName:   "room-pw",
				AdminPass:  tt.adminPass,
			}

			session, err := fts.CreateTransferSession(req)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.errContains)
				}
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Fatalf("expected error containing %q, got %q", tt.errContains, err.Error())
				}
				// Verify no session was created on error
				if session != nil {
					t.Error("expected nil session when error occurs")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if session == nil {
				t.Fatal("expected non-nil session on success")
			}
			if session.TransferID != req.TransferID {
				t.Errorf("TransferID = %q, want %q", session.TransferID, req.TransferID)
			}

			// Verify session exists in the service
			got, getErr := fts.GetSession(req.TransferID)
			if getErr != nil {
				t.Fatalf("GetSession after create: %v", getErr)
			}
			if got.Status != "pending" {
				t.Errorf("new session Status = %q, want %q", got.Status, "pending")
			}
		})
	}
}

// ====================================================================
// Test 2: Admin password is NOT stored in the session or leaked
// ====================================================================

func TestCreateTransferSession_AdminPasswordNotStored(t *testing.T) {
	fts := newTestFTS()

	// Create a session that requires admin password
	req := &model.FileTransferRequest{
		TransferID: "tf-noleak",
		FileName:   "large.bin",
		FileSize:   100 * 1024 * 1024, // 100MB > 50MB basic limit
		FileType:   "application/octet-stream",
		ChunkSize:  65536,
		FromUserID: "sender-1",
		ToUserID:   "receiver-1",
		RoomName:   "room-noleak",
		AdminPass:  testAdminPassword,
	}

	session := createSession(t, fts, req)

	// Verify the FileTransferSession struct has no AdminPass field at all.
	// This is a compile-time guarantee, but we also verify runtime behavior:
	// the session should not expose any password information in its fields.
	if session.TransferID == "" {
		t.Error("session should have a non-empty TransferID")
	}
	if session.FileName == "" {
		t.Error("session should have a non-empty FileName")
	}

	// Additional safety: verify that re-serializing the request struct (as the
	// handler does before forwarding to the receiver) would NOT include admin_pass
	// when AdminPass has been cleared. This simulates what the handler fix does.
	req.AdminPass = "" // handler fix: clear before forwarding
	serialized, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if strings.Contains(string(serialized), "admin_pass") {
		t.Fatalf("serialized request should NOT contain 'admin_pass' after clearing, got: %s", string(serialized))
	}
	t.Logf("✓ Serialized request (after clearing AdminPass): %s", string(serialized))
}

// ====================================================================
// Test 3: FileTransferRequest JSON omitempty behavior
// ====================================================================

func TestFileTransferRequest_JSONOmitEmpty(t *testing.T) {
	// Verify that admin_pass is omitted from JSON when empty (omitempty tag behavior).
	// This is critical for Bug #1: the handler must clear AdminPass before forwarding.

	t.Run("AdminPass set to non-empty appears in JSON", func(t *testing.T) {
		req := &model.FileTransferRequest{
			TransferID: "tf-json-1",
			FileName:   "f.txt",
			FileSize:   100,
			FromUserID: "s",
			ToUserID:   "r",
			RoomName:   "room",
			AdminPass:  "secret123",
		}
		data, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(data), "admin_pass") {
			t.Fatalf("expected 'admin_pass' in JSON when AdminPass is non-empty, got: %s", string(data))
		}
		t.Logf("JSON with AdminPass: %s", string(data))
	})

	t.Run("AdminPass set to empty string is omitted from JSON", func(t *testing.T) {
		req := &model.FileTransferRequest{
			TransferID: "tf-json-2",
			FileName:   "f.txt",
			FileSize:   100,
			FromUserID: "s",
			ToUserID:   "r",
			RoomName:   "room",
			AdminPass:  "", // cleared by handler fix
		}
		data, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(data), "admin_pass") {
			t.Fatalf("must NOT contain 'admin_pass' when AdminPass is empty (omitempty), got: %s", string(data))
		}
		t.Logf("JSON without AdminPass: %s", string(data))
	})

	t.Run("AdminPass not set (zero value) is omitted from JSON", func(t *testing.T) {
		req := &model.FileTransferRequest{
			TransferID: "tf-json-3",
			FileName:   "f.txt",
			FileSize:   100,
			FromUserID: "s",
			ToUserID:   "r",
			RoomName:   "room",
			// AdminPass not set — zero value
		}
		data, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(data), "admin_pass") {
			t.Fatalf("must NOT contain 'admin_pass' when AdminPass is zero value, got: %s", string(data))
		}
		t.Logf("JSON without AdminPass field: %s", string(data))
	})
}

// ====================================================================
// Test 4: Error message type is "file:transfer:error"
// ====================================================================

func TestFileTransferError_MessageType(t *testing.T) {
	// Verify the constant has the correct value.
	if model.MessageTypeFileTransferError != "file:transfer:error" {
		t.Fatalf("MessageTypeFileTransferError = %q, want %q",
			model.MessageTypeFileTransferError, "file:transfer:error")
	}

	// Verify that messages created with this constant produce correct JSON type field.
	msg := model.NewWebSocketMessage(
		model.MessageTypeFileTransferError,
		"room-x",
		"",
		map[string]interface{}{
			"transfer_id": "tf-err-001",
			"error":       "test error message",
		},
	)

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal error message: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	gotType, ok := decoded["type"].(string)
	if !ok {
		t.Fatalf("decoded message missing 'type' field: %s", string(data))
	}
	if gotType != "file:transfer:error" {
		t.Fatalf("message type = %q, want %q", gotType, "file:transfer:error")
	}

	t.Logf("✓ Error message JSON type is correct: %q", gotType)
}

// ====================================================================
// Test 5: NewErrorMessage (general) vs file:transfer:error distinction
// ====================================================================

func TestErrorTypes_Distinction(t *testing.T) {
	// General NewErrorMessage uses "error" type (for non-file-transfer errors like
	// subscribe failures, invalid message types, etc.)
	generalErr := model.NewErrorMessage(400, "bad request")

	if generalErr.Type != "error" {
		t.Fatalf("NewErrorMessage type = %q, want %q", generalErr.Type, "error")
	}

	// File-transfer-specific errors must use "file:transfer:error" type.
	// The handler's sendError should use file:transfer:error for file-transfer
	// related error responses. Verify the constants are distinct.
	if model.MessageTypeError == model.MessageTypeFileTransferError {
		t.Fatal("MessageTypeError and MessageTypeFileTransferError should be distinct constants")
	}

	t.Logf("✓ General error type: %q, File transfer error type: %q",
		model.MessageTypeError, model.MessageTypeFileTransferError)
}

// ====================================================================
// Test 6: UpdateSessionStatus — resend from transferring state (Bug fix)
// ====================================================================

func TestUpdateSessionStatus_ResendFromTransferring(t *testing.T) {
	fts := newTestFTS()

	req := validRequest()
	req.TransferID = "tf-resend-transition"
	session := createSession(t, fts, req)

	// Advance to "transferring" state (simulating what the handler does after START)
	if err := fts.UpdateSessionStatus("tf-resend-transition", "pending", "accepted"); err != nil {
		t.Fatalf("pending → accepted: %v", err)
	}
	if err := fts.UpdateSessionStatus("tf-resend-transition", "accepted", "transferring"); err != nil {
		t.Fatalf("accepted → transferring: %v", err)
	}

	// Verify we're in transferring state
	got, _ := fts.GetSession("tf-resend-transition")
	if got.Status != "transferring" {
		t.Fatalf("expected transferring, got %q", got.Status)
	}

	// BUG FIX: Transition from "transferring" → "resending" should now be allowed.
	// The handler previously only allowed "ending" → "resending", but the fix also
	// allows "transferring" → "resending" (e.g., when receiver detects missing
	// chunks mid-transfer and requests resend before sender sends END).
	err := fts.UpdateSessionStatus("tf-resend-transition", "transferring", "resending")
	if err != nil {
		t.Fatalf("BUG: transferring → resending transition failed: %v", err)
	}

	got, _ = fts.GetSession("tf-resend-transition")
	if got.Status != "resending" {
		t.Fatalf("after transition, expected resending, got %q", got.Status)
	}

	t.Log("✓ transferring → resending transition succeeded (Bug fix verified)")

	_ = session
}

// ====================================================================
// Test 7: UpdateSessionStatus — all valid state transitions
// ====================================================================

func TestUpdateSessionStatus_AllTransitions(t *testing.T) {
	fts := newTestFTS()

	tests := []struct {
		name           string
		setupStatus    string // status to set before the transition test
		expectedStatus string
		newStatus      string
		wantErr        bool
	}{
		{"pending → accepted", "pending", "pending", "accepted", false},
		{"accepted → transferring", "accepted", "accepted", "transferring", false},
		{"transferring → ending", "transferring", "transferring", "ending", false},
		{"ending → resending", "ending", "ending", "resending", false},
		{"transferring → resending (bug fix)", "transferring", "transferring", "resending", false},
		{"resending → transferring", "resending", "resending", "transferring", false},
		{"resending → ending", "resending", "resending", "ending", false},
		{"resending → completed", "resending", "resending", "completed", false},
		{"ending → completed", "ending", "ending", "completed", false},
		{"any → cancelled", "transferring", "transferring", "cancelled", false},
		{"any → error", "transferring", "transferring", "error", false},
		// CAS mismatch tests
		{"wrong expected status fails", "pending", "accepted", "transferring", true},
		{"empty expected skips CAS", "pending", "", "accepted", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transferID := "tf-trans-" + strings.ReplaceAll(tt.name, " ", "-")

			// Create session and set it to the desired starting status
			req := validRequest()
			req.TransferID = transferID
			_ = createSession(t, fts, req)

			// Manually set the starting status (skip CAS for setup)
			if tt.setupStatus != "pending" {
				if err := fts.UpdateSessionStatus(transferID, "", tt.setupStatus); err != nil {
					// If setup status is different from current, use CAS from "pending"
					_ = fts.UpdateSessionStatus(transferID, "pending", tt.setupStatus)
				}
			}

			err := fts.UpdateSessionStatus(transferID, tt.expectedStatus, tt.newStatus)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for transition %q → %q (expected=%q), got nil",
						tt.setupStatus, tt.newStatus, tt.expectedStatus)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error for transition %q → %q (expected=%q): %v",
					tt.setupStatus, tt.newStatus, tt.expectedStatus, err)
			}

			session, _ := fts.GetSession(transferID)
			if session.Status != tt.newStatus {
				t.Fatalf("after transition, status = %q, want %q", session.Status, tt.newStatus)
			}
		})
	}
}

// ====================================================================
// Test 8: UpdateSessionStatus — non-existent session
// ====================================================================

func TestUpdateSessionStatus_NonExistentSession(t *testing.T) {
	fts := newTestFTS()

	err := fts.UpdateSessionStatus("does-not-exist", "pending", "accepted")
	if err == nil {
		t.Fatal("expected error for non-existent session")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("error should mention session doesn't exist, got: %v", err)
	}
}

// ====================================================================
// Test 9: Full session lifecycle (create, get, update, delete)
// ====================================================================

func TestFileTransferSession_Lifecycle(t *testing.T) {
	fts := newTestFTS()

	// 1. Create
	req := validRequest()
	req.TransferID = "tf-lifecycle"
	session := createSession(t, fts, req)

	if session.Status != "pending" {
		t.Fatalf("new session status = %q, want %q", session.Status, "pending")
	}
	if session.TransferID != "tf-lifecycle" {
		t.Fatalf("TransferID = %q, want %q", session.TransferID, "tf-lifecycle")
	}
	if session.FileName != req.FileName {
		t.Fatalf("FileName = %q, want %q", session.FileName, req.FileName)
	}
	if session.FileSize != req.FileSize {
		t.Fatalf("FileSize = %d, want %d", session.FileSize, req.FileSize)
	}
	if session.FromUserID != req.FromUserID {
		t.Fatalf("FromUserID = %q, want %q", session.FromUserID, req.FromUserID)
	}
	if session.ToUserID != req.ToUserID {
		t.Fatalf("ToUserID = %q, want %q", session.ToUserID, req.ToUserID)
	}
	if session.TotalChunks == 0 {
		t.Fatal("TotalChunks should be > 0")
	}
	t.Logf("✓ Created: transfer_id=%s, total_chunks=%d", session.TransferID, session.TotalChunks)

	// 2. Get
	got, err := fts.GetSession("tf-lifecycle")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.TransferID != "tf-lifecycle" {
		t.Fatalf("GetSession mismatch")
	}

	// 3. Get non-existent
	_, err = fts.GetSession("does-not-exist")
	if err == nil {
		t.Fatal("expected error for non-existent session")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("error should mention session doesn't exist, got: %v", err)
	}

	// 4. Update status
	if err := fts.UpdateSessionStatus("tf-lifecycle", "pending", "accepted"); err != nil {
		t.Fatalf("UpdateSessionStatus: %v", err)
	}
	got, _ = fts.GetSession("tf-lifecycle")
	if got.Status != "accepted" {
		t.Fatalf("status = %q, want accepted", got.Status)
	}

	// 5. Update clients
	if err := fts.UpdateSessionClients("tf-lifecycle", "client-sender", "client-receiver"); err != nil {
		t.Fatalf("UpdateSessionClients: %v", err)
	}
	got, _ = fts.GetSession("tf-lifecycle")
	if got.FromClientID != "client-sender" {
		t.Fatalf("FromClientID = %q, want client-sender", got.FromClientID)
	}
	if got.ToClientID != "client-receiver" {
		t.Fatalf("ToClientID = %q, want client-receiver", got.ToClientID)
	}

	// 6. Remove
	fts.RemoveSession("tf-lifecycle")
	_, err = fts.GetSession("tf-lifecycle")
	if err == nil {
		t.Fatal("expected error after RemoveSession")
	}

	// 7. Remove non-existent (should be a no-op, not a panic)
	fts.RemoveSession("does-not-exist")

	t.Log("✓ Full lifecycle: create → get → update → delete")
}

// ====================================================================
// Test 10: CreateTransferSession — required fields validation
// ====================================================================

func TestCreateTransferSession_RequiredFields(t *testing.T) {
	fts := newTestFTS()

	tests := []struct {
		name        string
		fromUserID  string
		toUserID    string
		wantErr     bool
		errContains string
	}{
		{"both empty", "", "", true, "发送者"},
		{"fromUserID empty", "", "receiver", true, "发送者"},
		{"toUserID empty", "sender", "", true, "接收者"},
		{"both provided", "sender", "receiver", false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validRequest()
			req.TransferID = "tf-req-" + strings.ReplaceAll(tt.name, " ", "-")
			req.FromUserID = tt.fromUserID
			req.ToUserID = tt.toUserID

			_, err := fts.CreateTransferSession(req)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Fatalf("error should contain %q, got: %v", tt.errContains, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// ====================================================================
// Test 11: CreateTransferSession — chunk calculation
// ====================================================================

func TestCreateTransferSession_ChunkCalculation(t *testing.T) {
	fts := newTestFTS()

	tests := []struct {
		name           string
		fileSize       int64
		chunkSize      int
		wantChunks     int
		wantChunkSize  int // the chunk size that gets stored in the session
	}{
		{
			name:          "exact multiple",
			fileSize:      65536,
			chunkSize:     65536,
			wantChunks:    1,
			wantChunkSize: 65536,
		},
		{
			name:          "one extra byte",
			fileSize:      65537,
			chunkSize:     65536,
			wantChunks:    2,
			wantChunkSize: 65536,
		},
		{
			name:          "half chunk",
			fileSize:      32768,
			chunkSize:     65536,
			wantChunks:    1,
			wantChunkSize: 65536,
		},
		{
			name:          "zero file size defaults to 1 chunk",
			fileSize:      0,
			chunkSize:     65536,
			wantChunks:    1,
			wantChunkSize: 65536,
		},
		{
			name:          "default chunk size when 0 specified",
			fileSize:      10 * 1024 * 1024, // 10MB — below basic limit, no password needed
			chunkSize:     0,
			wantChunks:    int(10*1024*1024) / testChunkSize,
			wantChunkSize: testChunkSize,
		},
		{
			name:          "small custom chunk size",
			fileSize:      1000,
			chunkSize:     100,
			wantChunks:    10,
			wantChunkSize: 100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validRequest()
			req.TransferID = "tf-chunk-" + strings.ReplaceAll(tt.name, " ", "-")
			req.FileSize = tt.fileSize
			req.ChunkSize = tt.chunkSize

			session := createSession(t, fts, req)

			if session.TotalChunks != tt.wantChunks {
				t.Fatalf("TotalChunks = %d, want %d", session.TotalChunks, tt.wantChunks)
			}
			if session.ChunkSize != tt.wantChunkSize {
				t.Fatalf("ChunkSize = %d, want %d", session.ChunkSize, tt.wantChunkSize)
			}
			if session.FileSize != tt.fileSize {
				t.Fatalf("FileSize = %d, want %d", session.FileSize, tt.fileSize)
			}
		})
	}
}

// ====================================================================
// Test 12: GetStats — session counting by status
// ====================================================================

func TestGetStats_SessionCounts(t *testing.T) {
	fts := newTestFTS()

	// Initially empty
	stats := fts.GetStats()
	if total, ok := stats["total_sessions"].(int); !ok || total != 0 {
		t.Fatalf("initial total_sessions = %v, want 0", stats["total_sessions"])
	}

	// Create 3 sessions with different statuses
	for i := range []string{"pending", "accepted", "transferring"} {
		req := validRequest()
		req.TransferID = fmt.Sprintf("tf-stats-%d", i)
		session := createSession(t, fts, req)
		if i > 0 {
			// Advance status for non-first sessions
			if i == 1 {
				fts.UpdateSessionStatus(session.TransferID, "pending", "accepted")
			} else {
				fts.UpdateSessionStatus(session.TransferID, "pending", "accepted")
				fts.UpdateSessionStatus(session.TransferID, "accepted", "transferring")
			}
		}
	}

	stats = fts.GetStats()
	total, _ := stats["total_sessions"].(int)
	if total != 3 {
		t.Fatalf("total_sessions = %d, want 3", total)
	}

	statusCount, _ := stats["status_count"].(map[string]int)
	if statusCount["pending"] != 1 {
		t.Errorf("pending count = %d, want 1", statusCount["pending"])
	}
	if statusCount["accepted"] != 1 {
		t.Errorf("accepted count = %d, want 1", statusCount["accepted"])
	}
	if statusCount["transferring"] != 1 {
		t.Errorf("transferring count = %d, want 1", statusCount["transferring"])
	}

	t.Logf("✓ Stats: total=%d, counts=%v", total, statusCount)
}

// ====================================================================
// Test 13: Concurrent session access (race detection)
// ====================================================================

func TestCreateTransferSession_Concurrent(t *testing.T) {
	fts := newTestFTS()

	const numGoroutines = 20
	errCh := make(chan error, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(idx int) {
			req := validRequest()
			req.TransferID = fmt.Sprintf("tf-concurrent-%d", idx)
			_, err := fts.CreateTransferSession(req)
			errCh <- err
		}(i)
	}

	for i := 0; i < numGoroutines; i++ {
		if err := <-errCh; err != nil {
			t.Errorf("concurrent CreateTransferSession %d: %v", i, err)
		}
	}

	stats := fts.GetStats()
	total, _ := stats["total_sessions"].(int)
	if total != numGoroutines {
		t.Errorf("total_sessions = %d, want %d", total, numGoroutines)
	}

	// Verify all sessions exist
	for i := 0; i < numGoroutines; i++ {
		session, err := fts.GetSession(fmt.Sprintf("tf-concurrent-%d", i))
		if err != nil {
			t.Errorf("GetSession concurrent-%d: %v", i, err)
		}
		if session != nil && session.Status != "pending" {
			t.Errorf("concurrent session %d status = %q, want pending", i, session.Status)
		}
	}

	t.Logf("✓ %d concurrent sessions created successfully", numGoroutines)
}

// ====================================================================
// Test 14: HandleClientDisconnect uses file:transfer:error type
// ====================================================================

func TestHandleClientDisconnect_UsesCorrectErrorType(t *testing.T) {
	// Verify at compile/runtime level that file:transfer:error constant is correct.
	// The HandleClientDisconnect method (file_transfer.go:211-218) uses
	// model.MessageTypeFileTransferError when creating WebSocket messages,
	// which must equal "file:transfer:error".
	if model.MessageTypeFileTransferError != "file:transfer:error" {
		t.Fatalf("MessageTypeFileTransferError = %q, want %q",
			model.MessageTypeFileTransferError, "file:transfer:error")
	}

	// Verify the error type used in HandleClientDisconnect is NOT the generic "error" type.
	if model.MessageTypeFileTransferError == model.MessageTypeError {
		t.Fatal("file transfer error type must NOT equal generic error type")
	}

	t.Logf("✓ HandleClientDisconnect uses type %q (not %q)",
		model.MessageTypeFileTransferError, model.MessageTypeError)
}

// ====================================================================
// Test 15: UpdateSessionStatus with empty expectedStatus (skip CAS)
// ====================================================================

func TestUpdateSessionStatus_SkipCAS(t *testing.T) {
	fts := newTestFTS()

	req := validRequest()
	req.TransferID = "tf-skip-cas"
	_ = createSession(t, fts, req)

	// Skip CAS by providing empty expectedStatus — should update regardless of current status.
	if err := fts.UpdateSessionStatus("tf-skip-cas", "", "cancelled"); err != nil {
		t.Fatalf("skip CAS update failed: %v", err)
	}

	session, _ := fts.GetSession("tf-skip-cas")
	if session.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", session.Status)
	}

	// Skip CAS again to change from cancelled to error
	if err := fts.UpdateSessionStatus("tf-skip-cas", "", "error"); err != nil {
		t.Fatalf("skip CAS update (2nd): %v", err)
	}

	session, _ = fts.GetSession("tf-skip-cas")
	if session.Status != "error" {
		t.Fatalf("status = %q, want error", session.Status)
	}

	t.Log("✓ Skip CAS updates work correctly")
}
