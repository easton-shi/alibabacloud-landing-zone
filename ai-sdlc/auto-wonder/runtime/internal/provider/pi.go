package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Pi runs `pi -p --mode json`. Its stdout is mostly NDJSON but may include
// control markup (`call:` / `response:`), so parsing is deliberately lenient.
type Pi struct {
	registry activeTurnRegistry
}

func (p *Pi) Name() string { return "pi" }

func (p *Pi) Steer(ctx context.Context, dispatchID int64, content string) error {
	return p.registry.steer(ctx, dispatchID, content)
}

func (p *Pi) Run(ctx context.Context, req Request, emit func(Event)) (Result, error) {
	return runSteerable(ctx, req, emit, &p.registry, p.runOnce)
}

func (p *Pi) runOnce(ctx context.Context, req Request, prompt string, emit func(Event), onStart func(error)) (Result, error) {
	req.Prompt = prompt
	args := buildPiArgs(req)
	sessionID := req.SessionID
	if sessionID == "" {
		dir := filepath.Join(req.WorkDir, ".pi", "sessions")
		if err := os.MkdirAll(dir, 0o755); err == nil {
			sessionID = filepath.Join(dir, fmt.Sprintf("session-%d.jsonl", time.Now().UnixNano()))
			_ = os.WriteFile(sessionID, nil, 0o600)
			args = buildPiArgs(piSessionRequest(req, sessionID))
			emit(Event{Kind: "session", SessionID: sessionID})
		}
	}

	var summary strings.Builder
	reportedSession := ""
	var engineErr string
	env, err := MergeEnvironment(currentEnv(), req.Env)
	if err != nil {
		onStart(err)
		return Result{}, fmt.Errorf("pi environment: %w", err)
	}
	stream := streamCommandWithStart(ctx, agentCommand(req.AgentPath, "pi"), args, req.WorkDir, env, func(line string) {
		emit(Event{Kind: "stdout", Text: truncate(line, 2000)})
		kind, text, session, _ := parsePiLine(line)
		if session != "" && reportedSession == "" {
			reportedSession = session
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
	}, onStart)
	if stream.err != nil && stream.exitCode != 0 {
		message := stream.stderr
		if message == "" {
			message = stream.err.Error()
		}
		return Result{Summary: truncate(summary.String(), 2000), FullText: summary.String(), SessionID: firstNonEmpty(reportedSession, sessionID)},
			fmt.Errorf("pi exited %d: %s", stream.exitCode, truncate(message, 500))
	}
	if engineErr != "" {
		return Result{Summary: truncate(summary.String(), 2000), FullText: summary.String(), SessionID: firstNonEmpty(reportedSession, sessionID)},
			fmt.Errorf("pi reported error: %s", truncate(engineErr, 500))
	}
	return Result{Summary: truncate(summary.String(), 2000), FullText: summary.String(), SessionID: firstNonEmpty(reportedSession, sessionID)}, nil
}

func piSessionRequest(req Request, sessionID string) Request {
	req.SessionID = sessionID
	return req
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func buildPiArgs(req Request) []string {
	args := []string{"-p", "--mode", "json"}
	if req.SessionID != "" {
		args = append(args, "--session", req.SessionID)
	}
	providerName, model := splitPiModel(req.Model)
	if req.PiProvider != "" {
		providerName = req.PiProvider
	}
	if providerName != "" {
		args = append(args, "--provider", providerName)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if req.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", req.SystemPrompt)
	}
	return append(args, req.Prompt)
}

func splitPiModel(model string) (providerName, name string) {
	if before, after, ok := strings.Cut(model, "/"); ok {
		return before, after
	}
	return "", model
}

func parsePiLine(line string) (kind, text, session, errText string) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "", "", "", ""
	}
	if strings.HasPrefix(trimmed, "{") {
		var event map[string]any
		if json.Unmarshal([]byte(trimmed), &event) == nil {
			session = findSessionID(event)
			eventType := strings.ToLower(stringField(event, "type"))
			text := firstTextField(event["text"], lookup(event, "message", "content"), lookup(event, "delta", "text"), event["content"])
			switch {
			case strings.Contains(eventType, "error"):
				return "error", firstTextField(event["error"], event["message"], text), session, ""
			case strings.Contains(eventType, "tool"):
				return "tool", firstTextField(event["name"], event["tool"], lookup(event, "tool", "name"), text), session, ""
			case strings.Contains(eventType, "message"), strings.Contains(eventType, "text"), strings.Contains(eventType, "assistant"):
				if text != "" {
					return "text", text, session, ""
				}
			}
			if text != "" {
				return "text", text, session, ""
			}
		}
	}
	if strings.HasPrefix(trimmed, "call:") || strings.HasPrefix(trimmed, "response:") {
		return "tool", trimmed, "", ""
	}
	return "text", trimmed, "", ""
}
