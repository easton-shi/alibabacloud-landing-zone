package control

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

type execCall struct {
	executable string
	argv       []string
	env        []string
}

type fakeInventory struct {
	mu        sync.Mutex
	busy      bool
	draining  bool
	canceled  bool
	idleAfter time.Duration
	started   time.Time
}

func (f *fakeInventory) Busy() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.idleAfter > 0 && time.Since(f.started) >= f.idleAfter {
		f.busy = false
	}
	return f.busy
}
func (f *fakeInventory) SetDraining(value bool) { f.mu.Lock(); f.draining = value; f.mu.Unlock() }
func (f *fakeInventory) CancelAll()             { f.mu.Lock(); f.canceled = true; f.busy = false; f.mu.Unlock() }
func (f *fakeInventory) WaitIdle(ctx context.Context) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !f.Busy() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

type recorder struct {
	mu      sync.Mutex
	results []RestartResult
}

func (r *recorder) send(value any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = append(r.results, value.(RestartResult))
	return nil
}
func (r *recorder) statuses() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]string, 0, len(r.results))
	for _, item := range r.results {
		result = append(result, item.Status)
	}
	return result
}

func frame(t *testing.T, value map[string]any) []byte {
	t.Helper()
	value["type"] = "EXECUTOR_RESTART"
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestIdleRestartExecsSameArgs(t *testing.T) {
	calls := make(chan execCall, 1)
	events := &recorder{}
	inventory := &fakeInventory{}
	handler := &Handler{ExecutorID: 5, Version: "1.2.3",
		Args:      []string{"connect", "--ws-url", "wss://x", "--token", "t", "--executor-id", "5", "--provider", "codex"},
		Inventory: inventory, Send: events.send,
		Exec: func(executable string, argv, env []string) error {
			calls <- execCall{executable: executable, argv: argv, env: env}
			return nil
		}}
	if !handler.Handle(context.Background(), frame(t, map[string]any{"executorId": 5, "requestId": "r1"})) {
		t.Fatal("frame was not handled")
	}
	call := waitCall(t, calls)
	if call.executable == "" || len(call.argv) != 9 || call.argv[0] != "connect" {
		t.Fatalf("unexpected exec: executable=%q argv=%v", call.executable, call.argv)
	}
	if !contains(call.env, "AUTOWONDER_RESTART_REQUEST_ID=r1") {
		t.Fatal("restart request id was not exported")
	}
	if statuses := events.statuses(); len(statuses) != 1 || statuses[0] != statusRestarting {
		t.Fatalf("unexpected statuses: %v", statuses)
	}
}

func TestBusyRestartWithoutModeFails(t *testing.T) {
	calls := make(chan execCall, 1)
	events := &recorder{}
	handler := &Handler{ExecutorID: 5, Inventory: &fakeInventory{busy: true}, Send: events.send,
		Exec: func(string, []string, []string) error { calls <- execCall{}; return nil }}
	handler.Handle(context.Background(), frame(t, map[string]any{"executorId": 5, "requestId": "r2"}))
	waitFor(t, func() bool { return len(events.statuses()) > 0 })
	if statuses := events.statuses(); statuses[0] != statusFailed {
		t.Fatalf("unexpected statuses: %v", statuses)
	}
	select {
	case <-calls:
		t.Fatal("busy executor restarted without a mode")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestWhenIdleWaitsThenRestarts(t *testing.T) {
	calls := make(chan execCall, 1)
	events := &recorder{}
	inventory := &fakeInventory{busy: true, idleAfter: 40 * time.Millisecond, started: time.Now()}
	handler := &Handler{ExecutorID: 5, Inventory: inventory, Send: events.send, IdlePoll: 10 * time.Millisecond,
		Exec: func(string, []string, []string) error { calls <- execCall{}; return nil }}
	handler.Handle(context.Background(), frame(t, map[string]any{"executorId": 5, "requestId": "r3",
		"restartMode": restartModeIdle}))
	waitCall(t, calls)
	statuses := events.statuses()
	if len(statuses) < 2 || statuses[0] != statusWaitingIdle || statuses[len(statuses)-1] != statusRestarting {
		t.Fatalf("unexpected statuses: %v", statuses)
	}
	if !inventory.draining {
		t.Fatal("draining was not enabled while waiting")
	}
}

func TestForceRestartCancelsRunningWork(t *testing.T) {
	calls := make(chan execCall, 1)
	events := &recorder{}
	inventory := &fakeInventory{busy: true}
	handler := &Handler{ExecutorID: 5, Inventory: inventory, Send: events.send, ForceGrace: 20 * time.Millisecond,
		Exec: func(string, []string, []string) error { calls <- execCall{}; return nil }}
	handler.Handle(context.Background(), frame(t, map[string]any{"executorId": 5, "requestId": "r4",
		"restartMode": restartModeForce}))
	waitCall(t, calls)
	if !inventory.canceled {
		t.Fatal("force restart did not cancel running work")
	}
}

func TestLaunchConfigRewritesSupportedFlags(t *testing.T) {
	calls := make(chan execCall, 1)
	handler := &Handler{ExecutorID: 5, Version: "1.2.3",
		Args: []string{"connect", "--ws-url", "wss://x", "--token", "t", "--provider", "codex",
			"--model", "old", "--debug", "--workspace-root", "/old"},
		Inventory: &fakeInventory{}, Send: (&recorder{}).send,
		Exec: func(_ string, argv, _ []string) error { calls <- execCall{argv: argv}; return nil }}
	handler.Handle(context.Background(), frame(t, map[string]any{"executorId": 5, "requestId": "r5",
		"launch": map[string]any{"runtimeVersion": "1.2.3", "workspaceRoot": "/new", "daemonName": "worker",
			"debug": false, "memoryMode": "none", "maxConcurrentDispatches": 3, "model": "new"}}))
	call := waitCall(t, calls)
	joined := strings.Join(call.argv, " ")
	for _, expected := range []string{"--workspace-root /new", "--name worker", "--memory-mode none",
		"--max-tasks 3", "--model new"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("missing %q in %q", expected, joined)
		}
	}
	if strings.Contains(joined, "--debug") || strings.Contains(joined, "old") {
		t.Fatalf("stale flags were not replaced: %q", joined)
	}
}

func TestLaunchConfigAppliesReasoningAndContext(t *testing.T) {
	calls := make(chan execCall, 1)
	handler := &Handler{ExecutorID: 5, Version: "1.0.0", Inventory: &fakeInventory{}, Send: (&recorder{}).send,
		Args: []string{"connect", "--ws-url", "wss://x"},
		Exec: func(_ string, argv, _ []string) error { calls <- execCall{argv: argv}; return nil }}
	handler.Handle(context.Background(), frame(t, map[string]any{"executorId": 5, "requestId": "r6",
		"launch": map[string]any{"runtimeVersion": "1.0.0", "reasoningEffort": "high",
			"contextWindow": "128000"}}))
	call := waitCall(t, calls)
	joined := strings.Join(call.argv, " ")
	if !strings.Contains(joined, "--reasoning-effort high") || !strings.Contains(joined, "--context-window 128000") {
		t.Fatalf("reasoning/context not applied: %q", joined)
	}
}

func TestLaunchConfigRejectsCrossVersion(t *testing.T) {
	events := &recorder{}
	handler := &Handler{ExecutorID: 5, Version: "1.0.0", Inventory: &fakeInventory{}, Send: events.send}
	handler.Handle(context.Background(), frame(t, map[string]any{"executorId": 5, "requestId": "r7",
		"launch": map[string]any{"runtimeVersion": "2.0.0"}}))
	waitFor(t, func() bool { return len(events.statuses()) > 0 })
	if statuses := events.statuses(); statuses[0] != statusFailed {
		t.Fatalf("unexpected statuses: %v", statuses)
	}
}

func TestIgnoresWrongExecutorAndDuplicates(t *testing.T) {
	calls := make(chan execCall, 2)
	handler := &Handler{ExecutorID: 5, Inventory: &fakeInventory{}, Send: (&recorder{}).send,
		Exec: func(string, []string, []string) error { calls <- execCall{}; return nil }}
	handler.Handle(context.Background(), frame(t, map[string]any{"executorId": 9, "requestId": "r8"}))
	handler.Handle(context.Background(), frame(t, map[string]any{"executorId": 5, "requestId": "r9"}))
	waitCall(t, calls)
	handler.Handle(context.Background(), frame(t, map[string]any{"executorId": 5, "requestId": "r9"}))
	select {
	case <-calls:
		t.Fatal("duplicate request restarted twice")
	case <-time.After(50 * time.Millisecond):
	}
}

func waitCall(t *testing.T, calls chan execCall) execCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(2 * time.Second):
		t.Fatal("restart did not exec")
		return execCall{}
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition was not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
