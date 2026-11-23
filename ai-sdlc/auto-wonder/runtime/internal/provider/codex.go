package provider

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Codex drives `codex app-server --listen stdio://` over JSON-RPC.
func buildCodexArgs(unattended bool, reasoningEffort, contextWindow string) []string {
	args := []string{}
	if unattended {
		args = append(args, "--dangerously-bypass-hook-trust")
	}
	if reasoningEffort != "" {
		args = append(args, "-c", "model_reasoning_effort="+reasoningEffort)
	}
	if contextWindow != "" {
		args = append(args, "-c", "model_context_window="+contextWindow)
	}
	return append(args, "app-server", "--listen", "stdio://")
}

type Codex struct {
	registry activeTurnRegistry
}

func (c *Codex) Name() string { return "codex" }

func (c *Codex) Steer(ctx context.Context, dispatchID int64, content string) error {
	return c.registry.steer(ctx, dispatchID, content)
}

type rpcMessage struct {
	ID     *int64          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

type codexClient struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex
	nextID  atomic.Int64
	pending sync.Map // int64 -> chan rpcMessage
	emit    func(Event)
	ctx     context.Context
	ask     func(context.Context, InputRequest) (InputAnswer, error)

	completed chan struct{}
	closeOnce sync.Once
	engineErr string
	errMu     sync.Mutex
	stderr    strings.Builder
	turnDone  bool
	summary   strings.Builder
	threadID  string
	turnID    string
}

func (c *Codex) Run(ctx context.Context, req Request, emit func(Event)) (Result, error) {
	return c.runOnce(ctx, req, req.Prompt, emit, func(error) {})
}

func (c *Codex) runOnce(ctx context.Context, req Request, prompt string, emit func(Event), onStart func(error)) (Result, error) {
	mcpArgs, mcpEnv, err := buildCodexMCPProjection(req.MCPConfigPath)
	if err != nil {
		onStart(err)
		return Result{}, fmt.Errorf("codex MCP config: %w", err)
	}
	args := append(buildCodexArgs(req.Unattended, req.ReasoningEffort, req.ContextWindow), mcpArgs...)

	cmd := exec.CommandContext(ctx, agentCommand(req.AgentPath, "codex"), args...)
	cmd.Dir = req.WorkDir
	env, err := MergeEnvironment(currentEnv(), mcpEnv)
	if err != nil {
		onStart(err)
		return Result{}, fmt.Errorf("codex MCP environment: %w", err)
	}
	env, err = MergeEnvironment(env, req.Env)
	if err != nil {
		onStart(err)
		return Result{}, fmt.Errorf("codex environment: %w", err)
	}
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		onStart(err)
		return Result{}, fmt.Errorf("codex stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		onStart(err)
		return Result{}, fmt.Errorf("codex stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		onStart(err)
		return Result{}, fmt.Errorf("codex stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		onStart(err)
		return Result{}, fmt.Errorf("start codex: %w", err)
	}

	client := &codexClient{cmd: cmd, stdin: stdin, emit: emit, completed: make(chan struct{}),
		ctx: ctx, ask: req.RequestInput}
	go client.readStdout(stdout)
	go client.drainStderr(stderr)

	if err := client.initialize(ctx); err != nil {
		client.stop()
		onStart(err)
		return Result{}, err
	}
	threadID, err := client.startOrResumeThread(ctx, req)
	if err != nil {
		client.stop()
		onStart(err)
		return Result{}, err
	}
	client.threadID = threadID
	turnID, err := client.startTurn(ctx, prompt)
	if err != nil {
		client.stop()
		onStart(err)
		return Result{}, err
	}
	client.turnID = turnID
	if req.DispatchID > 0 {
		unregister := c.registry.register(req.DispatchID, func(steerCtx context.Context, content string) error {
			return client.steerTurn(steerCtx, content)
		})
		defer unregister()
	}
	onStart(nil)

	select {
	case <-client.completed:
	case <-ctx.Done():
		_ = client.stop()
		return Result{Summary: truncate(client.summary.String(), 2000), FullText: client.summary.String(), SessionID: threadID},
			fmt.Errorf("codex timed out: %w", ctx.Err())
	}
	exitErr := client.stop()

	client.errMu.Lock()
	engineErr := client.engineErr
	turnDone := client.turnDone
	client.errMu.Unlock()
	if engineErr != "" {
		return Result{Summary: truncate(client.summary.String(), 2000), FullText: client.summary.String(), SessionID: threadID},
			fmt.Errorf("codex reported error: %s", truncate(engineErr, 500))
	}
	if !turnDone && ctx.Err() == nil {
		message := "codex app-server exited before turn completion"
		if exitErr != nil {
			message += ": " + exitErr.Error()
		}
		if stderr := client.stderrText(); stderr != "" {
			message += ": " + truncate(stderr, 500)
		}
		return Result{Summary: truncate(client.summary.String(), 2000), FullText: client.summary.String(), SessionID: threadID},
			fmt.Errorf("%s", message)
	}
	return Result{Summary: truncate(client.summary.String(), 2000), FullText: client.summary.String(), SessionID: threadID}, nil
}

func (c *codexClient) stderrText() string {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return strings.TrimSpace(c.stderr.String())
}

// buildCodexMCPProjection projects mcpServers into `-c` TOML overrides. Bearer
// credentials are passed through named environment variables so they never
// appear in the child process argv.
func buildCodexMCPProjection(path string) ([]string, map[string]string, error) {
	if path == "" {
		return nil, nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var source map[string]any
	if json.Unmarshal(raw, &source) != nil {
		return nil, nil, fmt.Errorf("parse MCP config")
	}
	servers, _ := source["mcpServers"].(map[string]any)
	var args []string
	env := map[string]string{}
	for name, rawEntry := range servers {
		entry, _ := rawEntry.(map[string]any)
		if entry == nil {
			continue
		}
		key := "mcp_servers." + tomlKey(name)
		if url, ok := entry["url"].(string); ok && url != "" {
			args = append(args, "-c", key+".url="+tomlString(url))
			if headers, ok := entry["headers"].(map[string]any); ok {
				for header, value := range headers {
					if text, ok := value.(string); ok {
						if strings.EqualFold(header, "Authorization") && strings.HasPrefix(strings.ToLower(text), "bearer ") {
							envName := codexMCPBearerEnvName(name)
							env[envName] = strings.TrimSpace(text[len("Bearer "):])
							args = append(args, "-c", key+".bearer_token_env_var="+tomlString(envName))
							continue
						}
						args = append(args, "-c", key+".http_headers."+tomlKey(header)+"="+tomlString(text))
					}
				}
			}
			continue
		}
		if command, ok := entry["command"].(string); ok && command != "" {
			args = append(args, "-c", key+".command="+tomlString(command))
			if values, ok := entry["args"].([]any); ok {
				parts := make([]string, 0, len(values))
				for _, value := range values {
					if text, ok := value.(string); ok {
						parts = append(parts, tomlString(text))
					}
				}
				args = append(args, "-c", key+".args=["+strings.Join(parts, ",")+"]")
			}
		}
	}
	return args, env, nil
}

func codexMCPBearerEnvName(server string) string {
	var normalized strings.Builder
	for _, char := range strings.ToUpper(server) {
		if char == '_' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' {
			normalized.WriteRune(char)
		} else {
			normalized.WriteByte('_')
		}
	}
	name := strings.Trim(normalized.String(), "_")
	if name == "" {
		name = "SERVER"
	}
	return "AUTOWONDER_MCP_TOKEN_" + name
}

func tomlString(value string) string {
	return strconv.Quote(value)
}

func tomlKey(value string) string {
	for _, r := range value {
		if !(r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return strconv.Quote(value)
		}
	}
	return value
}

func (c *codexClient) readStdout(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		c.emit(Event{Kind: "stdout", Text: truncate(line, 2000)})
		var message rpcMessage
		if json.Unmarshal([]byte(line), &message) != nil {
			continue
		}
		switch {
		case message.ID != nil && message.Method != "":
			c.handleServerRequest(*message.ID, message.Method, message.Params)
		case message.ID != nil:
			if channel, ok := c.pending.LoadAndDelete(*message.ID); ok {
				channel.(chan rpcMessage) <- message
			}
		case message.Method != "":
			c.handleNotification(message.Method, message.Params)
		}
	}
	c.closeOnce.Do(func() { close(c.completed) })
}

func (c *codexClient) drainStderr(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 32*1024), 512*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		c.emit(Event{Kind: "stderr", Text: truncate(line, 2000)})
		c.errMu.Lock()
		if c.stderr.Len() < 64*1024 {
			c.stderr.WriteString(line + "\n")
		}
		c.errMu.Unlock()
	}
}

func (c *codexClient) handleNotification(method string, params json.RawMessage) {
	lower := strings.ToLower(method)
	if strings.Contains(lower, "turn/completed") || strings.Contains(lower, "turn.completed") {
		c.errMu.Lock()
		c.turnDone = true
		c.errMu.Unlock()
		c.closeOnce.Do(func() { close(c.completed) })
		return
	}
	if strings.Contains(lower, "error") {
		c.errMu.Lock()
		c.engineErr = truncate(string(params), 500)
		c.errMu.Unlock()
		return
	}
	var payload map[string]any
	if json.Unmarshal(params, &payload) != nil {
		return
	}
	text := firstTextField(payload["text"], lookup(payload, "item", "text"), lookup(payload, "delta", "text"), lookup(payload, "message", "content"))
	if text != "" {
		c.summary.WriteString(text)
		c.emit(Event{Kind: "text", Text: truncate(text, 200), FullText: text})
	}
}

func (c *codexClient) handleServerRequest(id int64, method string, params json.RawMessage) {
	lower := strings.ToLower(method)
	var result any
	var answered *InputAnswer
	switch {
	case strings.Contains(lower, "requestapproval"):
		answer, ok := c.askHuman("APPROVAL", method, params, "Agent requests approval to continue", nil)
		if ok && (answer.Action == "approve" || answer.Action == "accept") {
			result = map[string]any{"decision": "approved"}
			answered = &answer
		} else {
			result = map[string]any{"decision": "denied"}
			if ok {
				answered = &answer
			}
		}
	case strings.Contains(lower, "elicitation"):
		answer, ok := c.askHuman("INPUT", method, params, "Agent requests additional input", nil)
		if ok && answer.Action == "decline" {
			result = map[string]any{"action": "decline"}
		} else {
			content := map[string]any{}
			if ok && answer.Content != nil {
				content = answer.Content
			}
			result = map[string]any{"action": "accept", "content": content}
		}
		if ok {
			answered = &answer
		}
	case strings.Contains(lower, "requestuserinput"):
		answer, ok := c.askHuman("INPUT", method, params, "Agent requests user input", nil)
		if ok && answer.Content != nil {
			if answers, exists := answer.Content["answers"]; exists {
				result = map[string]any{"answers": answers}
			} else {
				result = map[string]any{"answers": []any{answer.Content}}
			}
		} else {
			result = map[string]any{"answers": []any{}}
		}
		if ok {
			answered = &answer
		}
	default:
		result = map[string]any{}
	}
	c.emit(Event{Kind: "tool", Text: "codex server request: " + method})
	writeErr := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	if answered != nil {
		answered.ReportWritten(writeErr)
	}
	if writeErr != nil {
		c.errMu.Lock()
		c.engineErr = "codex response write failed: " + writeErr.Error()
		c.errMu.Unlock()
		c.emit(Event{Kind: "error", Text: "codex response write failed: " + writeErr.Error()})
	}
}

func (c *codexClient) askHuman(kind, method string, params json.RawMessage, fallbackMessage string,
	schema map[string]any) (InputAnswer, bool) {
	if c.ask == nil {
		return InputAnswer{}, false
	}
	var payload map[string]any
	_ = json.Unmarshal(params, &payload)
	message := firstTextField(payload["message"], payload["prompt"], payload["question"])
	if message == "" {
		message = fallbackMessage
	}
	digest := sha256.Sum256(append([]byte(method+"\n"), params...))
	key := "codex-" + hex.EncodeToString(digest[:8])
	if kind == "APPROVAL" {
		schema = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"decision": map[string]any{"type": "string", "enum": []string{"approve", "reject"}},
			},
			"required": []string{"decision"},
		}
	} else if schema == nil {
		schema = map[string]any{"type": "object", "additionalProperties": true}
	}
	context := map[string]any{"fingerprint": hex.EncodeToString(digest[:]), "method": method}
	answer, err := c.ask(c.ctx, InputRequest{
		Kind: kind, Key: key, Method: method, Message: truncate(message, 1024),
		Schema: schema, Context: context, TimeoutSeconds: 1800,
	})
	if err != nil {
		c.emit(Event{Kind: "error", Text: "human gate failed: " + truncate(err.Error(), 300)})
		return InputAnswer{}, false
	}
	return answer, true
}

func (c *codexClient) initialize(ctx context.Context) error {
	_, err := c.call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "autowonder-runtime", "version": "0.1.0"},
	})
	return err
}

func (c *codexClient) startOrResumeThread(ctx context.Context, req Request) (string, error) {
	if req.SessionID != "" {
		result, err := c.call(ctx, "thread/resume", map[string]any{"threadId": req.SessionID})
		if err == nil {
			if id := threadIDFrom(result); id != "" {
				c.emit(Event{Kind: "session", SessionID: id})
				return id, nil
			}
		}
	}
	approvalPolicy := req.ApprovalPolicy
	if approvalPolicy == "" {
		approvalPolicy = "never"
	}
	params := map[string]any{"cwd": req.WorkDir, "approvalPolicy": approvalPolicy}
	if req.Model != "" {
		params["model"] = req.Model
	}
	result, err := c.call(ctx, "thread/start", params)
	if err != nil {
		return "", err
	}
	id := threadIDFrom(result)
	if id == "" {
		return "", fmt.Errorf("codex thread/start returned no thread ID")
	}
	c.emit(Event{Kind: "session", SessionID: id})
	return id, nil
}

func (c *codexClient) startTurn(ctx context.Context, prompt string) (string, error) {
	result, err := c.call(ctx, "turn/start", map[string]any{
		"threadId": c.threadID,
		"input":    []any{map[string]any{"type": "text", "text": prompt}},
	})
	if err != nil {
		return "", err
	}
	return turnIDFrom(result), nil
}

func (c *codexClient) steerTurn(ctx context.Context, content string) error {
	if c.threadID == "" || c.turnID == "" {
		return fmt.Errorf("codex turn is not active")
	}
	result, err := c.call(ctx, "turn/steer", map[string]any{
		"threadId":       c.threadID,
		"expectedTurnId": c.turnID,
		"input":          []any{map[string]any{"type": "text", "text": content}},
	})
	if err != nil {
		return err
	}
	if id := turnIDFrom(result); id != "" {
		c.turnID = id
	}
	return nil
}

func (c *codexClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	channel := make(chan rpcMessage, 1)
	c.pending.Store(id, channel)
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.pending.Delete(id)
		return nil, err
	}
	select {
	case response := <-channel:
		if len(response.Error) > 0 && string(response.Error) != "null" {
			return nil, fmt.Errorf("codex %s: %s", method, truncate(string(response.Error), 500))
		}
		return response.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(120 * time.Second):
		return nil, fmt.Errorf("codex %s timed out", method)
	case <-c.completed:
		return nil, fmt.Errorf("codex process exited during %s", method)
	}
}

func (c *codexClient) write(value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(append(payload, '\n'))
	return err
}

func (c *codexClient) stop() error {
	_ = c.stdin.Close()
	done := make(chan error, 1)
	go func() {
		done <- c.cmd.Wait()
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
	}
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Signal(os.Interrupt)
	}
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
	}
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	return <-done
}

func threadIDFrom(result json.RawMessage) string {
	var payload map[string]any
	if json.Unmarshal(result, &payload) != nil {
		return ""
	}
	for _, candidate := range []any{
		payload["threadId"], payload["threadID"],
		lookup(payload, "thread", "id"), lookup(payload, "thread", "threadId"),
	} {
		if text, ok := candidate.(string); ok && text != "" {
			return text
		}
	}
	return ""
}

func turnIDFrom(result json.RawMessage) string {
	var payload map[string]any
	if json.Unmarshal(result, &payload) != nil {
		return ""
	}
	if id, ok := payload["turnId"].(string); ok && id != "" {
		return id
	}
	if turn, ok := payload["turn"].(map[string]any); ok {
		if id, ok := turn["id"].(string); ok && id != "" {
			return id
		}
	}
	return ""
}
