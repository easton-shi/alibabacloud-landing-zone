package conversation

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/config"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/protocol"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/provider"
)

type fakeProvider struct {
	name string
	run  func(context.Context, provider.Request, func(provider.Event)) (provider.Result, error)
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Run(ctx context.Context, req provider.Request,
	emit func(provider.Event)) (provider.Result, error) {
	return f.run(ctx, req, emit)
}

type recorder struct {
	mu     sync.Mutex
	frames []any
	notify chan struct{}
}

func newRecorder() *recorder { return &recorder{notify: make(chan struct{}, 64)} }

func (r *recorder) Send(value any) error {
	r.mu.Lock()
	r.frames = append(r.frames, value)
	r.mu.Unlock()
	select {
	case r.notify <- struct{}{}:
	default:
	}
	return nil
}

func (r *recorder) snapshot() []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]any(nil), r.frames...)
}

func (r *recorder) waitFor(t *testing.T, match func(any) bool) any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, frame := range r.snapshot() {
			if match(frame) {
				return frame
			}
		}
		select {
		case <-r.notify:
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatalf("frame not observed; frames=%#v", r.snapshot())
	return nil
}

func capabilityServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, body := range map[string]string{
		"skills.json":   `{"skills":[],"mcpServers":[],"plugins":[]}`,
		"manifest.json": `{"schemaVersion":"autoWonder.taskPackage.v1"}`,
	} {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	archive := buffer.Bytes()
	digest := sha256.Sum256(archive)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)
	return server, hex.EncodeToString(digest[:])
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		ServerWSURL:   "ws://127.0.0.1:8080/ws/executor",
		WorkspaceRoot: t.TempDir(),
		MaxConcurrent: 2,
		Unattended:    true,
	}
}

func turnFrame(server *httptest.Server, digest string) protocol.ConversationTurn {
	return protocol.ConversationTurn{
		Type: "CONVERSATION_TURN", ExecutorID: 7, ConversationID: 100, TurnID: 200,
		DispatchAttempt: 1, Content: "hello", SystemPrompt: "You are the platform assistant.",
		CapabilityDownloadURL: server.URL + "/capability.zip", CapabilitySHA256: digest,
		CLISessionRef: "session-old",
	}
}

func sendFrame(t *testing.T, handler *Handler, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	handler.Handle(context.Background(), raw)
}

func TestHandlerStreamsTurnAndAcksSuccess(t *testing.T) {
	server, digest := capabilityServer(t)
	rec := newRecorder()
	engine := &fakeProvider{name: "codex", run: func(_ context.Context, req provider.Request,
		emit func(provider.Event)) (provider.Result, error) {
		if !strings.Contains(req.Prompt, "hello") || !strings.Contains(req.Prompt, "platform assistant") {
			t.Errorf("prompt missing conversation context: %q", req.Prompt)
		}
		if req.SessionID != "session-old" {
			t.Errorf("session id = %q, want session-old", req.SessionID)
		}
		emit(provider.Event{Kind: "session", SessionID: "session-new"})
		emit(provider.Event{Kind: "text", Text: "hi "})
		emit(provider.Event{Kind: "text", Text: "there"})
		return provider.Result{Summary: "hi there", SessionID: "session-new"}, nil
	}}
	handler := New(testConfig(t), rec, engine)
	sendFrame(t, handler, turnFrame(server, digest))

	ack := rec.waitFor(t, func(value any) bool {
		frame, ok := value.(protocol.ConversationTurnACK)
		return ok && frame.Type == "CONVERSATION_TURN_ACK"
	}).(protocol.ConversationTurnACK)
	if ack.Status != "SUCCESS" || ack.ReplyMarkdown != "hi there" || ack.SessionID != "session-new" {
		t.Fatalf("unexpected ack: %#v", ack)
	}
	if ack.ConversationID != 100 || ack.TurnID != 200 || ack.DispatchAttempt != 1 {
		t.Fatalf("ack routing fields lost: %#v", ack)
	}

	var text string
	for _, value := range rec.snapshot() {
		event, ok := value.(protocol.ConversationTurnEvent)
		if !ok || event.EventType != "text" {
			continue
		}
		var payload struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(event.PayloadFragment), &payload); err != nil {
			t.Fatal(err)
		}
		text += payload.Content
	}
	if text != "hi there" {
		t.Fatalf("streamed text = %q", text)
	}
}

func TestCancelSendsCanceledAckWithPartialReply(t *testing.T) {
	server, digest := capabilityServer(t)
	rec := newRecorder()
	started := make(chan struct{})
	engine := &fakeProvider{name: "opencode", run: func(ctx context.Context, _ provider.Request,
		emit func(provider.Event)) (provider.Result, error) {
		emit(provider.Event{Kind: "text", Text: "partial "})
		close(started)
		<-ctx.Done()
		return provider.Result{}, ctx.Err()
	}}
	handler := New(testConfig(t), rec, engine)
	sendFrame(t, handler, turnFrame(server, digest))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not start")
	}
	sendFrame(t, handler, protocol.ConversationCancel{
		Type: "CONVERSATION_TURN_CANCEL", ConversationID: 100, TurnID: 200, DispatchAttempt: 1,
	})
	ack := rec.waitFor(t, func(value any) bool {
		_, ok := value.(protocol.ConversationTurnACK)
		return ok
	}).(protocol.ConversationTurnACK)
	if ack.Status != "CANCELED" || ack.ReplyMarkdown != "partial " {
		t.Fatalf("unexpected cancel ack: %#v", ack)
	}
}

func TestACPElicitationRoundTrip(t *testing.T) {
	server, digest := capabilityServer(t)
	rec := newRecorder()
	engine := &fakeProvider{name: "codex", run: func(ctx context.Context, req provider.Request,
		emit func(provider.Event)) (provider.Result, error) {
		answer, err := req.RequestInput(ctx, provider.InputRequest{
			Kind: "INPUT", Key: "tool-1", Message: "Which branch?",
			Schema: map[string]any{"type": "object"}, TimeoutSeconds: 5,
		})
		if err != nil {
			return provider.Result{}, err
		}
		emit(provider.Event{Kind: "text", Text: "using " + fmt.Sprint(answer.Content["branch"])})
		return provider.Result{Summary: "done", SessionID: "session-acp"}, nil
	}}
	handler := New(testConfig(t), rec, engine)
	if !handler.SupportsACP() {
		t.Fatal("codex must advertise ACP")
	}
	sendFrame(t, handler, turnFrame(server, digest))

	opened := rec.waitFor(t, func(value any) bool {
		frame, ok := value.(protocol.ConversationTurnEvent)
		return ok && frame.EventType == "acp_elicitation"
	}).(protocol.ConversationTurnEvent)
	var payload struct {
		Data struct {
			RequestID string `json:"requestId"`
			Message   string `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(opened.PayloadFragment), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Data.RequestID == "" || payload.Data.Message != "Which branch?" {
		t.Fatalf("bad elicitation payload: %s", opened.PayloadFragment)
	}
	sendFrame(t, handler, protocol.ConversationElicitation{
		Type: "CONVERSATION_ELICITATION_REPLY", ConversationID: 100, TurnID: 200,
		RequestID: payload.Data.RequestID, Action: "accept",
		Content: map[string]any{"branch": "main"},
	})
	rec.waitFor(t, func(value any) bool {
		frame, ok := value.(protocol.ConversationTurnEvent)
		return ok && frame.EventType == "acp_elicitation_resolved"
	})
	ack := rec.waitFor(t, func(value any) bool {
		_, ok := value.(protocol.ConversationTurnACK)
		return ok
	}).(protocol.ConversationTurnACK)
	if ack.Status != "SUCCESS" || ack.ReplyMarkdown != "using main" {
		t.Fatalf("unexpected ack: %#v", ack)
	}
}

func TestCommandsProbeReportsUnsupported(t *testing.T) {
	rec := newRecorder()
	handler := New(testConfig(t), rec, &fakeProvider{name: "opencode"})
	sendFrame(t, handler, protocol.ConversationCommandsProbe{
		Type: "CONVERSATION_COMMANDS_PROBE", ConversationID: 100,
	})
	result := rec.waitFor(t, func(value any) bool {
		_, ok := value.(protocol.ConversationCommandsResult)
		return ok
	}).(protocol.ConversationCommandsResult)
	if result.Status != "UNSUPPORTED" || result.ConversationID != 100 {
		t.Fatalf("unexpected commands result: %#v", result)
	}
}

func TestInventoryIsNeverNil(t *testing.T) {
	handler := New(testConfig(t), newRecorder(), &fakeProvider{name: "pi"})
	running, computing := handler.Inventory()
	if running == nil || computing == nil {
		t.Fatalf("inventory must be non-nil: running=%#v computing=%#v", running, computing)
	}
}

func TestLongTextUsesFullTextInAck(t *testing.T) {
	server, digest := capabilityServer(t)
	rec := newRecorder()
	long := strings.Repeat("长文本", 400)
	engine := &fakeProvider{name: "claude", run: func(_ context.Context, _ provider.Request,
		emit func(provider.Event)) (provider.Result, error) {
		emit(provider.Event{Kind: "text", Text: "short", FullText: long})
		return provider.Result{Summary: "short"}, nil
	}}
	handler := New(testConfig(t), rec, engine)
	sendFrame(t, handler, turnFrame(server, digest))
	ack := rec.waitFor(t, func(value any) bool {
		_, ok := value.(protocol.ConversationTurnACK)
		return ok
	}).(protocol.ConversationTurnACK)
	if ack.ReplyMarkdown != long {
		t.Fatalf("reply truncated: got %d bytes want %d", len(ack.ReplyMarkdown), len(long))
	}
}

func TestFinalProviderTextWinsOverIntermediateChunks(t *testing.T) {
	server, digest := capabilityServer(t)
	rec := newRecorder()
	engine := &fakeProvider{name: "claude", run: func(_ context.Context, _ provider.Request,
		emit func(provider.Event)) (provider.Result, error) {
		emit(provider.Event{Kind: "text", Text: "let me check", FullText: "let me check"})
		return provider.Result{Summary: "final answer", FullText: "final answer"}, nil
	}}
	handler := New(testConfig(t), rec, engine)
	sendFrame(t, handler, turnFrame(server, digest))
	ack := rec.waitFor(t, func(value any) bool {
		_, ok := value.(protocol.ConversationTurnACK)
		return ok
	}).(protocol.ConversationTurnACK)
	if ack.ReplyMarkdown != "final answer" {
		t.Fatalf("reply = %q, want final answer", ack.ReplyMarkdown)
	}
}

func TestReliableAnswerAcksReceivedThenWritten(t *testing.T) {
	server, digest := capabilityServer(t)
	rec := newRecorder()
	engine := &fakeProvider{name: "codex", run: func(ctx context.Context, req provider.Request,
		emit func(provider.Event)) (provider.Result, error) {
		answer, err := req.RequestInput(ctx, provider.InputRequest{
			Kind: "INPUT", Key: "tool-1", Message: "Which branch?", TimeoutSeconds: 5,
		})
		if err != nil {
			return provider.Result{}, err
		}
		emit(provider.Event{Kind: "text", Text: "using " + fmt.Sprint(answer.Content["branch"])})
		return provider.Result{Summary: "done", SessionID: "session-acp"}, nil
	}}
	handler := New(testConfig(t), rec, engine)
	frame := turnFrame(server, digest)
	frame.ConversationAnswerProtocolVersion = 1
	sendFrame(t, handler, frame)

	opened := rec.waitFor(t, func(value any) bool {
		event, ok := value.(protocol.ConversationTurnEvent)
		return ok && event.EventType == "acp_elicitation"
	}).(protocol.ConversationTurnEvent)
	var payload struct {
		Data struct {
			RequestID             string `json:"requestId"`
			AnswerProtocolVersion int    `json:"answerProtocolVersion"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(opened.PayloadFragment), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Data.RequestID == "" || payload.Data.AnswerProtocolVersion != 1 {
		t.Fatalf("bad reliable elicitation payload: %s", opened.PayloadFragment)
	}
	sendFrame(t, handler, protocol.ConversationElicitation{
		Type: "CONVERSATION_ELICITATION_REPLY", ConversationID: 100, TurnID: 200,
		RequestID: payload.Data.RequestID, Action: "accept", AnswerProtocolVersion: 1,
		AnswerID: "answer-1", DispatchAttempt: 1, Content: map[string]any{"branch": "main"},
	})
	rec.waitFor(t, func(value any) bool {
		ack, ok := value.(protocol.ConversationElicitationReplyACK)
		return ok && ack.Status == "WRITTEN" && ack.AnswerID == "answer-1" &&
			ack.AnswerProtocolVersion == 1
	})
	var stages []string
	for _, value := range rec.snapshot() {
		if ack, ok := value.(protocol.ConversationElicitationReplyACK); ok {
			stages = append(stages, ack.Status)
		}
	}
	if strings.Join(stages, ",") != "RECEIVED,WRITTEN" {
		t.Fatalf("ack stages = %v", stages)
	}
	rec.waitFor(t, func(value any) bool {
		_, ok := value.(protocol.ConversationTurnACK)
		return ok
	})
}

func TestDuplicateReliableAnswerIsAckedWithoutRedelivery(t *testing.T) {
	server, digest := capabilityServer(t)
	rec := newRecorder()
	calls := 0
	engine := &fakeProvider{name: "codex", run: func(ctx context.Context, req provider.Request,
		emit func(provider.Event)) (provider.Result, error) {
		calls++
		_, err := req.RequestInput(ctx, provider.InputRequest{Kind: "INPUT", Key: "tool-1", TimeoutSeconds: 5})
		if err != nil {
			return provider.Result{}, err
		}
		return provider.Result{Summary: "done"}, nil
	}}
	handler := New(testConfig(t), rec, engine)
	frame := turnFrame(server, digest)
	frame.ConversationAnswerProtocolVersion = 1
	sendFrame(t, handler, frame)
	opened := rec.waitFor(t, func(value any) bool {
		event, ok := value.(protocol.ConversationTurnEvent)
		return ok && event.EventType == "acp_elicitation"
	}).(protocol.ConversationTurnEvent)
	var payload struct {
		Data struct {
			RequestID string `json:"requestId"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(opened.PayloadFragment), &payload); err != nil {
		t.Fatal(err)
	}
	reply := protocol.ConversationElicitation{
		Type: "CONVERSATION_ELICITATION_REPLY", ConversationID: 100, TurnID: 200,
		RequestID: payload.Data.RequestID, Action: "accept", AnswerProtocolVersion: 1,
		AnswerID: "answer-dup", DispatchAttempt: 1, Content: map[string]any{"branch": "main"},
	}
	sendFrame(t, handler, reply)
	rec.waitFor(t, func(value any) bool {
		ack, ok := value.(protocol.ConversationElicitationReplyACK)
		return ok && ack.Status == "WRITTEN"
	})
	sendFrame(t, handler, reply)
	time.Sleep(100 * time.Millisecond)
	if calls != 1 {
		t.Fatalf("provider input called %d times", calls)
	}
	if !hasAck(rec.snapshot(), "WRITTEN") {
		t.Fatal("duplicate answer did not replay WRITTEN ack")
	}
}

func TestReliableAnswerWithoutPendingGateIsUnavailable(t *testing.T) {
	rec := newRecorder()
	handler := New(testConfig(t), rec, &fakeProvider{name: "codex"})
	sendFrame(t, handler, protocol.ConversationElicitation{
		Type: "CONVERSATION_ELICITATION_REPLY", ConversationID: 100, TurnID: 200,
		RequestID: "missing", Action: "accept", AnswerProtocolVersion: 1,
		AnswerID: "answer-missing", DispatchAttempt: 1,
	})
	ack := rec.waitFor(t, func(value any) bool {
		_, ok := value.(protocol.ConversationElicitationReplyACK)
		return ok
	}).(protocol.ConversationElicitationReplyACK)
	if ack.Status != "UNAVAILABLE" {
		t.Fatalf("status = %q, want UNAVAILABLE", ack.Status)
	}
}

func hasAck(frames []any, status string) bool {
	for _, value := range frames {
		if ack, ok := value.(protocol.ConversationElicitationReplyACK); ok && ack.Status == status {
			return true
		}
	}
	return false
}

func TestReliableDeliveryReplaysUnackedFrames(t *testing.T) {
	server, digest := capabilityServer(t)
	rec := newRecorder()
	engine := &fakeProvider{name: "opencode", run: func(_ context.Context, _ provider.Request,
		emit func(provider.Event)) (provider.Result, error) {
		emit(provider.Event{Kind: "text", Text: "one"})
		return provider.Result{Summary: "one"}, nil
	}}
	handler := New(testConfig(t), rec, engine)
	frame := turnFrame(server, digest)
	frame.ConversationDeliveryProtocolVersion = 1
	sendFrame(t, handler, frame)
	rec.waitFor(t, func(value any) bool {
		_, ok := value.(protocol.ConversationTurnACK)
		return ok
	})
	var textSeq int64
	for _, value := range rec.snapshot() {
		if event, ok := value.(protocol.ConversationTurnEvent); ok && event.EventType == "text" {
			if event.ConversationDeliveryProtocolVersion != 1 {
				t.Fatalf("event lost delivery protocol: %#v", event)
			}
			textSeq = event.EventSeq
		}
	}
	if textSeq == 0 {
		t.Fatal("no text event observed")
	}
	sendFrame(t, handler, protocol.ConversationDeliveryAck{
		Type: "CONVERSATION_DELIVERY_ACK", ConversationDeliveryProtocolVersion: 1,
		FrameType: "CONVERSATION_TURN_EVENT", ConversationID: 100, TurnID: 200,
		DispatchAttempt: 1, EventSeq: textSeq, ChunkIndex: 0,
	})
	replay := newRecorder()
	handler.Attach(replay)
	replay.waitFor(t, func(value any) bool {
		ack, ok := value.(protocol.ConversationTurnACK)
		return ok && ack.ConversationDeliveryProtocolVersion == 1
	})
	for _, value := range replay.snapshot() {
		if event, ok := value.(protocol.ConversationTurnEvent); ok && event.EventSeq == textSeq {
			t.Fatalf("acknowledged event was replayed: %#v", event)
		}
	}
}

func TestDetachAbandonsTurnWithoutTerminalAck(t *testing.T) {
	server, digest := capabilityServer(t)
	rec := newRecorder()
	started := make(chan struct{})
	engine := &fakeProvider{name: "opencode", run: func(ctx context.Context, _ provider.Request,
		_ func(provider.Event)) (provider.Result, error) {
		close(started)
		<-ctx.Done()
		return provider.Result{}, ctx.Err()
	}}
	handler := New(testConfig(t), rec, engine)
	frame := turnFrame(server, digest)
	frame.ConversationDeliveryProtocolVersion = 1
	sendFrame(t, handler, frame)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not start")
	}
	handler.Detach()
	time.Sleep(200 * time.Millisecond)
	for _, value := range rec.snapshot() {
		if ack, ok := value.(protocol.ConversationTurnACK); ok {
			t.Fatalf("detached turn emitted terminal ack: %#v", ack)
		}
	}
}

func TestDuplicateTurnAfterTerminalAckIsNotRerun(t *testing.T) {
	server, digest := capabilityServer(t)
	rec := newRecorder()
	runs := 0
	engine := &fakeProvider{name: "claude", run: func(_ context.Context, _ provider.Request,
		_ func(provider.Event)) (provider.Result, error) {
		runs++
		return provider.Result{Summary: "done"}, nil
	}}
	handler := New(testConfig(t), rec, engine)
	frame := turnFrame(server, digest)
	frame.ConversationDeliveryProtocolVersion = 1
	sendFrame(t, handler, frame)
	rec.waitFor(t, func(value any) bool {
		_, ok := value.(protocol.ConversationTurnACK)
		return ok
	})
	sendFrame(t, handler, frame)
	time.Sleep(100 * time.Millisecond)
	if runs != 1 {
		t.Fatalf("provider ran %d times", runs)
	}
}

type fakeCommandProvider struct {
	fakeProvider
	commands []map[string]any
}

func (f *fakeCommandProvider) Commands() []map[string]any { return f.commands }

func TestSupportsACPDependsOnProviderAndOpenCodeMode(t *testing.T) {
	cfg := testConfig(t)
	cfg.OpenCodeACP = true
	handler := New(cfg, newRecorder(), &fakeProvider{name: "opencode"})
	if !handler.SupportsACP() {
		t.Fatal("opencode ACP mode must advertise ACP")
	}
	cfg.OpenCodeACP = false
	if New(cfg, newRecorder(), &fakeProvider{name: "opencode"}).SupportsACP() {
		t.Fatal("opencode CLI mode must not advertise ACP")
	}
	if New(cfg, newRecorder(), &fakeProvider{name: "codex"}).SupportsACP() == false {
		t.Fatal("codex must advertise ACP")
	}
}

func TestCommandsProbeReturnsProviderCatalog(t *testing.T) {
	rec := newRecorder()
	engine := &fakeCommandProvider{fakeProvider: fakeProvider{name: "opencode"},
		commands: []map[string]any{{"name": "review", "description": "review changes"}}}
	handler := New(testConfig(t), rec, engine)
	sendFrame(t, handler, protocol.ConversationCommandsProbe{
		Type: "CONVERSATION_COMMANDS_PROBE", ConversationID: 100,
	})
	result := rec.waitFor(t, func(value any) bool {
		_, ok := value.(protocol.ConversationCommandsResult)
		return ok
	}).(protocol.ConversationCommandsResult)
	if result.Status != "OK" || !strings.Contains(result.Commands, "review") {
		t.Fatalf("commands result = %#v", result)
	}
}

func TestConversationAckCarriesProviderUsage(t *testing.T) {
	server, digest := capabilityServer(t)
	rec := newRecorder()
	engine := &fakeProvider{name: "opencode", run: func(_ context.Context, _ provider.Request,
		emit func(provider.Event)) (provider.Result, error) {
		emit(provider.Event{Kind: "text", Text: "done"})
		return provider.Result{Summary: "done", FullText: "done", Usage: &provider.Usage{
			Provider: "opencode", Model: "opencode/big-pickle", InputTokens: 10, OutputTokens: 2,
			CacheReadTokens: 3, ReasoningTokens: 1,
		}}, nil
	}}
	handler := New(testConfig(t), rec, engine)
	sendFrame(t, handler, turnFrame(server, digest))
	ack := rec.waitFor(t, func(value any) bool {
		_, ok := value.(protocol.ConversationTurnACK)
		return ok
	}).(protocol.ConversationTurnACK)
	if ack.Usage == nil || ack.Usage.InputTokens != 10 || ack.Usage.CacheReadTokens != 3 ||
		ack.Usage.Model != "opencode/big-pickle" {
		t.Fatalf("usage = %#v", ack.Usage)
	}
}
