package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/config"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/protocol"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/provider"
)

func TestUploadCompletionCheckpointPinsResultIdentity(t *testing.T) {
	var purpose, resultID, checkpointSeq, providerName string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", req.Method)
		}
		if token := req.URL.Query().Get("token"); token != "executor-token" {
			t.Errorf("token = %q, want executor-token", token)
		}
		if err := req.ParseMultipartForm(2 << 20); err != nil {
			t.Errorf("parse multipart form: %v", err)
		}
		purpose = req.FormValue("purpose")
		resultID = req.FormValue("resultId")
		checkpointSeq = req.FormValue("checkpointSeq")
		providerName = req.FormValue("provider")
		_, _ = w.Write([]byte(`{"checkpointSeq":8,"sha256":"sha256:server-receipt"}`))
	}))
	defer server.Close()

	runner := New(config.Config{ExecutorToken: "executor-token"}, &mockSender{}, plainProvider{})
	outcome := runOutcome{
		result: protocol.Result{
			Type:                  "TASK_RESULT",
			DispatchID:            10002,
			ResultProtocolVersion: 2,
			ResultID:              "11111111-1111-4111-8111-111111111111",
			Success:               true,
		},
		sessionID: "session-1",
	}
	frame := protocol.Dispatch{
		DispatchID:           10002,
		SDLCStepID:           10002,
		CheckpointUploadPath: server.URL,
		ResumeCheckpointSeq:  7,
	}

	if err := runner.uploadCompletionCheckpoint(context.Background(), frame, &outcome); err != nil {
		t.Fatal(err)
	}
	if purpose != "COMPLETION" {
		t.Fatalf("purpose = %q, want COMPLETION", purpose)
	}
	if resultID != outcome.result.ResultID {
		t.Fatalf("resultId = %q, want %q", resultID, outcome.result.ResultID)
	}
	if checkpointSeq != "8" {
		t.Fatalf("checkpointSeq = %q, want 8", checkpointSeq)
	}
	if providerName != "plain" {
		t.Fatalf("provider = %q, want plain", providerName)
	}
	if outcome.result.CheckpointSeq != 8 {
		t.Fatalf("result checkpointSeq = %d, want 8", outcome.result.CheckpointSeq)
	}
	if outcome.result.CheckpointSHA256 != "sha256:server-receipt" {
		t.Fatalf("result checkpointSha256 = %q, want server receipt", outcome.result.CheckpointSHA256)
	}
}

type mockSteerableProvider struct {
	mu             sync.Mutex
	steerCalled    bool
	steerCallCount int
	dispatchID     int64
	content        string
	steerErr       error
	steerStarted   chan struct{}
	blockSteer     chan struct{}
}

type plainProvider struct{}

func (plainProvider) Name() string { return "plain" }

func (plainProvider) Run(context.Context, provider.Request, func(provider.Event)) (provider.Result, error) {
	return provider.Result{}, nil
}

func (m *mockSteerableProvider) Name() string { return "mock" }

func (m *mockSteerableProvider) Run(ctx context.Context, req provider.Request, emit func(provider.Event)) (provider.Result, error) {
	return provider.Result{}, nil
}

func (m *mockSteerableProvider) Steer(ctx context.Context, dispatchID int64, content string) error {
	m.mu.Lock()
	m.steerCalled = true
	m.steerCallCount++
	m.dispatchID = dispatchID
	m.content = content
	err := m.steerErr
	m.mu.Unlock()

	if m.steerStarted != nil {
		select {
		case m.steerStarted <- struct{}{}:
		default:
		}
	}
	if m.blockSteer != nil {
		<-m.blockSteer
	}
	return err
}

func (m *mockSteerableProvider) getSteerCalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.steerCalled
}

func (m *mockSteerableProvider) getSteerCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.steerCallCount
}

func (m *mockSteerableProvider) getDispatchID() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dispatchID
}

func (m *mockSteerableProvider) getContent() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.content
}

type mockSender struct {
	mu     sync.Mutex
	frames []any
}

func (m *mockSender) Send(value any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.frames = append(m.frames, value)
	return nil
}

func (m *mockSender) getFrames() []any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]any(nil), m.frames...)
}

func TestRunnerHandlesTaskSteer(t *testing.T) {
	engine := &mockSteerableProvider{}
	sender := &mockSender{}
	runner := New(config.Config{}, sender, engine)
	runner.running[123] = func() {}

	frame := protocol.TaskSteer{
		Type:       "TASK_STEER",
		GuidanceID: 1,
		DispatchID: 123,
		ExecutorID: 456,
		Content:    "new guidance",
	}
	raw, _ := json.Marshal(frame)

	runner.Handle(context.Background(), raw)

	if !engine.getSteerCalled() {
		t.Fatal("expected provider.Steer to be called")
	}
	if engine.getDispatchID() != 123 {
		t.Fatalf("expected dispatch ID 123, got %d", engine.getDispatchID())
	}
	if engine.getContent() != "new guidance" {
		t.Fatalf("expected content 'new guidance', got %q", engine.getContent())
	}

	var ack protocol.TaskSteerAck
	found := false
	for _, f := range sender.getFrames() {
		if a, ok := f.(protocol.TaskSteerAck); ok {
			ack = a
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected TASK_STEER_ACK to be sent")
	}
	if ack.Status != "APPLIED" {
		t.Fatalf("expected status APPLIED, got %s", ack.Status)
	}
}

func TestRunnerDeduplicatesTaskSteer(t *testing.T) {
	engine := &mockSteerableProvider{}
	sender := &mockSender{}
	runner := New(config.Config{}, sender, engine)
	runner.running[123] = func() {}

	frame := protocol.TaskSteer{
		Type:       "TASK_STEER",
		GuidanceID: 1,
		DispatchID: 123,
		ExecutorID: 456,
		Content:    "new guidance",
	}
	raw, _ := json.Marshal(frame)

	runner.Handle(context.Background(), raw)
	runner.Handle(context.Background(), raw)

	if engine.getSteerCallCount() != 1 {
		t.Fatalf("expected provider.Steer to be called once, got %d", engine.getSteerCallCount())
	}

	ackCount := 0
	for _, f := range sender.getFrames() {
		if _, ok := f.(protocol.TaskSteerAck); ok {
			ackCount++
		}
	}
	if ackCount != 2 {
		t.Fatalf("expected 2 TASK_STEER_ACK frames (one original, one replay), got %d", ackCount)
	}
}

func TestRunnerHandlesTaskGuidance(t *testing.T) {
	engine := &mockSteerableProvider{}
	sender := &mockSender{}
	runner := New(config.Config{}, sender, engine)
	runner.running[123] = func() {}

	frame := protocol.TaskGuidance{
		Type:       "TASK_GUIDANCE",
		GuidanceID: 77,
		DispatchID: 123,
		Content:    "safe steering",
		Mode:       "SAFE_STEER",
	}
	raw, _ := json.Marshal(frame)
	runner.Handle(context.Background(), raw)

	if engine.getSteerCallCount() != 1 {
		t.Fatalf("expected one steer call, got %d", engine.getSteerCallCount())
	}
	if engine.getContent() != "safe steering" {
		t.Fatalf("unexpected steering content %q", engine.getContent())
	}
	ack := findTaskGuidanceAck(t, sender.getFrames())
	if ack.Status != "APPLIED" || ack.GuidanceID != 77 || ack.DispatchID != 123 {
		t.Fatalf("unexpected guidance ack: %#v", ack)
	}
}

func TestRunnerDeduplicatesTaskGuidance(t *testing.T) {
	engine := &mockSteerableProvider{}
	sender := &mockSender{}
	runner := New(config.Config{}, sender, engine)
	runner.running[123] = func() {}

	frame := protocol.TaskGuidance{
		Type:       "TASK_GUIDANCE",
		GuidanceID: 88,
		DispatchID: 123,
		Content:    "safe steering",
		Mode:       "SAFE_STEER",
	}
	raw, _ := json.Marshal(frame)
	runner.Handle(context.Background(), raw)
	runner.Handle(context.Background(), raw)

	if engine.getSteerCallCount() != 1 {
		t.Fatalf("expected one steer call, got %d", engine.getSteerCallCount())
	}
	if countTaskGuidanceAcks(sender.getFrames()) != 2 {
		t.Fatalf("expected original and replayed guidance ack, got %d", countTaskGuidanceAcks(sender.getFrames()))
	}
}

func TestRunnerRejectsSteerWithoutActiveTurn(t *testing.T) {
	engine := &mockSteerableProvider{}
	sender := &mockSender{}
	runner := New(config.Config{}, sender, engine)

	raw, _ := json.Marshal(protocol.TaskSteer{
		Type: "TASK_STEER", GuidanceID: 101, DispatchID: 999, Content: "late",
	})
	runner.Handle(context.Background(), raw)

	if engine.getSteerCallCount() != 0 {
		t.Fatalf("provider must not be called without an active turn, got %d calls", engine.getSteerCallCount())
	}
	ack := findTaskSteerAck(t, sender.getFrames())
	if ack.Status != "FAILED" || ack.Error == "" {
		t.Fatalf("unexpected failed ack: %#v", ack)
	}
}

func TestRunnerReplaysSteerFailure(t *testing.T) {
	engine := &mockSteerableProvider{steerErr: errors.New("handoff failed")}
	sender := &mockSender{}
	runner := New(config.Config{}, sender, engine)
	runner.running[123] = func() {}

	raw, _ := json.Marshal(protocol.TaskSteer{
		Type: "TASK_STEER", GuidanceID: 102, DispatchID: 123, Content: "steer",
	})
	runner.Handle(context.Background(), raw)
	runner.Handle(context.Background(), raw)

	if engine.getSteerCallCount() != 1 {
		t.Fatalf("failed steering must be deduplicated, got %d calls", engine.getSteerCallCount())
	}
	acks := findTaskSteerAcks(sender.getFrames())
	if len(acks) != 2 || acks[0].Status != "FAILED" || acks[1].Status != "FAILED" {
		t.Fatalf("unexpected replayed acks: %#v", acks)
	}
	if acks[0].Error != "handoff failed" || acks[1].Error != "handoff failed" {
		t.Fatalf("cached failure was not replayed: %#v", acks)
	}
}

func TestRunnerReplaysGuidanceFailure(t *testing.T) {
	engine := &mockSteerableProvider{steerErr: errors.New("guidance failed")}
	sender := &mockSender{}
	runner := New(config.Config{}, sender, engine)
	runner.running[123] = func() {}

	raw, _ := json.Marshal(protocol.TaskGuidance{
		Type: "TASK_GUIDANCE", GuidanceID: 104, DispatchID: 123, Content: "steer", Mode: "SAFE_STEER",
	})
	runner.Handle(context.Background(), raw)
	runner.Handle(context.Background(), raw)

	if engine.getSteerCallCount() != 1 {
		t.Fatalf("failed guidance must be deduplicated, got %d calls", engine.getSteerCallCount())
	}
	acks := findTaskGuidanceAcks(t, sender.getFrames())
	if len(acks) != 2 {
		t.Fatalf("expected original and replayed guidance acks, got %d", len(acks))
	}
	for _, ack := range acks {
		if ack.Status != "FAILED" || ack.Error != "guidance failed" {
			t.Fatalf("cached guidance failure was not replayed: %#v", acks)
		}
	}
}

func TestRunnerHandlesConcurrentGuidance(t *testing.T) {
	engine := &mockSteerableProvider{
		steerStarted: make(chan struct{}, 2),
		blockSteer:   make(chan struct{}),
	}
	sender := &mockSender{}
	runner := New(config.Config{}, sender, engine)
	runner.running[123] = func() {}

	first, _ := json.Marshal(protocol.TaskGuidance{
		Type: "TASK_GUIDANCE", GuidanceID: 105, DispatchID: 123, Content: "first", Mode: "SAFE_STEER",
	})
	second, _ := json.Marshal(protocol.TaskGuidance{
		Type: "TASK_GUIDANCE", GuidanceID: 106, DispatchID: 123, Content: "second", Mode: "SAFE_STEER",
	})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		runner.Handle(context.Background(), first)
	}()
	go func() {
		defer wg.Done()
		runner.Handle(context.Background(), second)
	}()

	select {
	case <-engine.steerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first guidance did not reach the provider")
	}
	time.Sleep(20 * time.Millisecond)
	if engine.getSteerCallCount() != 1 {
		t.Fatalf("second guidance should wait for the first, got %d calls", engine.getSteerCallCount())
	}
	close(engine.blockSteer)
	wg.Wait()

	if engine.getSteerCallCount() != 2 {
		t.Fatalf("expected both guidance frames to be applied, got %d calls", engine.getSteerCallCount())
	}
	if countTaskGuidanceAcks(sender.getFrames()) != 2 {
		t.Fatalf("expected two guidance acks, got %d", countTaskGuidanceAcks(sender.getFrames()))
	}
}

func TestRunnerDeduplicatesConcurrentGuidance(t *testing.T) {
	engine := &mockSteerableProvider{
		steerStarted: make(chan struct{}, 2),
		blockSteer:   make(chan struct{}),
	}
	sender := &mockSender{}
	runner := New(config.Config{}, sender, engine)
	runner.running[123] = func() {}

	raw, _ := json.Marshal(protocol.TaskGuidance{
		Type: "TASK_GUIDANCE", GuidanceID: 107, DispatchID: 123, Content: "once", Mode: "SAFE_STEER",
	})
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			runner.Handle(context.Background(), raw)
		}()
	}

	select {
	case <-engine.steerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("guidance did not reach the provider")
	}
	time.Sleep(20 * time.Millisecond)
	if engine.getSteerCallCount() != 1 {
		t.Fatalf("duplicate guidance should wait for the first, got %d calls", engine.getSteerCallCount())
	}
	close(engine.blockSteer)
	wg.Wait()

	if engine.getSteerCallCount() != 1 {
		t.Fatalf("duplicate guidance was applied more than once: %d", engine.getSteerCallCount())
	}
	if countTaskGuidanceAcks(sender.getFrames()) != 2 {
		t.Fatalf("expected applied and replayed guidance acks, got %d", countTaskGuidanceAcks(sender.getFrames()))
	}
}

func TestRunnerRejectsGuidanceWhenProviderIsNotSteerable(t *testing.T) {
	sender := &mockSender{}
	runner := New(config.Config{}, sender, plainProvider{})
	runner.running[123] = func() {}

	raw, _ := json.Marshal(protocol.TaskGuidance{
		Type: "TASK_GUIDANCE", GuidanceID: 103, DispatchID: 123, Content: "steer", Mode: "SAFE_STEER",
	})
	runner.Handle(context.Background(), raw)

	ack := findTaskGuidanceAck(t, sender.getFrames())
	if ack.Status != "FAILED" || ack.Error == "" {
		t.Fatalf("unexpected unsupported-provider ack: %#v", ack)
	}
}

func findTaskSteerAck(t *testing.T, frames []any) protocol.TaskSteerAck {
	t.Helper()
	acks := findTaskSteerAcks(frames)
	if len(acks) == 0 {
		t.Fatal("expected TASK_STEER_ACK")
	}
	return acks[0]
}

func findTaskSteerAcks(frames []any) []protocol.TaskSteerAck {
	var acks []protocol.TaskSteerAck
	for _, frame := range frames {
		if ack, ok := frame.(protocol.TaskSteerAck); ok {
			acks = append(acks, ack)
		}
	}
	return acks
}

func findTaskGuidanceAck(t *testing.T, frames []any) protocol.TaskGuidanceAck {
	t.Helper()
	acks := findTaskGuidanceAcks(t, frames)
	if len(acks) > 0 {
		return acks[0]
	}
	t.Fatal("expected TASK_GUIDANCE_ACK")
	return protocol.TaskGuidanceAck{}
}

func findTaskGuidanceAcks(t *testing.T, frames []any) []protocol.TaskGuidanceAck {
	t.Helper()
	var acks []protocol.TaskGuidanceAck
	for _, frame := range frames {
		if ack, ok := frame.(protocol.TaskGuidanceAck); ok {
			acks = append(acks, ack)
		}
	}
	return acks
}

func countTaskGuidanceAcks(frames []any) int {
	count := 0
	for _, frame := range frames {
		if _, ok := frame.(protocol.TaskGuidanceAck); ok {
			count++
		}
	}
	return count
}
