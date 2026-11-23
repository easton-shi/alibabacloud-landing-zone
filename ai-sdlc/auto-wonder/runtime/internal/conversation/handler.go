// Package conversation executes interactive CONVERSATION_TURN frames.
//
// Unlike dispatch execution there is no SDLC step, checkpoint or artifact
// upload. A turn downloads the server-built capability bundle, materializes the
// conversation's bound repositories into a stable workspace, runs one agent
// turn with the conversation's MCP identity, streams provider events, and
// closes the turn with a terminal ACK.
package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/config"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/protocol"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/provider"
)

// Sender is the narrow write surface used by the handler.
type Sender interface {
	Send(value any) error
}

var errDetached = errors.New("conversation handler is not attached to a websocket session")

type turnKey struct {
	conversationID int64
	turnID         int64
}

type deliveryKey struct {
	conversationID int64
	turnID         int64
	attempt        int
	seq            int64
	chunk          int
}

// Handler owns conversation concurrency and ACP reply routing. It survives
// WebSocket reconnects so unacknowledged reliable frames can be replayed.
type Handler struct {
	cfg        config.Config
	provider   provider.Provider
	sem        chan struct{}
	executorID int64

	mu               sync.Mutex
	send             Sender
	attached         bool
	retryCancel      context.CancelFunc
	turns            map[turnKey]*turn
	byTurn           map[int64]*turn
	draining         bool
	computing        map[int64]bool
	outbox           map[deliveryKey]json.RawMessage
	outboxStore      *deliveryOutbox
	answerReceipts   *answerReceiptStore
	attachmentPolicy attachmentPolicy
}

// New builds a conversation handler for the configured provider. A non-nil
// sender immediately attaches the handler, which is the normal shape in tests
// and in embedders; the runtime itself reattaches on every reconnect.
func New(cfg config.Config, send Sender, engine provider.Provider) *Handler {
	executorID, _ := strconv.ParseInt(cfg.ExecutorID, 10, 64)
	outboxStore := newDeliveryOutbox(cfg.WorkspaceRoot)
	persisted, err := outboxStore.Load()
	if err != nil {
		slog.Warn("load conversation outbox failed", "error", err)
		persisted = map[deliveryKey]json.RawMessage{}
	}
	h := &Handler{
		cfg:              cfg,
		provider:         engine,
		sem:              make(chan struct{}, cfg.MaxConcurrent),
		executorID:       executorID,
		turns:            map[turnKey]*turn{},
		byTurn:           map[int64]*turn{},
		computing:        map[int64]bool{},
		outbox:           persisted,
		outboxStore:      outboxStore,
		answerReceipts:   newAnswerReceiptStore(cfg.WorkspaceRoot),
		attachmentPolicy: newAttachmentPolicy(cfg.BaseURL(), cfg.AttachmentAllowedHosts),
	}
	if send != nil {
		h.Attach(send)
	}
	return h
}

// Attach binds the handler to one WebSocket session and starts the reliable
// delivery replay loop. Frames already in the outbox are resent immediately.
func (h *Handler) Attach(sender Sender) {
	h.mu.Lock()
	h.send = sender
	h.attached = true
	if h.retryCancel != nil {
		h.retryCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.retryCancel = cancel
	h.mu.Unlock()
	go h.retryLoop(ctx)
	h.replay()
}

// Detach marks the current session as lost. In-flight turns are abandoned
// without emitting a terminal ACK: the server must see the turn as still
// processing and recover it, not as user-canceled.
func (h *Handler) Detach() {
	h.mu.Lock()
	h.attached = false
	h.send = nil
	if h.retryCancel != nil {
		h.retryCancel()
		h.retryCancel = nil
	}
	turns := make([]*turn, 0, len(h.turns))
	for _, t := range h.turns {
		t.markAbandoned()
		turns = append(turns, t)
	}
	h.mu.Unlock()
	for _, t := range turns {
		t.cancel()
	}
}

func (h *Handler) retryLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.replay()
		}
	}
}

func (h *Handler) replay() {
	h.mu.Lock()
	if !h.attached || h.send == nil {
		h.mu.Unlock()
		return
	}
	keys := make([]deliveryKey, 0, len(h.outbox))
	for key := range h.outbox {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return deliveryKeyBefore(keys[i], keys[j])
	})
	frames := make([]json.RawMessage, 0, len(keys))
	for _, key := range keys {
		frames = append(frames, h.outbox[key])
	}
	h.mu.Unlock()
	for _, frame := range frames {
		_ = h.sendFrame(frame)
	}
}

// SupportsACP reports whether this provider exposes a native human-input RPC.
// Only Codex app-server currently has one; declaring ACP for the others would
// tell the server it can render answer cards that the runtime cannot settle.
func (h *Handler) SupportsACP() bool {
	if h.provider == nil {
		return false
	}
	return h.provider.Name() == "codex" || (h.provider.Name() == "opencode" && h.cfg.OpenCodeACP)
}

// Handle routes one inbound frame. Unknown frames are ignored.
func (h *Handler) Handle(parent context.Context, raw []byte) {
	var envelope protocol.Frame
	if json.Unmarshal(raw, &envelope) != nil {
		return
	}
	switch envelope.Type {
	case "CONVERSATION_TURN":
		var frame protocol.ConversationTurn
		if json.Unmarshal(raw, &frame) != nil || frame.ConversationID <= 0 || frame.TurnID <= 0 {
			return
		}
		h.start(parent, frame)
	case "CONVERSATION_TURN_CANCEL":
		var frame protocol.ConversationCancel
		if json.Unmarshal(raw, &frame) == nil {
			h.cancel(frame.ConversationID, frame.TurnID, frame.DispatchAttempt)
		}
	case "CONVERSATION_ELICITATION_REPLY":
		var frame protocol.ConversationElicitation
		if json.Unmarshal(raw, &frame) == nil {
			h.resolveElicitation(frame)
		}
	case "CONVERSATION_COMMANDS_PROBE":
		var frame protocol.ConversationCommandsProbe
		if json.Unmarshal(raw, &frame) == nil {
			h.commandsProbe(frame)
		}
	case "CONVERSATION_DELIVERY_ACK":
		var frame protocol.ConversationDeliveryAck
		if json.Unmarshal(raw, &frame) == nil {
			h.ackDelivery(frame)
		}
	}
}

func (h *Handler) start(parent context.Context, frame protocol.ConversationTurn) {
	key := turnKey{conversationID: frame.ConversationID, turnID: frame.TurnID}
	h.mu.Lock()
	if h.draining {
		h.mu.Unlock()
		slog.Info("conversation turn ignored while draining", "conversationId", frame.ConversationID,
			"turnId", frame.TurnID)
		return
	}
	if _, exists := h.turns[key]; exists {
		h.mu.Unlock()
		slog.Warn("duplicate conversation turn ignored", "conversationId", frame.ConversationID,
			"turnId", frame.TurnID)
		return
	}
	// A terminal ACK already produced for this attempt must be replayed, not
	// executed a second time. Drop outbox entries from older attempts so stale
	// reconnect replays cannot outlive a recovery retry.
	for outKey := range h.outbox {
		if outKey.conversationID != frame.ConversationID || outKey.turnID != frame.TurnID {
			continue
		}
		if outKey.attempt > 0 && frame.DispatchAttempt > 0 && outKey.attempt < frame.DispatchAttempt {
			delete(h.outbox, outKey)
			_ = h.outboxStore.Delete(outKey)
			continue
		}
		if outKey.seq == 0 && outKey.attempt >= frame.DispatchAttempt {
			h.mu.Unlock()
			h.replay()
			return
		}
	}
	ctx, cancel := context.WithCancel(parent)
	t := newTurn(h, ctx, cancel, frame)
	h.turns[key] = t
	h.byTurn[frame.TurnID] = t
	h.mu.Unlock()

	go h.execute(t)
}

func (h *Handler) execute(t *turn) {
	defer func() {
		h.mu.Lock()
		delete(h.turns, turnKey{conversationID: t.frame.ConversationID, turnID: t.frame.TurnID})
		delete(h.byTurn, t.frame.TurnID)
		delete(h.computing, t.frame.TurnID)
		h.mu.Unlock()
		t.cancel()
	}()

	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	case <-t.ctx.Done():
		t.finish("CANCELED", "", "")
		return
	}

	h.mu.Lock()
	h.computing[t.frame.TurnID] = true
	h.mu.Unlock()

	t.emitStatus("RUNNING", "")
	status, errText, sessionID := t.run()
	t.finish(status, errText, sessionID)
}

func (h *Handler) cancel(conversationID, turnID int64, attempt int) {
	h.mu.Lock()
	t := h.turns[turnKey{conversationID: conversationID, turnID: turnID}]
	h.mu.Unlock()
	if t == nil {
		return
	}
	if attempt > 0 && t.frame.DispatchAttempt > 0 && attempt != t.frame.DispatchAttempt {
		return
	}
	t.markCanceled()
	t.cancel()
}

func (h *Handler) resolveElicitation(frame protocol.ConversationElicitation) {
	existingReceived := false
	if frame.AnswerID != "" {
		receipt, err := h.answerReceipts.Get(frame)
		if err != nil {
			slog.Warn("read conversation answer receipt failed", "answerId", frame.AnswerID, "error", err)
			h.ackAnswerFrame(frame, "UNAVAILABLE")
			return
		}
		if receipt != nil && receipt.Status != "RECEIVED" {
			h.ackAnswerFrame(frame, receipt.Status)
			return
		}
		if receipt != nil {
			existingReceived = true
		} else {
			if err := h.answerReceipts.Save(frame, "RECEIVED"); err != nil {
				slog.Warn("persist conversation answer receipt failed", "answerId", frame.AnswerID, "error", err)
				h.ackAnswerFrame(frame, "UNAVAILABLE")
				return
			}
		}
	}
	h.mu.Lock()
	t := h.byTurn[frame.TurnID]
	h.mu.Unlock()
	if t == nil {
		if existingReceived {
			h.ackAnswerFrame(frame, "RECEIVED")
			return
		}
		// The turn may already be terminal. Durable delivery still requires a
		// stage so the server can stop resending the answer.
		_ = h.answerReceipts.Save(frame, "UNAVAILABLE")
		h.ackAnswerFrame(frame, "UNAVAILABLE")
		return
	}
	t.resolveElicitation(frame)
}

func (h *Handler) ackAnswerFrame(frame protocol.ConversationElicitation, stage string) {
	if frame.AnswerID == "" {
		return
	}
	_ = h.sendFrame(protocol.ConversationElicitationReplyACK{
		Type: "CONVERSATION_ELICITATION_REPLY_ACK", ExecutorID: frame.ExecutorID,
		ConversationID: frame.ConversationID, TurnID: frame.TurnID,
		DispatchAttempt: frame.DispatchAttempt, RequestID: frame.RequestID,
		AnswerID: frame.AnswerID, Status: stage,
		AnswerProtocolVersion: frame.AnswerProtocolVersion,
	})
}

func (h *Handler) commandsProbe(frame protocol.ConversationCommandsProbe) {
	if h.executorID > 0 && frame.ExecutorID > 0 && frame.ExecutorID != h.executorID {
		return
	}
	if source, ok := h.provider.(interface {
		CommandsFor(int64) []map[string]any
	}); ok {
		if commands := source.CommandsFor(frame.ConversationID); len(commands) > 0 {
			raw, err := json.Marshal(map[string]any{"availableCommands": commands})
			if err == nil {
				_ = h.sendFrame(protocol.ConversationCommandsResult{
					Type: "CONVERSATION_COMMANDS_RESULT", ExecutorID: h.executorID,
					ConversationID: frame.ConversationID,
					Status:         "OK", Commands: string(raw),
				})
				return
			}
		}
	}
	// Codex/Pi/Claude do not expose a stable slash-command catalog.
	_ = h.sendFrame(protocol.ConversationCommandsResult{
		Type: "CONVERSATION_COMMANDS_RESULT", ExecutorID: h.executorID,
		ConversationID: frame.ConversationID,
		Status:         "UNSUPPORTED", Error: "runtime does not expose a slash-command catalog",
	})
}

func (h *Handler) ackDelivery(frame protocol.ConversationDeliveryAck) {
	if frame.ConversationDeliveryProtocolVersion != 1 {
		return
	}
	seq := frame.EventSeq
	if frame.FrameType == "CONVERSATION_TURN_ACK" {
		seq = 0
	}
	key := deliveryKey{conversationID: frame.ConversationID, turnID: frame.TurnID,
		attempt: frame.DispatchAttempt, seq: seq, chunk: frame.ChunkIndex}
	h.mu.Lock()
	delete(h.outbox, key)
	h.mu.Unlock()
	if err := h.outboxStore.Delete(key); err != nil {
		slog.Warn("delete conversation outbox frame failed", "conversationId", key.conversationID,
			"turnId", key.turnID, "eventSeq", key.seq, "error", err)
	}
}

// Inventory returns running and actively computing turn IDs. Both slices are
// non-nil: the server rejects a heartbeat whose inventory arrays are null.
func (h *Handler) Inventory() (running []int64, computing []int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	running = make([]int64, 0, len(h.turns))
	computing = make([]int64, 0, len(h.computing))
	for _, t := range h.turns {
		running = append(running, t.frame.TurnID)
	}
	for id := range h.computing {
		computing = append(computing, id)
	}
	return running, computing
}

// Busy reports whether a conversation turn is running.
func (h *Handler) Busy() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.turns) > 0
}

// ConversationActive reports whether any turn for the conversation is live.
func (h *Handler) ConversationActive(conversationID int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for key := range h.turns {
		if key.conversationID == conversationID {
			return true
		}
	}
	return false
}

// SetDraining stops accepting new turns during a WHEN_IDLE restart.
func (h *Handler) SetDraining(draining bool) {
	h.mu.Lock()
	h.draining = draining
	h.mu.Unlock()
}

// CancelAll cancels every in-flight conversation turn.
func (h *Handler) CancelAll() {
	h.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(h.turns))
	for _, t := range h.turns {
		cancels = append(cancels, t.cancel)
	}
	h.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// WaitIdle waits until no conversation turn remains registered.
func (h *Handler) WaitIdle(ctx context.Context) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !h.Busy() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (h *Handler) sendFrame(value any) error {
	h.mu.Lock()
	sender := h.send
	attached := h.attached
	h.mu.Unlock()
	if !attached || sender == nil {
		return errDetached
	}
	if err := sender.Send(value); err != nil {
		slog.Warn("conversation frame send failed", "error", err)
		return fmt.Errorf("send conversation frame: %w", err)
	}
	return nil
}

// deliver stores reliable frames until the server acknowledges them, then
// sends immediately. Non-reliable frames are fire-and-forget.
func (h *Handler) deliver(value any, key deliveryKey, reliable bool) {
	if reliable {
		raw, err := json.Marshal(value)
		if err != nil {
			slog.Error("persist conversation outbox frame", "error", err)
			return
		}
		if err := h.outboxStore.Put(key, value); err != nil {
			slog.Error("persist conversation outbox frame", "error", err)
			return
		}
		h.mu.Lock()
		h.outbox[key] = json.RawMessage(raw)
		h.pruneOutboxLocked()
		h.mu.Unlock()
	}
	_ = h.sendFrame(value)
}

func (h *Handler) pruneOutboxLocked() {
	const limit = 20000
	for len(h.outbox) > limit {
		var oldest deliveryKey
		first := true
		for key := range h.outbox {
			if first || deliveryKeyBefore(key, oldest) {
				oldest = key
				first = false
			}
		}
		delete(h.outbox, oldest)
		_ = h.outboxStore.Delete(oldest)
	}
}

func deliveryKeyBefore(left, right deliveryKey) bool {
	if left.attempt != right.attempt {
		return left.attempt < right.attempt
	}
	if left.turnID != right.turnID {
		return left.turnID < right.turnID
	}
	if left.seq == 0 || right.seq == 0 {
		if left.seq == 0 && right.seq == 0 {
			return left.chunk < right.chunk
		}
		return right.seq == 0
	}
	if left.seq != right.seq {
		return left.seq < right.seq
	}
	if left.chunk != right.chunk {
		return left.chunk < right.chunk
	}
	return left.conversationID < right.conversationID
}
