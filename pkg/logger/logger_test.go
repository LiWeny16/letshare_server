package logger

import (
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestRetainErrorLogsKeepsOnlyRecentErrors(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	logs := []LogEntry{
		{Timestamp: now.Add(-time.Hour), Level: "warn", Message: "recent warn"},
		{Timestamp: now.Add(-2 * time.Hour), Level: "error", Message: "recent error"},
		{Timestamp: now.Add(-8 * 24 * time.Hour), Level: "error", Message: "old error"},
		{Timestamp: time.Time{}, Level: "error", Message: "missing timestamp"},
	}

	got := retainErrorLogs(logs, now, 0)
	if len(got) != 1 {
		t.Fatalf("retained %d logs, want 1: %#v", len(got), got)
	}
	if got[0].Message != "recent error" {
		t.Fatalf("retained message = %q, want recent error", got[0].Message)
	}
}

func TestRetainErrorLogsCapsNewestAfterRetention(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	logs := []LogEntry{
		{Timestamp: now.Add(-4 * time.Hour), Level: "error", Message: "oldest retained"},
		{Timestamp: now.Add(-3 * time.Hour), Level: "error", Message: "middle retained"},
		{Timestamp: now.Add(-2 * time.Hour), Level: "error", Message: "newest retained"},
	}

	got := retainErrorLogs(logs, now, 2)
	if len(got) != 2 {
		t.Fatalf("retained %d logs, want 2", len(got))
	}
	if got[0].Message != "middle retained" || got[1].Message != "newest retained" {
		t.Fatalf("retained newest logs in wrong order: %#v", got)
	}
}

func TestFileHookRecordsErrorLevelOnly(t *testing.T) {
	hook := &FileHook{}
	levels := hook.Levels()
	if len(levels) != 1 || levels[0] != logrus.ErrorLevel {
		t.Fatalf("Levels() = %#v, want only error", levels)
	}
}
