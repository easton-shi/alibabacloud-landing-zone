package workspacegc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunOnceRemovesExpiredTerminalDispatch(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	dir := filepath.Join(root, "dispatch-1")
	if err := WriteTerminal(dir, "SUCCESS", now.Add(-49*time.Hour)); err != nil {
		t.Fatal(err)
	}
	report, err := (Collector{
		Root: root, TaskTTL: 48 * time.Hour, ConversationTTL: 720 * time.Hour, Now: func() time.Time { return now },
	}).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) || len(report.Removed) != 1 || report.Removed[0] != "dispatch-1" {
		t.Fatalf("dir err=%v report=%+v", err, report)
	}
}

func TestRunOnceRetainsDispatchWithPendingResultOutbox(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	dir := filepath.Join(root, "dispatch-2")
	if err := WriteTerminal(dir, "SUCCESS", now.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	outbox := filepath.Join(root, ".runtime-outbox", "results", "dispatch-2.json")
	if err := os.MkdirAll(filepath.Dir(outbox), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outbox, []byte(`{"type":"TASK_RESULT","dispatchId":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Collector{
		Root: root, TaskTTL: 48 * time.Hour, ConversationTTL: 720 * time.Hour, Now: func() time.Time { return now },
	}).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dispatch with pending outbox was removed: %v", err)
	}
}

func TestRunOnceRetainsActiveDispatchAndConversation(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	dispatchDir := filepath.Join(root, "dispatch-3")
	conversationDir := filepath.Join(root, "conversations", "300")
	if err := WriteTerminal(dispatchDir, "RUNNING", now.Add(-96*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := TouchConversation(conversationDir, now.Add(-1000*time.Hour)); err != nil {
		t.Fatal(err)
	}
	report, err := (Collector{
		Root: root, TaskTTL: 48 * time.Hour, ConversationTTL: 720 * time.Hour,
		Now:                  func() time.Time { return now },
		IsDispatchActive:     func(id int64) bool { return id == 3 },
		IsConversationActive: func(id int64) bool { return id == 300 },
	}).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Removed) != 0 {
		t.Fatalf("removed active work: %+v", report)
	}
	for _, dir := range []string{dispatchDir, conversationDir} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("%s removed: %v", dir, err)
		}
	}
}

func TestRunOnceRemovesExpiredInactiveConversation(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	dir := filepath.Join(root, "conversations", "301")
	if err := TouchConversation(dir, now.Add(-721*time.Hour)); err != nil {
		t.Fatal(err)
	}
	report, err := (Collector{
		Root: root, TaskTTL: 48 * time.Hour, ConversationTTL: 720 * time.Hour, Now: func() time.Time { return now },
	}).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Removed) != 1 || report.Removed[0] != filepath.Join("conversations", "301") {
		t.Fatalf("report=%+v", report)
	}
}

func TestRunOncePressureModeRemovesCompletedBeforeTTL(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	completedDir := filepath.Join(root, "dispatch-4")
	failedDir := filepath.Join(root, "dispatch-5")
	if err := WriteTerminal(completedDir, "SUCCESS", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := WriteTerminal(failedDir, "FAILED", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(completedDir, "payload.bin"), make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := (Collector{
		Root: root, TaskTTL: 48 * time.Hour, ConversationTTL: 720 * time.Hour,
		Now: func() time.Time { return now }, HighWatermark: 85, LowWatermark: 75,
		MinFreeBytes: 5, TargetFreeBytes: 20,
		DiskUsage: func(string) (DiskUsage, error) { return DiskUsage{Total: 100, Free: 10}, nil },
	}).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Pressure || len(report.Removed) != 1 || report.Removed[0] != "dispatch-4" {
		t.Fatalf("report=%+v", report)
	}
	if _, err := os.Stat(failedDir); err != nil {
		t.Fatalf("recent failed dispatch was removed: %v", err)
	}
}
