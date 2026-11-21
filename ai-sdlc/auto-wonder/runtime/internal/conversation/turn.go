package conversation

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/capsule"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/mcpconfig"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/prompt"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/protocol"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/provider"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/repos"
)

type turn struct {
	h      *Handler
	ctx    context.Context
	cancel context.CancelFunc
	frame  protocol.ConversationTurn

	mu           sync.Mutex
	emitMu       sync.Mutex
	seq          int64
	text         strings.Builder
	sessionID    string
	canceled     bool
	abandoned    bool
	finished     bool
	finalText    string
	usage        *protocol.ConversationTurnUsage
	pending      map[string]*elicitationRequest
	answerStages map[string]string
	nextReq      int
}

func newTurn(h *Handler, ctx context.Context, cancel context.CancelFunc,
	frame protocol.ConversationTurn) *turn {
	return &turn{h: h, ctx: ctx, cancel: cancel, frame: frame,
		pending: map[string]*elicitationRequest{}, answerStages: map[string]string{}}
}

func (t *turn) markAbandoned() {
	t.mu.Lock()
	t.abandoned = true
	t.mu.Unlock()
}

func (t *turn) markCanceled() {
	t.mu.Lock()
	t.canceled = true
	t.mu.Unlock()
}

func (t *turn) emit(eventType string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("conversation event payload encode failed", "type", eventType, "error", err)
		return
	}
	t.emitMu.Lock()
	defer t.emitMu.Unlock()
	t.mu.Lock()
	t.seq++
	seq := t.seq
	t.mu.Unlock()
	frame := protocol.ConversationTurnEvent{
		Type:                                "CONVERSATION_TURN_EVENT",
		ExecutorID:                          t.frame.ExecutorID,
		ConversationID:                      t.frame.ConversationID,
		TurnID:                              t.frame.TurnID,
		DispatchAttempt:                     t.frame.DispatchAttempt,
		EventSeq:                            seq,
		ChunkIndex:                          0,
		ChunkCount:                          1,
		EventType:                           eventType,
		PayloadFragment:                     string(raw),
		ConversationDeliveryProtocolVersion: t.frame.ConversationDeliveryProtocolVersion,
	}
	key := deliveryKey{conversationID: t.frame.ConversationID, turnID: t.frame.TurnID,
		attempt: t.frame.DispatchAttempt, seq: seq, chunk: 0}
	t.h.deliver(frame, key, t.frame.ConversationDeliveryProtocolVersion == 1)
}

func (t *turn) emitStatus(status, sessionID string) {
	payload := map[string]any{"type": "status", "status": status}
	if sessionID != "" {
		payload["sessionId"] = sessionID
	}
	t.emit("status", payload)
}

func (t *turn) appendText(text string) {
	if text == "" {
		return
	}
	t.mu.Lock()
	t.text.WriteString(text)
	t.mu.Unlock()
	t.emit("text", map[string]any{"type": "text", "content": text})
}

func (t *turn) setSession(sessionID string) {
	t.mu.Lock()
	if sessionID != "" {
		t.sessionID = sessionID
	}
	t.mu.Unlock()
	if sessionID != "" {
		t.emitStatus("RUNNING", sessionID)
	}
}

func (t *turn) finish(status, errText, sessionID string) {
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return
	}
	t.finished = true
	partial := t.text.String()
	if t.finalText != "" {
		partial = t.finalText
	}
	if sessionID == "" {
		sessionID = t.sessionID
	}
	canceled := t.canceled
	abandoned := t.abandoned
	usage := t.usage
	t.mu.Unlock()

	if abandoned {
		return
	}
	if canceled {
		status = "CANCELED"
	}
	if status == "SUCCESS" && strings.TrimSpace(partial) == "" {
		status = "FAILED"
		errText = "agent returned an empty reply"
	}
	ack := protocol.ConversationTurnACK{
		Type:                                "CONVERSATION_TURN_ACK",
		ExecutorID:                          t.frame.ExecutorID,
		ConversationID:                      t.frame.ConversationID,
		TurnID:                              t.frame.TurnID,
		DispatchAttempt:                     t.frame.DispatchAttempt,
		Status:                              status,
		Error:                               truncate(errText, 1000),
		ReplyMarkdown:                       partial,
		SessionID:                           sessionID,
		Usage:                               usage,
		ConversationDeliveryProtocolVersion: t.frame.ConversationDeliveryProtocolVersion,
	}
	key := deliveryKey{conversationID: t.frame.ConversationID, turnID: t.frame.TurnID,
		attempt: t.frame.DispatchAttempt, seq: 0, chunk: 0}
	t.h.deliver(ack, key, t.frame.ConversationDeliveryProtocolVersion == 1)
	slog.Info("conversation turn finished", "conversationId", t.frame.ConversationID,
		"turnId", t.frame.TurnID, "status", status)
}

func (t *turn) run() (status, errText, sessionID string) {
	frame := t.frame
	conversationRoot := filepath.Join(t.h.cfg.WorkspaceRoot, "conversations",
		strconv.FormatInt(frame.ConversationID, 10))
	turnRoot := filepath.Join(conversationRoot, "turn-"+strconv.FormatInt(frame.TurnID, 10))
	packageDir := filepath.Join(turnRoot, "package")
	workspaceDir := filepath.Join(conversationRoot, "workspace")
	artifactsDir := filepath.Join(turnRoot, "artifacts")
	for _, dir := range []string{packageDir, workspaceDir, artifactsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "FAILED", "create conversation workspace: " + err.Error(), ""
		}
	}

	if frame.CapabilityDownloadURL != "" {
		if err := capsule.DownloadAndExtract(t.ctx, frame.CapabilityDownloadURL, frame.CapabilitySHA256,
			filepath.Join(turnRoot, "capability.zip"), packageDir); err != nil {
			return "FAILED", "conversation capability: " + err.Error(), ""
		}
	}

	specs, err := repos.Load(packageDir)
	if err != nil {
		return "FAILED", "repositories: " + err.Error(), ""
	}
	eagerSpecs, lazySpecs := repos.SplitByMode(specs)
	// A lazy repository cloned by an earlier turn is already part of this
	// conversation's workspace; keep treating it as checked out.
	lazySpecs, previouslyCloned := splitExistingCheckouts(workspaceDir, lazySpecs)
	eagerSpecs = append(eagerSpecs, previouslyCloned...)
	checkouts, err := prepareRepositories(t.ctx, workspaceDir, eagerSpecs)
	if err != nil {
		return "FAILED", "repositories: " + err.Error(), ""
	}

	mcpPath, err := mcpconfig.Write(packageDir, t.h.cfg.BaseURL(), frame.MCPToken, frame.MCPSecrets,
		filepath.Join(turnRoot, "mcp.config.json"))
	if err != nil {
		return "FAILED", "mcp config: " + err.Error(), ""
	}

	content := frame.Content
	if note := downloadAttachments(t.ctx, frame.Content, workspaceDir, frame.TurnID); note != "" {
		content = note + "\n\n" + content
	}
	agentPrompt := prompt.BuildConversation(frame.SystemPrompt, content, packageDir,
		workspaceDir, checkouts, lazySpecs)
	env := copyEnvironment(frame.EnvironmentVariables)
	if t.h.cfg.ReasoningEffort != "" {
		env["AUTOWONDER_REASONING_EFFORT"] = t.h.cfg.ReasoningEffort
	}
	if t.h.cfg.ContextWindow != "" {
		env["AUTOWONDER_CONTEXT_WINDOW"] = t.h.cfg.ContextWindow
	}
	env["AUTOWONDER_CONVERSATION_ID"] = strconv.FormatInt(frame.ConversationID, 10)
	env["AUTOWONDER_CONVERSATION_TURN_ID"] = strconv.FormatInt(frame.TurnID, 10)

	request := provider.Request{
		AgentPath:       t.h.cfg.AgentPath,
		Prompt:          agentPrompt,
		WorkDir:         workspaceDir,
		ArtifactsDir:    artifactsDir,
		Model:           t.h.cfg.AgentModel,
		ReasoningEffort: t.h.cfg.ReasoningEffort,
		ContextWindow:   t.h.cfg.ContextWindow,
		SessionID:       frame.CLISessionRef,
		MCPConfigPath:   mcpPath,
		Env:             env,
		Unattended:      t.h.cfg.Unattended,
		PiProvider:      t.h.cfg.PiProvider,
		PermissionFlag:  t.h.cfg.OpenCodePermissionFlag,
		PromptFlag:      t.h.cfg.OpenCodePromptFlag,
		UseACP:          t.h.cfg.OpenCodeACP,
		Interactive:     true,
	}
	if t.h.SupportsACP() {
		request.RequestInput = t.requestInput
		request.ApprovalPolicy = "on-request"
	}

	var engineErr string
	result, runErr := t.h.provider.Run(t.ctx, request, func(event provider.Event) {
		switch event.Kind {
		case "session":
			t.setSession(event.SessionID)
		case "text":
			text := event.FullText
			if text == "" {
				text = event.Text
			}
			t.appendText(text)
		case "thinking":
			text := event.FullText
			if text == "" {
				text = event.Text
			}
			t.emit("thinking", map[string]any{"type": "thinking", "content": text})
		case "tool":
			// Legacy CLI providers expose only a human-readable progress line.
			t.emit("log", map[string]any{"type": "log", "content": "tool: " + event.Text})
		case "tool_start":
			t.emit("tool_use", toolUsePayload(event))
		case "tool_update":
			if terminalToolStatus(event.ToolStatus) {
				t.emit("tool_result", toolResultPayload(event))
			} else {
				t.emit("tool_use", toolUsePayload(event))
			}
		case "plan":
			payload := map[string]any{"type": "acp_plan"}
			if len(event.Plan) > 0 {
				payload["data"] = map[string]any{"entries": event.Plan}
			}
			t.emit("acp_plan", payload)
		case "error":
			engineErr = event.Text
			t.emit("error", map[string]any{"type": "error", "content": event.Text})
		}
	})
	sessionID = result.SessionID
	if result.Usage != nil {
		t.mu.Lock()
		t.usage = &protocol.ConversationTurnUsage{
			Provider: result.Usage.Provider, Model: result.Usage.Model,
			InputTokens: result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens,
			CacheReadTokens: result.Usage.CacheReadTokens, CacheWriteTokens: result.Usage.CacheWriteTokens,
			ReasoningTokens: result.Usage.ReasoningTokens, Credits: result.Usage.Credits,
		}
		t.mu.Unlock()
	}
	if result.FullText != "" {
		t.mu.Lock()
		t.finalText = result.FullText
		t.mu.Unlock()
	}
	if runErr != nil {
		if t.ctx.Err() != nil {
			return "CANCELED", "", sessionID
		}
		return "FAILED", runErr.Error(), sessionID
	}
	if engineErr != "" {
		return "FAILED", engineErr, sessionID
	}
	return "SUCCESS", "", sessionID
}

func splitExistingCheckouts(workspaceDir string, specs []repos.Spec) (remaining, existing []repos.Spec) {
	for _, spec := range specs {
		dir := filepath.Join(workspaceDir, "repos", spec.Path)
		if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil && info.IsDir() {
			existing = append(existing, spec)
			continue
		}
		remaining = append(remaining, spec)
	}
	return remaining, existing
}

// prepareRepositories reuses an existing checkout instead of resetting it.
// A conversation is multi-turn; re-running `git checkout` on every turn would
// either fail on the agent's uncommitted work or silently discard it.
func prepareRepositories(ctx context.Context, workspaceDir string, specs []repos.Spec) ([]repos.Checkout, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	var missing []repos.Spec
	checkouts := make([]repos.Checkout, 0, len(specs))
	for _, spec := range specs {
		dir := filepath.Join(workspaceDir, "repos", spec.Path)
		if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil && info.IsDir() {
			head, headErr := repos.Head(ctx, dir)
			if headErr != nil {
				return nil, headErr
			}
			checkouts = append(checkouts, repos.Checkout{Spec: spec, Dir: dir, BaseCommit: head})
			continue
		}
		missing = append(missing, spec)
	}
	if len(missing) > 0 {
		created, err := repos.Materialize(ctx, workspaceDir, missing)
		if err != nil {
			return nil, err
		}
		checkouts = append(checkouts, created...)
	}
	return checkouts, nil
}

func copyEnvironment(source map[string]string) map[string]string {
	result := make(map[string]string, len(source)+4)
	for key, value := range source {
		result[key] = value
	}
	return result
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func terminalToolStatus(status string) bool {
	switch status {
	case "completed", "failed", "error", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func toolUsePayload(event provider.Event) map[string]any {
	payload := map[string]any{"type": "tool_use", "callId": event.ToolCallID}
	if event.ToolName != "" {
		payload["tool"] = event.ToolName
	}
	if len(event.ToolInput) > 0 {
		payload["input"] = event.ToolInput
	}
	data := map[string]any{}
	if event.ToolKind != "" {
		data["kind"] = event.ToolKind
	}
	if event.ToolTitle != "" {
		data["title"] = event.ToolTitle
	}
	if len(event.ToolLocations) > 0 {
		data["locations"] = event.ToolLocations
	}
	if len(data) > 0 {
		payload["data"] = data
	}
	return payload
}

func toolResultPayload(event provider.Event) map[string]any {
	payload := map[string]any{"type": "tool_result", "callId": event.ToolCallID,
		"status": normalizedToolStatus(event.ToolStatus)}
	if event.ToolName != "" {
		payload["tool"] = event.ToolName
	}
	if len(event.ToolOutput) > 0 {
		payload["output"] = event.ToolOutput
	}
	data := map[string]any{}
	if event.ToolKind != "" {
		data["kind"] = event.ToolKind
	}
	if event.ToolTitle != "" {
		data["title"] = event.ToolTitle
	}
	if len(event.ToolLocations) > 0 {
		data["locations"] = event.ToolLocations
	}
	if len(data) > 0 {
		payload["data"] = data
	}
	return payload
}

func normalizedToolStatus(status string) string {
	switch status {
	case "failed", "error":
		return "failed"
	case "cancelled", "canceled":
		return "cancelled"
	default:
		return "completed"
	}
}
