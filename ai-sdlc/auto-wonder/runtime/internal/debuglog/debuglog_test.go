package debuglog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrintfRedactsRegisteredSecrets(t *testing.T) {
	dir := t.TempDir()
	recorder, err := New(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	recorder.AddSecret("awdispatch_super_secret")
	recorder.AddSecret("executor_token_value")
	recorder.Printf("Authorization: Bearer awdispatch_super_secret token=executor_token_value")
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "runtime-debug.log"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, "awdispatch_super_secret") || strings.Contains(text, "executor_token_value") {
		t.Fatalf("secret leaked: %q", text)
	}
	if !strings.Contains(text, "***") {
		t.Fatalf("redaction marker missing: %q", text)
	}
}

func TestAddSecretIgnoresShortValues(t *testing.T) {
	dir := t.TempDir()
	recorder, err := New(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	recorder.AddSecret("short")
	recorder.Printf("short value")
	_ = recorder.Close()
	raw, _ := os.ReadFile(filepath.Join(dir, "runtime-debug.log"))
	if !strings.Contains(string(raw), "short value") {
		t.Fatal("short values should not be redacted")
	}
}
