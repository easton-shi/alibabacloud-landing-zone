package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// Claude preserves the original Claude Code headless behaviour so the runtime
// stays drop-in compatible with the community executor.
type Claude struct {
	registry activeTurnRegistry
}

func (c *Claude) Name() string { return "claude" }

func (c *Claude) Steer(ctx context.Context, dispatchID int64, content string) error {
	return c.registry.steer(ctx, dispatchID, content)
}

func (c *Claude) Run(ctx context.Context, req Request, emit func(Event)) (Result, error) {
	return runSteerable(ctx, req, emit, &c.registry, c.runOnce)
}

func (c *Claude) runOnce(ctx context.Context, req Request, prompt string, emit func(Event), onStart func(error)) (Result, error) {
	req.Prompt = prompt
	args := buildClaudeArgs(req)

	var summary strings.Builder
	var sessionID string
	var resultText string
	var resultErr string
	isError := false
	var startOnce sync.Once
	start := func(err error) {
		startOnce.Do(func() { onStart(err) })
	}
	env, err := MergeEnvironment(currentEnv(), req.Env)
	if err != nil {
		start(err)
		return Result{}, fmt.Errorf("claude environment: %w", err)
	}
	stream := streamCommandWithStart(ctx, agentCommand(req.AgentPath, "claude"), args, req.WorkDir, env, func(line string) {
		emit(Event{Kind: "stdout", Text: truncate(line, 2000)})
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "{") {
			return
		}
		var event map[string]any
		if json.Unmarshal([]byte(trimmed), &event) != nil {
			return
		}
		if id, ok := event["session_id"].(string); ok && sessionID == "" {
			sessionID = id
			emit(Event{Kind: "session", SessionID: id})
			start(nil)
		}
		if event["type"] == "assistant" {
			if text := claudeAssistantText(event); text != "" {
				summary.WriteString(text)
				emit(Event{Kind: "text", Text: truncate(text, 200), FullText: text})
			}
		}
		if event["type"] == "result" {
			if text, ok := event["result"].(string); ok && resultText == "" {
				resultText = text
			}
			if flag, ok := event["is_error"].(bool); ok && flag {
				isError = true
			}
			if errors, ok := event["errors"].([]any); ok {
				parts := make([]string, 0, len(errors))
				for _, item := range errors {
					if text, ok := item.(string); ok {
						parts = append(parts, text)
					}
				}
				resultErr = truncate(strings.Join(parts, "; "), 500)
			}
		}
	}, func(line string) {
		emit(Event{Kind: "stderr", Text: truncate(line, 2000)})
	}, func(err error) {
		if err != nil {
			start(err)
			return
		}
		if req.SessionID != "" {
			start(nil)
		}
	})
	if stream.err != nil && stream.exitCode != 0 {
		message := stream.stderr
		if message == "" {
			message = stream.err.Error()
		}
		return Result{Summary: truncate(summary.String(), 2000), FullText: summary.String(), SessionID: sessionID},
			fmt.Errorf("claude exited %d: %s", stream.exitCode, truncate(message, 500))
	}
	if isError {
		return Result{Summary: truncate(resultText, 2000), FullText: resultText, SessionID: sessionID},
			fmt.Errorf("claude reported error: %s", truncate(resultErr, 500))
	}
	final := resultText
	if final == "" {
		final = summary.String()
	}
	return Result{Summary: truncate(final, 2000), FullText: final, SessionID: sessionID}, nil
}

func buildClaudeArgs(req Request) []string {
	args := []string{"-p", req.Prompt}
	if req.MCPConfigPath != "" {
		args = append(args, "--mcp-config", req.MCPConfigPath)
	}
	if req.ClaudeSettingsPath != "" {
		args = append(args, "--settings", req.ClaudeSettingsPath)
	}
	args = append(args,
		"--allowedTools", "mcp__autowonder__* Read Write Edit Bash LS Grep Glob",
		"--output-format", "stream-json",
		"--verbose",
	)
	if req.Unattended {
		args = append(args, "--permission-mode", "bypassPermissions")
	}
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	return args
}

func claudeAssistantText(event map[string]any) string {
	message, _ := event["message"].(map[string]any)
	content, _ := message["content"].([]any)
	for _, raw := range content {
		part, _ := raw.(map[string]any)
		if part["type"] == "text" {
			if text, ok := part["text"].(string); ok {
				return text
			}
		}
	}
	return ""
}
