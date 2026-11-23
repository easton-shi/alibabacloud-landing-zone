package dispatch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/config"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/protocol"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/provider"
)

type resultRecorder struct {
	mu     sync.Mutex
	frames []any
}

type callbackSender struct{ send func(any) }

func (s callbackSender) Send(value any) error {
	s.send(value)
	return nil
}

func (r *resultRecorder) Send(value any) error {
	r.mu.Lock()
	r.frames = append(r.frames, value)
	r.mu.Unlock()
	return nil
}

func (r *resultRecorder) snapshot() []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]any(nil), r.frames...)
}

type idleProvider struct{}

func (idleProvider) Name() string { return "codex" }
func (idleProvider) Run(_ context.Context, _ provider.Request,
	_ func(provider.Event)) (provider.Result, error) {
	return provider.Result{}, nil
}

type countingProvider struct{ runs atomic.Int32 }

func (p *countingProvider) Name() string { return "codex" }
func (p *countingProvider) Run(_ context.Context, _ provider.Request,
	_ func(provider.Event)) (provider.Result, error) {
	p.runs.Add(1)
	return provider.Result{}, nil
}

func TestResultOutboxSurvivesRunnerRestartAndAcknowledges(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{WorkspaceRoot: root, MaxConcurrent: 1}
	first := New(cfg, &resultRecorder{}, idleProvider{})
	result := protocol.Result{
		Type: "TASK_RESULT", DispatchID: 42, ResultProtocolVersion: 2,
		ResultID: "result-42", Success: true, ResultSummary: "done",
	}
	if err := first.outbox.Put(result); err != nil {
		t.Fatal(err)
	}

	recorder := &resultRecorder{}
	second := New(cfg, recorder, idleProvider{})
	second.ReplayOutbox()
	frames := recorder.snapshot()
	if len(frames) != 1 {
		t.Fatalf("replayed frames = %#v", frames)
	}
	raw, ok := frames[0].(json.RawMessage)
	if !ok {
		t.Fatalf("replayed frame type = %T", frames[0])
	}
	var replayed protocol.Result
	if err := json.Unmarshal(raw, &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.DispatchID != 42 || replayed.ResultID != "result-42" {
		t.Fatalf("replayed result = %#v", replayed)
	}

	second.HandleResultAck([]byte(`{"type":"TASK_RESULT_ACK","dispatchId":42,"accepted":true}`))
	if _, err := os.Stat(filepath.Join(root, ".runtime-outbox", "results", "dispatch-42.json")); !os.IsNotExist(err) {
		t.Fatalf("outbox file after accepted ack: %v", err)
	}
}

func TestResultOutboxRetainsRejectedAck(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{WorkspaceRoot: root, MaxConcurrent: 1}
	runner := New(cfg, &resultRecorder{}, idleProvider{})
	if err := runner.outbox.Put(protocol.Paused{
		Type: "TASK_PAUSED", DispatchID: 43, CheckpointSeq: 1, CheckpointSHA256: "sha256:test",
	}); err != nil {
		t.Fatal(err)
	}

	runner.HandleResultAck([]byte(`{"type":"TASK_RESULT_ACK","dispatchId":43,"accepted":false}`))
	if _, err := os.Stat(filepath.Join(root, ".runtime-outbox", "results", "dispatch-43.json")); err != nil {
		t.Fatalf("outbox file after rejected ack: %v", err)
	}
}

func TestTerminalFrameIsDurableBeforeSend(t *testing.T) {
	root := t.TempDir()
	observed := false
	sender := callbackSender{send: func(any) {
		_, err := os.Stat(filepath.Join(root, ".runtime-outbox", "results", "dispatch-44.json"))
		observed = err == nil
	}}
	runner := New(config.Config{WorkspaceRoot: root, MaxConcurrent: 1}, sender, idleProvider{})
	runner.sendTerminal(protocol.Result{Type: "TASK_RESULT", DispatchID: 44, ResultID: "result-44"})
	if !observed {
		t.Fatal("terminal frame was sent before it was persisted")
	}
}

func TestRedeliveredDispatchReplaysPendingResultWithoutRerun(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{WorkspaceRoot: root, MaxConcurrent: 1}
	engine := &countingProvider{}
	first := New(cfg, &resultRecorder{}, engine)
	if err := first.outbox.Put(protocol.Result{
		Type: "TASK_RESULT", DispatchID: 45, ResultProtocolVersion: 2,
		ResultID: "result-45", Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	recorder := &resultRecorder{}
	runner := New(cfg, recorder, engine)
	raw, _ := json.Marshal(protocol.Dispatch{Type: "TASK_DISPATCH", DispatchID: 45})
	runner.Handle(context.Background(), raw)
	if engine.runs.Load() != 0 {
		t.Fatalf("provider ran %d times for a pending result", engine.runs.Load())
	}
	if frames := recorder.snapshot(); len(frames) != 2 {
		t.Fatalf("replayed frames = %#v", frames)
	}
}
