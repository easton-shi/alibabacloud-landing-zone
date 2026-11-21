package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/update"
)

type upgradeRecorder struct {
	mu      sync.Mutex
	results []UpgradeResult
}

func (r *upgradeRecorder) send(value any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = append(r.results, value.(UpgradeResult))
	return nil
}

func (r *upgradeRecorder) phases() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.results))
	for _, result := range r.results {
		out = append(out, result.Phase)
	}
	return out
}

func (r *upgradeRecorder) waitFor(t *testing.T, phase string) UpgradeResult {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, result := range r.results {
			if result.Phase == phase {
				r.mu.Unlock()
				return result
			}
		}
		r.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("phase %q not observed; got %v", phase, r.phases())
	return UpgradeResult{}
}

func releaseServer(t *testing.T, version string, artifact []byte) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(artifact)
	manifest := fmt.Sprintf(`{"version":%q,"artifacts":{"%s":{"url":"artifact","sha256":"%s","size":%d}}}`,
		version, update.PlatformKey(), hex.EncodeToString(sum[:]), len(artifact))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + version + "/manifest.json":
			_, _ = w.Write([]byte(manifest))
		case "/" + version + "/artifact":
			_, _ = w.Write(artifact)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestUpgradeDownloadsReplacesAndExecs(t *testing.T) {
	artifact := []byte("new-binary")
	server := releaseServer(t, "0.2.0", artifact)
	dir := t.TempDir()
	executable := filepath.Join(dir, "autowonder-runtime")
	if err := os.WriteFile(executable, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	updater := update.New(update.Config{BaseURL: server.URL, Insecure: true}, "0.1.0", executable,
		filepath.Join(dir, "state", "receipt.json"))
	recorder := &upgradeRecorder{}
	executed := make(chan execCall, 1)
	handler := &Handler{
		ExecutorID: 5, Version: "0.1.0", Args: []string{"connect", "--token", "x"},
		Executable: executable, Inventory: &fakeInventory{}, Send: recorder.send,
		Updater: updater, DrainTimeout: time.Second,
		Exec: func(executable string, argv, env []string) error {
			executed <- execCall{executable: executable, argv: argv, env: env}
			return nil
		},
	}
	raw, err := json.Marshal(UpgradeFrame{
		Type: "EXECUTOR_UPGRADE", ExecutorID: 5, RequestID: "upgrade-1", TargetVersion: "0.2.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !handler.Handle(context.Background(), raw) {
		t.Fatal("upgrade frame was not handled")
	}
	select {
	case call := <-executed:
		if call.executable != executable {
			t.Fatalf("executable = %q", call.executable)
		}
		if strings.Join(call.argv, " ") != "connect --token x" {
			t.Fatalf("argv = %v", call.argv)
		}
		if !hasEnv(call.env, "AUTOWONDER_UPGRADE_TARGET_VERSION=0.2.0") {
			t.Fatalf("env = %v", call.env)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upgrade did not exec")
	}
	installed, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if string(installed) != string(artifact) {
		t.Fatalf("installed = %q", installed)
	}
	receipt, err := updater.ReadReceipt()
	if err != nil || receipt == nil || receipt.TargetVersion != "0.2.0" {
		t.Fatalf("receipt = %#v err=%v", receipt, err)
	}
	for _, phase := range []string{"accepted", "draining", "downloading", "applying"} {
		recorder.waitFor(t, phase)
	}
	for _, result := range recorder.results {
		if result.Phase != "success" && result.Phase != "failed" && result.ReceiptID != "" {
			t.Fatalf("intermediate phase carried receipt: %#v", result)
		}
	}
}

func TestUpgradeWithoutSourceReportsRetryableFailure(t *testing.T) {
	recorder := &upgradeRecorder{}
	handler := &Handler{ExecutorID: 5, Version: "0.1.0", Send: recorder.send, Inventory: &fakeInventory{}}
	raw, _ := json.Marshal(UpgradeFrame{
		Type: "EXECUTOR_UPGRADE", ExecutorID: 5, RequestID: "upgrade-2", TargetVersion: "0.2.0",
	})
	handler.Handle(context.Background(), raw)
	result := recorder.waitFor(t, "failed")
	if result.ReceiptID != "" || result.Terminal {
		t.Fatalf("transient failure must stay retryable: %#v", result)
	}
}

func TestPublishUpgradeReceiptAndAcknowledge(t *testing.T) {
	dir := t.TempDir()
	updater := update.New(update.Config{BaseURL: "https://example.com"}, "0.2.0",
		filepath.Join(dir, "runtime"), filepath.Join(dir, "receipt.json"))
	if err := updater.WriteReceipt(update.Receipt{
		RequestID: "upgrade-3", ReceiptID: "receipt-3", TargetVersion: "0.2.0", Phase: "restarting",
	}); err != nil {
		t.Fatal(err)
	}
	recorder := &upgradeRecorder{}
	handler := &Handler{ExecutorID: 5, Version: "0.2.0", Updater: updater}
	handler.PublishUpgradeReceipt(recorder.send)
	result := recorder.waitFor(t, "success")
	if result.ReceiptID != "receipt-3" || result.CurrentVersion != "0.2.0" || !result.Terminal {
		t.Fatalf("receipt result = %#v", result)
	}
	ack, _ := json.Marshal(map[string]any{
		"type": "EXECUTOR_UPGRADE_RESULT_ACK", "requestId": "upgrade-3", "receiptId": "receipt-3",
	})
	handler.acknowledgeUpgrade(ack)
	if receipt, err := updater.ReadReceipt(); err != nil || receipt != nil {
		t.Fatalf("receipt not cleared: %#v err=%v", receipt, err)
	}
}

func hasEnv(env []string, expected string) bool {
	for _, entry := range env {
		if entry == expected {
			return true
		}
	}
	return false
}
