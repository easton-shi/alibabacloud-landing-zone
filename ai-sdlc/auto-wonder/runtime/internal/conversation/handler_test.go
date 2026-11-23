package conversation

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
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

type commandProvider struct {
	fakeProvider
	commands map[int64][]map[string]any
}

func (p *commandProvider) CommandsFor(conversationID int64) []map[string]any {
	values := p.commands[conversationID]
	return append([]map[string]any(nil), values...)
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

func decodeFrame[T any](value any) (T, bool) {
	var decoded T
	raw, err := json.Marshal(value)
	if err != nil || json.Unmarshal(raw, &decoded) != nil {
		return decoded, false
	}
	return decoded, true
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

func TestConversationOutboxSurvivesRestartAndReplaysInSequence(t *testing.T) {
	cfg := testConfig(t)
	engine := &fakeProvider{name: "codex"}
	first := New(cfg, nil, engine)
	keys := []deliveryKey{
		{conversationID: 100, turnID: 200, attempt: 1, seq: 0, chunk: 0},
		{conversationID: 100, turnID: 200, attempt: 1, seq: 2, chunk: 0},
		{conversationID: 100, turnID: 200, attempt: 1, seq: 1, chunk: 0},
	}
	for _, key := range keys {
		if key.seq == 0 {
			first.deliver(protocol.ConversationTurnACK{
				Type: "CONVERSATION_TURN_ACK", ConversationID: key.conversationID,
				TurnID: key.turnID, DispatchAttempt: key.attempt, Status: "SUCCESS",
				ConversationDeliveryProtocolVersion: 1,
			}, key, true)
			continue
		}
		first.deliver(protocol.ConversationTurnEvent{
			Type: "CONVERSATION_TURN_EVENT", ConversationID: key.conversationID,
			TurnID: key.turnID, DispatchAttempt: key.attempt, EventSeq: key.seq,
			ConversationDeliveryProtocolVersion: 1,
		}, key, true)
	}

	rec := newRecorder()
	second := New(cfg, rec, engine)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(rec.snapshot()) < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	frames := rec.snapshot()
	if len(frames) != 3 {
		t.Fatalf("replayed frames = %#v", frames)
	}
	var sequences []int64
	for _, frame := range frames {
		event, ok := frame.(json.RawMessage)
		if !ok {
			t.Fatalf("replayed type = %T", frame)
		}
		var decoded protocol.ConversationTurnEvent
		if err := json.Unmarshal(event, &decoded); err != nil {
			t.Fatal(err)
		}
		sequences = append(sequences, decoded.EventSeq)
	}
	if !slices.Equal(sequences, []int64{1, 2, 0}) {
		t.Fatalf("replay sequence = %v", sequences)
	}
	for _, key := range keys {
		second.ackDelivery(protocol.ConversationDeliveryAck{
			ConversationDeliveryProtocolVersion: 1,
			FrameType:                           "CONVERSATION_TURN_EVENT",
			ConversationID:                      key.conversationID,
			TurnID:                              key.turnID,
			DispatchAttempt:                     key.attempt,
			EventSeq:                            key.seq,
			ChunkIndex:                          key.chunk,
		})
	}
	third := New(cfg, newRecorder(), engine)
	if got := len(third.outbox); got != 0 {
		t.Fatalf("outbox after ACK = %d", got)
	}
}

func TestCommandsProbeIsIsolatedByConversation(t *testing.T) {
	cfg := testConfig(t)
	cfg.ExecutorID = "7"
	engine := &commandProvider{
		fakeProvider: fakeProvider{name: "opencode"},
		commands: map[int64][]map[string]any{
			100: {{"name": "review", "description": "review conversation 100"}},
			101: {{"name": "deploy", "description": "deploy conversation 101"}},
		},
	}
	rec := newRecorder()
	handler := New(cfg, rec, engine)
	for _, conversationID := range []int64{100, 101} {
		sendFrame(t, handler, protocol.ConversationCommandsProbe{
			Type: "CONVERSATION_COMMANDS_PROBE", ExecutorID: 7, ConversationID: conversationID,
		})
	}
	for _, want := range []string{"review conversation 100", "deploy conversation 101"} {
		rec.waitFor(t, func(value any) bool {
			result, ok := value.(protocol.ConversationCommandsResult)
			return ok && strings.Contains(result.Commands, want)
		})
	}
	before := len(rec.snapshot())
	sendFrame(t, handler, protocol.ConversationCommandsProbe{
		Type: "CONVERSATION_COMMANDS_PROBE", ExecutorID: 99, ConversationID: 100,
	})
	time.Sleep(50 * time.Millisecond)
	if after := len(rec.snapshot()); after != before {
		t.Fatalf("foreign executor probe emitted %d frames", after-before)
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
		answer.ReportWritten(nil)
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
		answer.ReportWritten(nil)
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
		answer, err := req.RequestInput(ctx, provider.InputRequest{Kind: "INPUT", Key: "tool-1", TimeoutSeconds: 5})
		if err != nil {
			return provider.Result{}, err
		}
		answer.ReportWritten(nil)
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

func TestReliableAnswerReportsUnknownWhenProviderWriteFails(t *testing.T) {
	server, digest := capabilityServer(t)
	rec := newRecorder()
	engine := &fakeProvider{name: "codex", run: func(ctx context.Context, req provider.Request,
		emit func(provider.Event)) (provider.Result, error) {
		answer, err := req.RequestInput(ctx, provider.InputRequest{
			Kind: "INPUT", Key: "tool-1", TimeoutSeconds: 5,
		})
		if err != nil {
			return provider.Result{}, err
		}
		answer.ReportWritten(errors.New("provider write failed"))
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
	sendFrame(t, handler, protocol.ConversationElicitation{
		Type: "CONVERSATION_ELICITATION_REPLY", ConversationID: 100, TurnID: 200,
		RequestID: payload.Data.RequestID, Action: "accept", AnswerProtocolVersion: 1,
		AnswerID: "answer-unknown", DispatchAttempt: 1, Content: map[string]any{"branch": "main"},
	})
	rec.waitFor(t, func(value any) bool {
		ack, ok := value.(protocol.ConversationElicitationReplyACK)
		return ok && ack.Status == "UNKNOWN" && ack.AnswerID == "answer-unknown"
	})
	var stages []string
	for _, value := range rec.snapshot() {
		if ack, ok := value.(protocol.ConversationElicitationReplyACK); ok {
			stages = append(stages, ack.Status)
		}
	}
	if strings.Join(stages, ",") != "RECEIVED,UNKNOWN" {
		t.Fatalf("ack stages = %v", stages)
	}
}

func TestReliableAnswerReceiptReplaysAfterTurnTerminal(t *testing.T) {
	server, digest := capabilityServer(t)
	rec := newRecorder()
	engine := &fakeProvider{name: "codex", run: func(ctx context.Context, req provider.Request,
		emit func(provider.Event)) (provider.Result, error) {
		answer, err := req.RequestInput(ctx, provider.InputRequest{
			Kind: "INPUT", Key: "tool-1", TimeoutSeconds: 5,
		})
		if err != nil {
			return provider.Result{}, err
		}
		answer.ReportWritten(nil)
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
		AnswerID: "answer-receipt", DispatchAttempt: 1, Content: map[string]any{"branch": "main"},
	}
	sendFrame(t, handler, reply)
	rec.waitFor(t, func(value any) bool {
		_, ok := value.(protocol.ConversationTurnACK)
		return ok
	})
	before := len(rec.snapshot())
	reply.ReceiptOnly = true
	sendFrame(t, handler, reply)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(rec.snapshot()) == before {
		time.Sleep(10 * time.Millisecond)
	}
	frames := rec.snapshot()
	if len(frames) == before {
		t.Fatal("receipt-only redelivery produced no acknowledgement")
	}
	ack, ok := frames[len(frames)-1].(protocol.ConversationElicitationReplyACK)
	if !ok || ack.AnswerID != "answer-receipt" || ack.Status != "WRITTEN" {
		t.Fatalf("receipt-only ack = %#v", frames[len(frames)-1])
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
		ack, ok := decodeFrame[protocol.ConversationTurnACK](value)
		return ok && ack.ConversationDeliveryProtocolVersion == 1
	})
	for _, value := range replay.snapshot() {
		if event, ok := decodeFrame[protocol.ConversationTurnEvent](value); ok && event.EventSeq == textSeq {
			t.Fatalf("acknowledged event was replayed: %#v", event)
		}
	}
}

func TestConversationActiveReportsLiveTurn(t *testing.T) {
	handler := New(testConfig(t), nil, &fakeProvider{name: "codex"})
	handler.turns[turnKey{conversationID: 100, turnID: 200}] = &turn{}
	if !handler.ConversationActive(100) {
		t.Fatal("live conversation was not reported active")
	}
	if handler.ConversationActive(101) {
		t.Fatal("unrelated conversation was reported active")
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
	commands map[int64][]map[string]any
}

func (f *fakeCommandProvider) CommandsFor(conversationID int64) []map[string]any {
	return f.commands[conversationID]
}

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
		commands: map[int64][]map[string]any{
			100: {{"name": "review", "description": "review changes"}},
		}}
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
