package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// OpenCode ACP support. `opencode acp` is a JSON-RPC 2.0 server that streams
// agent/tool updates and raises session/request_permission for native human
// approval. It is used only when Request.UseACP is set.

type acpMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *acpRPCError    `json:"error,omitempty"`
}

type acpRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *acpRPCError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("ACP error %d: %s", e.Code, e.Message)
}

type acpClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	writeMu sync.Mutex
	pending map[string]chan acpMessage
	nextID  atomic.Int64

	parent      context.Context
	emit        func(Event)
	ask         func(context.Context, InputRequest) (InputAnswer, error)
	interactive bool

	mu        sync.Mutex
	sessionID string
	summary   strings.Builder
	commands  []map[string]any
	usage     *Usage
	replaying atomic.Bool
}

func (o *OpenCode) runACP(ctx context.Context, req Request, emit func(Event), onStart func(error)) (Result, error) {
	var startOnce sync.Once
	start := func(err error) {
		startOnce.Do(func() { onStart(err) })
	}
	command := agentCommand(req.AgentPath, "opencode")
	cmd := exec.CommandContext(ctx, command, "acp")
	cmd.Dir = req.WorkDir
	env, err := MergeEnvironment(currentEnv(), req.Env)
	if err != nil {
		start(err)
		return Result{}, fmt.Errorf("opencode ACP environment: %w", err)
	}
	if content := openCodeConfigContent(req.MCPConfigPath); content != "" {
		env, err = MergeEnvironment(env, map[string]string{"OPENCODE_CONFIG_CONTENT": content})
		if err != nil {
			start(err)
			return Result{}, fmt.Errorf("opencode ACP MCP environment: %w", err)
		}
	}
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		start(err)
		return Result{}, fmt.Errorf("opencode acp stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		start(err)
		return Result{}, fmt.Errorf("opencode acp stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		start(err)
		return Result{}, fmt.Errorf("opencode acp stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		start(err)
		return Result{}, fmt.Errorf("start opencode acp: %w", err)
	}
	client := &acpClient{
		cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr,
		pending: map[string]chan acpMessage{}, parent: ctx, emit: emit, ask: req.RequestInput,
		interactive: req.Interactive,
	}
	go client.readLoop()
	go client.drainStderr()
	result, runErr := client.run(ctx, req, start)
	client.stop()
	o.mu.Lock()
	if o.commands == nil {
		o.commands = map[int64][]map[string]any{}
	}
	o.commands[req.ConversationID] = client.commandsCopy()
	o.mu.Unlock()
	return result, runErr
}

func (c *acpClient) run(ctx context.Context, req Request, onStart func(error)) (Result, error) {
	if _, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
		"clientInfo":         map[string]any{"name": "autowonder-runtime", "version": "0.1.0"},
	}); err != nil {
		onStart(err)
		return Result{}, fmt.Errorf("opencode ACP initialize: %w", err)
	}
	sessionID, err := c.openSession(ctx, req)
	if err != nil {
		onStart(err)
		return Result{}, err
	}
	c.mu.Lock()
	c.sessionID = sessionID
	c.mu.Unlock()
	onStart(nil)
	if req.Model != "" {
		_, _ = c.call(ctx, "session/set_config_option", map[string]any{
			"sessionId": sessionID, "configId": "model", "value": req.Model,
		})
	}
	if req.ReasoningEffort != "" {
		_, _ = c.call(ctx, "session/set_config_option", map[string]any{
			"sessionId": sessionID, "configId": "effort", "value": req.ReasoningEffort,
		})
	}
	raw, err := c.call(ctx, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": req.Prompt}},
	})
	if err != nil {
		if ctx.Err() != nil {
			c.notify("session/cancel", map[string]any{"sessionId": sessionID})
		}
		return Result{SessionID: sessionID, FullText: c.summaryText()}, err
	}
	var result struct {
		StopReason string `json:"stopReason"`
		Usage      struct {
			InputTokens      int64 `json:"inputTokens"`
			OutputTokens     int64 `json:"outputTokens"`
			ThoughtTokens    int64 `json:"thoughtTokens"`
			CachedReadTokens int64 `json:"cachedReadTokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(raw, &result)
	usage := &Usage{
		Provider:        "opencode",
		Model:           req.Model,
		InputTokens:     result.Usage.InputTokens,
		OutputTokens:    result.Usage.OutputTokens,
		ReasoningTokens: result.Usage.ThoughtTokens,
		CacheReadTokens: result.Usage.CachedReadTokens,
	}
	return Result{
		Summary:   truncate(c.summaryText(), 2000),
		FullText:  c.summaryText(),
		SessionID: sessionID,
		Usage:     usage,
	}, nil
}

func (c *acpClient) openSession(ctx context.Context, req Request) (string, error) {
	if req.SessionID != "" {
		// ACP replays the persisted conversation while session/load is in
		// flight. Those updates are history, not output from this turn.
		c.replaying.Store(true)
		raw, err := c.call(ctx, "session/load", map[string]any{
			"sessionId": req.SessionID, "cwd": req.WorkDir, "mcpServers": []any{},
		})
		c.replaying.Store(false)
		if err == nil {
			var result struct {
				SessionID string `json:"sessionId"`
			}
			_ = json.Unmarshal(raw, &result)
			if result.SessionID == "" {
				result.SessionID = req.SessionID
			}
			return result.SessionID, nil
		}
		slog.Debug("opencode session/load failed; creating a new session", "error", err)
	}
	raw, err := c.call(ctx, "session/new", map[string]any{
		"cwd": req.WorkDir, "mcpServers": []any{},
	})
	if err != nil {
		return "", fmt.Errorf("opencode ACP session/new: %w", err)
	}
	var result struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.SessionID == "" {
		return "", fmt.Errorf("opencode ACP session/new returned no sessionId")
	}
	return result.SessionID, nil
}

func (c *acpClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	key := strconv.FormatInt(id, 10)
	channel := make(chan acpMessage, 1)
	c.writeMu.Lock()
	if c.pending == nil {
		c.pending = map[string]chan acpMessage{}
	}
	c.pending[key] = channel
	c.writeMu.Unlock()
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.writeMu.Lock()
		delete(c.pending, key)
		c.writeMu.Unlock()
		return nil, err
	}
	select {
	case response := <-channel:
		if response.Error != nil {
			return nil, response.Error
		}
		return response.Result, nil
	case <-ctx.Done():
		c.writeMu.Lock()
		delete(c.pending, key)
		c.writeMu.Unlock()
		return nil, ctx.Err()
	case <-time.After(30 * time.Minute):
		c.writeMu.Lock()
		delete(c.pending, key)
		c.writeMu.Unlock()
		return nil, fmt.Errorf("opencode ACP %s timed out", method)
	}
}

func (c *acpClient) notify(method string, params any) {
	_ = c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *acpClient) write(value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(append(payload, '\n'))
	return err
}

func (c *acpClient) readLoop() {
	scanner := bufio.NewScanner(c.stdout)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var message acpMessage
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			continue
		}
		if len(message.ID) > 0 && message.Method != "" {
			go c.handleServerRequest(message)
			continue
		}
		if len(message.ID) > 0 {
			key := strings.Trim(string(message.ID), `"`)
			c.writeMu.Lock()
			channel := c.pending[key]
			delete(c.pending, key)
			c.writeMu.Unlock()
			if channel != nil {
				channel <- message
			}
			continue
		}
		if message.Method != "" {
			c.handleNotification(message)
		}
	}
}

func (c *acpClient) drainStderr() {
	scanner := bufio.NewScanner(c.stderr)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			slog.Debug("opencode acp stderr", "line", truncate(line, 1000))
		}
	}
}

func (c *acpClient) handleNotification(message acpMessage) {
	var params struct {
		Update json.RawMessage `json:"update"`
	}
	if json.Unmarshal(message.Params, &params) != nil || len(params.Update) == 0 {
		return
	}
	var update struct {
		SessionUpdate string           `json:"sessionUpdate"`
		Content       json.RawMessage  `json:"content"`
		MessageID     string           `json:"messageId"`
		ToolCallID    string           `json:"toolCallId"`
		Name          string           `json:"name"`
		Title         string           `json:"title"`
		Kind          string           `json:"kind"`
		Status        string           `json:"status"`
		RawInput      json.RawMessage  `json:"rawInput"`
		RawOutput     json.RawMessage  `json:"rawOutput"`
		Locations     json.RawMessage  `json:"locations"`
		Entries       json.RawMessage  `json:"entries"`
		Available     []map[string]any `json:"availableCommands"`
	}
	if json.Unmarshal(params.Update, &update) != nil {
		return
	}
	if c.replaying.Load() {
		// Keep command discovery from the loaded session, but do not emit the
		// replayed message, thought, tool, or plan updates as this turn's output.
		if update.SessionUpdate == "available_commands_update" {
			c.mu.Lock()
			c.commands = update.Available
			c.mu.Unlock()
		}
		return
	}
	switch update.SessionUpdate {
	case "agent_message_chunk":
		if text := acpText(update.Content); text != "" {
			c.mu.Lock()
			c.summary.WriteString(text)
			c.mu.Unlock()
			c.emit(Event{Kind: "text", Text: truncate(text, 200), FullText: text})
		}
	case "agent_thought_chunk":
		if text := acpText(update.Content); text != "" {
			c.emit(Event{Kind: "thinking", Text: truncate(text, 200), FullText: text})
		}
	case "tool_call":
		c.emit(Event{Kind: "tool_start", Text: truncate(update.Title, 200), ToolCallID: update.ToolCallID,
			ToolName: firstNonEmpty(update.Name, update.Title), ToolKind: update.Kind, ToolTitle: update.Title,
			ToolStatus: update.Status, ToolInput: update.RawInput, ToolLocations: update.Locations})
	case "tool_call_update":
		output := update.RawOutput
		if len(output) == 0 {
			output = update.Content
		}
		c.emit(Event{Kind: "tool_update", Text: truncate(update.Title, 200), ToolCallID: update.ToolCallID,
			ToolKind: update.Kind, ToolTitle: update.Title, ToolStatus: update.Status,
			ToolInput: update.RawInput, ToolOutput: output, ToolLocations: update.Locations})
	case "plan":
		c.emit(Event{Kind: "plan", Plan: update.Entries})
	case "available_commands_update":
		c.mu.Lock()
		c.commands = update.Available
		c.mu.Unlock()
	}
}

func (c *acpClient) handleServerRequest(message acpMessage) {
	switch message.Method {
	case "session/request_permission":
		c.handlePermission(message)
	default:
		c.respond(message.ID, nil, &acpRPCError{Code: -32601, Message: "method not supported by runtime: " + message.Method})
	}
}

type acpPermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

func (c *acpClient) handlePermission(message acpMessage) {
	var params struct {
		SessionID string `json:"sessionId"`
		ToolCall  struct {
			ToolCallID string          `json:"toolCallId"`
			Title      string          `json:"title"`
			Kind       string          `json:"kind"`
			Status     string          `json:"status"`
			RawInput   json.RawMessage `json:"rawInput"`
		} `json:"toolCall"`
		Options []acpPermissionOption `json:"options"`
	}
	if json.Unmarshal(message.Params, &params) != nil {
		c.respond(message.ID, map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil)
		return
	}
	if len(params.Options) == 0 {
		c.respond(message.ID, map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil)
		return
	}
	if !c.interactive || c.ask == nil {
		// Task dispatches are unattended. Choose an allow option when the agent
		// offers one instead of blocking on a human gate that nobody will answer.
		for _, option := range params.Options {
			kind := strings.ToLower(option.Kind)
			name := strings.ToLower(option.Name)
			if strings.Contains(kind, "allow") || strings.Contains(name, "allow") {
				c.respond(message.ID, map[string]any{"outcome": map[string]any{
					"outcome": "selected", "optionId": option.OptionID}}, nil)
				return
			}
		}
		c.respond(message.ID, map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil)
		return
	}
	ids := make([]string, 0, len(params.Options))
	names := make([]string, 0, len(params.Options))
	for _, option := range params.Options {
		ids = append(ids, option.OptionID)
		names = append(names, option.Name)
	}
	messageText := fmt.Sprintf("Allow tool %q?", firstNonEmpty(params.ToolCall.Title, params.ToolCall.ToolCallID))
	if len(names) > 0 {
		messageText += " Options: " + strings.Join(names, ", ")
	}
	answer, err := c.ask(c.parent, InputRequest{
		Kind: "APPROVAL", Key: "perm-" + params.ToolCall.ToolCallID,
		Message: truncate(messageText, 1024),
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"optionId": map[string]any{"type": "string", "enum": ids, "title": "Decision"},
			},
			"required": []string{"optionId"},
		},
		Context:        map[string]any{"options": params.Options, "tool": params.ToolCall},
		TimeoutSeconds: 1800,
	})
	selected := ""
	answered := err == nil
	if err == nil {
		if value, ok := answer.Content["optionId"].(string); ok {
			for _, id := range ids {
				if value == id {
					selected = id
					break
				}
			}
		}
		if selected == "" && answer.Action == "accept" {
			selected = ids[0]
		}
	}
	if selected == "" {
		writeErr := c.respond(message.ID, map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil)
		if answered {
			answer.ReportWritten(writeErr)
		}
		return
	}
	writeErr := c.respond(message.ID, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": selected}}, nil)
	if answered {
		answer.ReportWritten(writeErr)
	}
}

func (c *acpClient) respond(id json.RawMessage, result any, rpcErr *acpRPCError) error {
	response := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id)}
	if rpcErr != nil {
		response["error"] = rpcErr
	} else {
		response["result"] = result
	}
	return c.write(response)
}

func (c *acpClient) summaryText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.summary.String()
}

func (c *acpClient) commandsCopy() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.commands...)
}

func (c *acpClient) stop() {
	if c.sessionID != "" {
		c.notify("session/cancel", map[string]any{"sessionId": c.sessionID})
	}
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_, _ = c.cmd.Process.Wait()
}

func acpText(raw json.RawMessage) string {
	var content struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &content) == nil && content.Type == "text" {
		return content.Text
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return ""
}

// DetectOpenCodeACP reports whether the installed OpenCode exposes the ACP
// server. It is executed once at runtime startup so the heartbeat never
// advertises a capability the binary cannot provide.
func DetectOpenCodeACP(agentPath string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, agentCommand(agentPath, "opencode"), "acp", "--help")
	_, err := command.CombinedOutput()
	return err == nil
}
