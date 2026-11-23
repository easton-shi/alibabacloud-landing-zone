package conversation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/protocol"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/provider"
)

const elicitationTimeout = 25 * time.Minute

var errTurnCanceled = errors.New("conversation turn canceled")

type elicitationRequest struct {
	channel chan protocol.ConversationElicitation
}

// requestInput turns a native agent human gate into an ACP answer card and
// blocks until CONVERSATION_ELICITATION_REPLY arrives. The server settles the
// card from the matching acp_elicitation_resolved event.
func (t *turn) requestInput(ctx context.Context, input provider.InputRequest) (provider.InputAnswer, error) {
	if t.h == nil || !t.h.SupportsACP() {
		return provider.InputAnswer{}, fmt.Errorf("conversation provider has no interactive input channel")
	}
	requestID, request := t.registerElicitation()
	defer t.unregisterElicitation(requestID)

	schema := input.Schema
	if len(schema) == 0 {
		schema = map[string]any{"type": "object", "additionalProperties": true}
	}
	message := input.Message
	if message == "" {
		message = "The agent needs your input to continue."
	}
	data := map[string]any{
		"requestId":       requestID,
		"mode":            "form",
		"message":         message,
		"requestedSchema": schema,
		"toolCallId":      input.Key,
	}
	// The server only persists the durable answer protocol when the card says
	// so. Without this field it falls back to best-effort legacy delivery.
	if t.frame.ConversationAnswerProtocolVersion == 1 {
		data["answerProtocolVersion"] = 1
	}
	t.emit("acp_elicitation", map[string]any{"type": "acp_elicitation", "data": data})

	timeout := elicitationTimeout
	if input.TimeoutSeconds > 0 && time.Duration(input.TimeoutSeconds)*time.Second < timeout {
		timeout = time.Duration(input.TimeoutSeconds) * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case reply := <-request.channel:
		action := reply.Action
		status := "DECLINED"
		switch action {
		case "accept":
			status = "ANSWERED"
		case "cancel":
			status = "CANCELED"
		}
		resolved := map[string]any{"requestId": requestID, "action": action, "status": status}
		if len(reply.Content) > 0 {
			resolved["content"] = reply.Content
		}
		t.emit("acp_elicitation_resolved", map[string]any{
			"type": "acp_elicitation_resolved", "data": resolved,
		})
		if action == "cancel" {
			return provider.InputAnswer{}, errTurnCanceled
		}
		return provider.InputAnswer{
			Action: action, Content: reply.Content,
			Written: func(err error) {
				stage := "WRITTEN"
				if err != nil {
					stage = "UNKNOWN"
				}
				t.markAnswerStage(reply, stage)
			},
		}, nil
	case <-timer.C:
		t.emit("acp_elicitation_resolved", map[string]any{
			"type": "acp_elicitation_resolved",
			"data": map[string]any{"requestId": requestID, "action": "cancel", "status": "EXPIRED"},
		})
		return provider.InputAnswer{}, fmt.Errorf("conversation input timed out")
	case <-ctx.Done():
		return provider.InputAnswer{}, ctx.Err()
	}
}

func (t *turn) registerElicitation() (string, *elicitationRequest) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nextReq++
	requestID := "acp-" + strconv.FormatInt(t.frame.TurnID, 10) + "-" + strconv.Itoa(t.nextReq)
	request := &elicitationRequest{channel: make(chan protocol.ConversationElicitation, 1)}
	t.pending[requestID] = request
	return requestID, request
}

func (t *turn) unregisterElicitation(requestID string) {
	t.mu.Lock()
	delete(t.pending, requestID)
	t.mu.Unlock()
}

// resolveElicitation routes a durable answer and acknowledges the stage the
// server's delivery state machine understands:
// RECEIVED -> WRITTEN, or UNAVAILABLE when no native gate is waiting.
func (t *turn) resolveElicitation(frame protocol.ConversationElicitation) {
	t.mu.Lock()
	request := t.pending[frame.RequestID]
	t.mu.Unlock()

	if request == nil {
		t.h.answerReceipts.Save(frame, "UNAVAILABLE")
		t.ackAnswer(frame, "UNAVAILABLE")
		return
	}
	t.ackAnswer(frame, "RECEIVED")
	select {
	case request.channel <- frame:
	case <-time.After(2 * time.Second):
		t.ackAnswer(frame, "UNAVAILABLE")
	}
}

func (t *turn) markAnswerStage(frame protocol.ConversationElicitation, stage string) {
	if frame.AnswerID == "" {
		return
	}
	ackStage := stage
	if err := t.h.answerReceipts.Save(frame, stage); err != nil {
		slog.Warn("persist conversation answer stage failed", "answerId", frame.AnswerID, "error", err)
		ackStage = "UNKNOWN"
	}
	t.ackAnswer(frame, ackStage)
}

func (t *turn) ackAnswer(frame protocol.ConversationElicitation, stage string) {
	t.h.ackAnswerFrame(frame, stage)
}

// pendingCount is used by tests and diagnostics.
func (t *turn) pendingCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pending)
}
