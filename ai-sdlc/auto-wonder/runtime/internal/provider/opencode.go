package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// OpenCode runs `opencode run --format json`.
//
// The closed-source daemon passed `--dangerously-skip-permissions` and
// `--prompt`; public OpenCode 1.x/2.x use `--auto` and a positional prompt.
// Both shapes are supported through Request.PermissionFlag / PromptFlag.
type OpenCode struct {
	mu       sync.Mutex
	commands []map[string]any
}

func (o *OpenCode) Name() string { return "opencode" }

// Commands returns the most recent ACP slash-command catalog.
func (o *OpenCode) Commands() []map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]map[string]any(nil), o.commands...)
}

func (o *OpenCode) Run(ctx context.Context, req Request, emit func(Event)) (Result, error) {
	if req.UseACP {
		return o.runACP(ctx, req, emit)
	}
	args := buildOpenCodeArgs(req)

	env := envSlice(currentEnv(), req.Env)
	if content := openCodeConfigContent(req.MCPConfigPath); content != "" {
		env = envSlice(env, map[string]string{"OPENCODE_CONFIG_CONTENT": content})
	}

	var summary strings.Builder
	var sessionID string
	var engineErr string
	stream := streamCommand(ctx, agentCommand(req.AgentPath, "opencode"), args, req.WorkDir, env, func(line string) {
		emit(Event{Kind: "stdout", Text: truncate(line, 2000)})
		kind, text, session, _ := parseOpenCodeEvent(line)
		if session != "" && sessionID == "" {
			sessionID = session
			emit(Event{Kind: "session", SessionID: session})
		}
		switch kind {
		case "text":
			if text != "" {
				summary.WriteString(text)
				emit(Event{Kind: "text", Text: truncate(text, 200), FullText: text})
			}
		case "tool":
			emit(Event{Kind: "tool", Text: truncate(text, 200)})
		case "error":
			engineErr = text
			emit(Event{Kind: "error", Text: truncate(text, 300)})
		}
	}, func(line string) {
		emit(Event{Kind: "stderr", Text: truncate(line, 2000)})
	})
	if stream.err != nil && stream.exitCode != 0 {
		message := stream.stderr
		if message == "" {
			message = stream.err.Error()
		}
		return Result{Summary: truncate(summary.String(), 2000), FullText: summary.String(), SessionID: sessionID},
			fmt.Errorf("opencode exited %d: %s", stream.exitCode, truncate(message, 500))
	}
	if engineErr != "" {
		return Result{Summary: truncate(summary.String(), 2000), FullText: summary.String(), SessionID: sessionID},
			fmt.Errorf("opencode reported error: %s", truncate(engineErr, 500))
	}
	return Result{Summary: truncate(summary.String(), 2000), FullText: summary.String(), SessionID: sessionID}, nil
}

// openCodeConfigContent translates an MCP config file into OpenCode's
// OPENCODE_CONFIG_CONTENT shape.
func buildOpenCodeArgs(req Request) []string {
	args := []string{"run", "--format", "json"}
	if req.Unattended && req.PermissionFlag != "" {
		args = append(args, req.PermissionFlag)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.SessionID != "" {
		args = append(args, "--session", req.SessionID)
	}
	if req.PromptFlag != "" {
		args = append(args, req.PromptFlag, req.Prompt)
	} else {
		args = append(args, req.Prompt)
	}
	return args
}

func openCodeConfigContent(path string) string {
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var source map[string]any
	if json.Unmarshal(raw, &source) != nil {
		return ""
	}
	servers, _ := source["mcpServers"].(map[string]any)
	if servers == nil {
		return ""
	}
	mcp := map[string]any{}
	for name, rawEntry := range servers {
		entry, _ := rawEntry.(map[string]any)
		if entry == nil {
			continue
		}
		translated := map[string]any{"enabled": true}
		if _, ok := entry["url"]; ok {
			translated["type"] = "remote"
			translated["url"] = entry["url"]
			if headers, ok := entry["headers"]; ok {
				translated["headers"] = headers
			}
		} else if command, ok := entry["command"]; ok {
			translated["type"] = "local"
			if args, ok := entry["args"].([]any); ok {
				parts := []any{command}
				parts = append(parts, args...)
				translated["command"] = parts
			} else {
				translated["command"] = []any{command}
			}
			if environment, ok := entry["env"]; ok {
				translated["environment"] = environment
			}
		} else {
			continue
		}
		mcp[name] = translated
	}
	if len(mcp) == 0 {
		return ""
	}
	encoded, err := json.Marshal(map[string]any{"mcp": mcp})
	if err != nil {
		return ""
	}
	return string(encoded)
}

func parseOpenCodeEvent(line string) (kind, text, session, errText string) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || !strings.HasPrefix(trimmed, "{") {
		return "", "", "", ""
	}
	var event map[string]any
	if json.Unmarshal([]byte(trimmed), &event) != nil {
		return "", "", "", ""
	}
	kind = stringField(event, "type")
	session = findSessionID(event)
	text = firstTextField(event["text"], lookup(event, "part", "text"), lookup(event, "properties", "part", "text"))
	switch kind {
	case "text":
		return "text", text, session, ""
	case "tool", "tool_use", "tool_call":
		name := firstTextField(lookup(event, "part", "tool"), lookup(event, "properties", "part", "tool"), event["tool"])
		if name == "" {
			name = "tool"
		}
		return "tool", name, session, ""
	case "error":
		return "error", firstTextField(event["error"], lookup(event, "properties", "error"), event["message"]), session, ""
	}
	if text != "" && (kind == "message.part.updated" || kind == "message" || kind == "assistant") {
		return "text", text, session, ""
	}
	return "", "", session, ""
}

func stringField(value map[string]any, key string) string {
	if text, ok := value[key].(string); ok {
		return text
	}
	return ""
}

func lookup(value map[string]any, keys ...string) any {
	var current any = value
	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[key]
	}
	return current
}

func firstTextField(values ...any) string {
	for _, value := range values {
		switch typed := value.(type) {
		case string:
			if typed != "" {
				return typed
			}
		case map[string]any:
			for _, key := range []string{"text", "message", "name", "tool", "error"} {
				if text, ok := typed[key].(string); ok && text != "" {
					return text
				}
			}
		}
	}
	return ""
}

func findSessionID(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range []string{"sessionID", "sessionId", "session_id", "session"} {
			if text, ok := typed[key].(string); ok && text != "" {
				return text
			}
		}
		for _, child := range typed {
			if found := findSessionID(child); found != "" {
				return found
			}
		}
	case []any:
		for _, child := range typed {
			if found := findSessionID(child); found != "" {
				return found
			}
		}
	}
	return ""
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
