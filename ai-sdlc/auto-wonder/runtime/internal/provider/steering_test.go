package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAllProvidersImplementSteerable(t *testing.T) {
	providers := []Provider{
		&Codex{},
		&OpenCode{},
		&Pi{},
		&Claude{},
	}
	for _, provider := range providers {
		steerable, ok := provider.(Steerable)
		if !ok {
			t.Fatalf("%s does not implement Steerable", provider.Name())
		}
		if err := steerable.Steer(context.Background(), 1, "continue"); !errors.Is(err, ErrNoActiveTurn) {
			t.Fatalf("%s Steer without active turn = %v, want ErrNoActiveTurn", provider.Name(), err)
		}
	}
}

func TestCodexSteeringUsesNativeTurnSteer(t *testing.T) {
	const script = `#!/usr/bin/env python3
import json, os, sys, time

log_path = os.environ.get("FAKE_CODEX_STEER_LOG", "")

def send(message):
    sys.stdout.write(json.dumps(message) + "\n")
    sys.stdout.flush()

for line in sys.stdin:
    message = json.loads(line)
    method = message.get("method")
    ident = message.get("id")
    if method == "initialize":
        send({"jsonrpc": "2.0", "id": ident, "result": {}})
    elif method == "thread/start":
        send({"jsonrpc": "2.0", "id": ident, "result": {"threadId": "thread-fake"}})
    elif method == "turn/start":
        send({"jsonrpc": "2.0", "id": ident, "result": {"turnId": "turn-fake"}})
    elif method == "turn/steer":
        if log_path:
            with open(log_path, "w", encoding="utf-8") as handle:
                json.dump(message, handle)
        send({"jsonrpc": "2.0", "id": ident, "result": {"turnId": "turn-fake"}})
        send({"jsonrpc": "2.0", "method": "turn/completed", "params": {}})
        break
`
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-codex")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "steer.json")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	engine := &Codex{}
	done := make(chan error, 1)
	go func() {
		_, err := engine.Run(ctx, Request{
			DispatchID: 42,
			AgentPath:  scriptPath,
			WorkDir:    dir,
			Prompt:     "initial",
			Env:        map[string]string{"FAKE_CODEX_STEER_LOG": logPath},
		}, func(Event) {})
		done <- err
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		err := engine.Steer(context.Background(), 42, "new direction")
		if err == nil {
			break
		}
		if !errors.Is(err, ErrNoActiveTurn) {
			t.Fatalf("Steer returned %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for Codex active turn")
		}
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Codex Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Codex Run did not finish after steering")
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Method string `json:"method"`
		Params struct {
			ThreadID       string `json:"threadId"`
			ExpectedTurnID string `json:"expectedTurnId"`
			Input          []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"input"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	if request.Method != "turn/steer" {
		t.Fatalf("method = %q, want turn/steer", request.Method)
	}
	if request.Params.ThreadID != "thread-fake" || request.Params.ExpectedTurnID != "turn-fake" {
		t.Fatalf("unexpected turn/steer target: %#v", request.Params)
	}
	if len(request.Params.Input) != 1 || request.Params.Input[0].Text != "new direction" {
		t.Fatalf("unexpected steering input: %#v", request.Params.Input)
	}
}

func TestOpenCodeCLISteeringCancelsAndResumesSameSession(t *testing.T) {
	const script = `#!/usr/bin/env python3
import json, os, sys, time

log_path = os.environ.get("FAKE_OPENCODE_STEER_LOG", "")
args = sys.argv[1:]
with open(log_path, "a", encoding="utf-8") as handle:
    handle.write(json.dumps(args) + "\n")

session = "ses_fake"
sys.stdout.write(json.dumps({"type": "text", "sessionID": session, "text": "started"}) + "\n")
sys.stdout.flush()

if "--session" in args:
    sys.stdout.write(json.dumps({"type": "text", "sessionID": session, "text": "steered"}) + "\n")
    sys.stdout.flush()
    sys.exit(0)

time.sleep(60)
`
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-opencode")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "args.log")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	engine := &OpenCode{}
	done := make(chan error, 1)
	var summaries []string
	go func() {
		result, err := engine.Run(ctx, Request{
			DispatchID: 43,
			AgentPath:  scriptPath,
			WorkDir:    dir,
			Prompt:     "initial",
			Env:        map[string]string{"FAKE_OPENCODE_STEER_LOG": logPath},
		}, func(event Event) {
			if event.Kind == "text" {
				summaries = append(summaries, event.Text)
			}
		})
		if err == nil {
			summaries = append(summaries, result.Summary)
		}
		done <- err
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		err := engine.Steer(context.Background(), 43, "new direction")
		if err == nil {
			break
		}
		if !errors.Is(err, ErrNoActiveTurn) {
			t.Fatalf("Steer returned %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for OpenCode active turn")
		}
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("OpenCode Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OpenCode Run did not finish after steering")
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected two OpenCode invocations, got %d: %q", len(lines), raw)
	}
	var first, second []string
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(first, "--session") {
		t.Fatalf("first invocation unexpectedly resumed a session: %#v", first)
	}
	if !slices.Contains(second, "--session") || !slices.Contains(second, "ses_fake") {
		t.Fatalf("second invocation did not resume the captured session: %#v", second)
	}
	if !slices.Contains(second, "new direction") {
		t.Fatalf("second invocation did not receive steering content: %#v", second)
	}
	if !slices.Contains(summaries, "steered") {
		t.Fatalf("steered result summary missing: %#v", summaries)
	}
}

func TestActiveTurnRegistryRejectsNoActiveTurn(t *testing.T) {
	var registry activeTurnRegistry
	err := registry.steer(context.Background(), 41, "continue")
	if !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("expected ErrNoActiveTurn, got %v", err)
	}
}

func TestRunSteerableCancelsAndResumesSameSession(t *testing.T) {
	var registry activeTurnRegistry
	var mu sync.Mutex
	var prompts []string
	var sessions []string
	started := make(chan struct{}, 2)

	runTurn := func(ctx context.Context, req Request, prompt string, emit func(Event), onStart func(error)) (Result, error) {
		mu.Lock()
		prompts = append(prompts, prompt)
		sessions = append(sessions, req.SessionID)
		call := len(prompts)
		mu.Unlock()
		onStart(nil)
		started <- struct{}{}
		if call == 1 {
			<-ctx.Done()
			return Result{SessionID: "session-1", Summary: "partial"}, ctx.Err()
		}
		return Result{SessionID: "session-2", Summary: "steered"}, nil
	}

	done := make(chan struct {
		result Result
		err    error
	}, 1)
	go func() {
		result, err := runSteerable(context.Background(), Request{
			DispatchID: 77, Prompt: "initial", SessionID: "session-0",
		}, func(Event) {}, &registry, runTurn)
		done <- struct {
			result Result
			err    error
		}{result, err}
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first turn did not start")
	}
	if err := registry.steer(context.Background(), 77, "new direction"); err != nil {
		t.Fatalf("steer failed: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("steered turn did not start")
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("runSteerable returned error: %v", got.err)
		}
		if got.result.Summary != "steered" {
			t.Fatalf("unexpected result: %#v", got.result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runSteerable did not finish")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(prompts) != 2 || prompts[0] != "initial" || prompts[1] != "new direction" {
		t.Fatalf("unexpected prompts: %#v", prompts)
	}
	if len(sessions) != 2 || sessions[0] != "session-0" || sessions[1] != "session-1" {
		t.Fatalf("steered turn did not resume the same session: %#v", sessions)
	}
}

func TestOpenCodeACPSteeringCancelsAndResumesSameSession(t *testing.T) {
	const script = `#!/usr/bin/env python3
import json, os, sys, time

log_path = os.environ.get("FAKE_ACP_STEER_LOG", "")
state_path = os.environ.get("FAKE_ACP_STEER_STATE", "")

first = True
if state_path:
    if os.path.exists(state_path):
        first = False
    else:
        with open(state_path, "w", encoding="utf-8") as handle:
            handle.write("first")

def log(message):
    if log_path:
        with open(log_path, "a", encoding="utf-8") as handle:
            handle.write(json.dumps(message) + "\n")

def send(message):
    sys.stdout.write(json.dumps(message) + "\n")
    sys.stdout.flush()

for line in sys.stdin:
    message = json.loads(line)
    log(message)
    method = message.get("method")
    ident = message.get("id")
    if method == "initialize":
        send({"jsonrpc": "2.0", "id": ident, "result": {}})
    elif method == "session/new":
        send({"jsonrpc": "2.0", "id": ident, "result": {"sessionId": "ses_fake"}})
    elif method == "session/load":
        send({"jsonrpc": "2.0", "id": ident, "result": {"sessionId": "ses_fake"}})
    elif method == "session/prompt":
        if first:
            time.sleep(60)
        else:
            send({"jsonrpc": "2.0", "method": "session/update", "params": {"sessionId": "ses_fake", "update": {"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": "steered"}}}})
            send({"jsonrpc": "2.0", "id": ident, "result": {"stopReason": "end_turn"}})
            sys.exit(0)
    elif method == "session/cancel":
        pass
`
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-opencode-acp")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "acp.log")
	statePath := filepath.Join(dir, "acp.state")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	engine := &OpenCode{}
	done := make(chan error, 1)
	go func() {
		_, err := engine.Run(ctx, Request{
			DispatchID: 44,
			AgentPath:  scriptPath,
			WorkDir:    dir,
			Prompt:     "initial",
			UseACP:     true,
			Env: map[string]string{
				"FAKE_ACP_STEER_LOG":   logPath,
				"FAKE_ACP_STEER_STATE": statePath,
			},
		}, func(Event) {})
		done <- err
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		err := engine.Steer(context.Background(), 44, "new direction")
		if err == nil {
			break
		}
		if !errors.Is(err, ErrNoActiveTurn) {
			t.Fatalf("Steer returned %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for OpenCode ACP active turn")
		}
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("OpenCode ACP Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OpenCode ACP Run did not finish after steering")
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var prompts []string
	var sessionLoad bool
	for _, line := range lines {
		var message struct {
			Method string `json:"method"`
			Params struct {
				Prompt []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			continue
		}
		if message.Method == "session/load" {
			sessionLoad = true
		}
		if message.Method == "session/prompt" {
			for _, prompt := range message.Params.Prompt {
				prompts = append(prompts, prompt.Text)
			}
		}
	}
	if !sessionLoad {
		t.Fatal("OpenCode ACP steering did not resume the same session")
	}
	if !slices.Contains(prompts, "new direction") {
		t.Fatalf("OpenCode ACP steering prompt missing: %#v", prompts)
	}
}

func TestPiSteeringCancelsAndResumesSameSession(t *testing.T) {
	const script = `#!/usr/bin/env python3
import json, os, sys, time

log_path = os.environ.get("FAKE_PI_STEER_LOG", "")
state_path = os.environ.get("FAKE_PI_STEER_STATE", "")
args = sys.argv[1:]
if log_path:
    with open(log_path, "a", encoding="utf-8") as handle:
        handle.write(json.dumps(args) + "\n")

first = True
if state_path:
    if os.path.exists(state_path):
        first = False
    else:
        with open(state_path, "w", encoding="utf-8") as handle:
            handle.write("first")

if not first:
    sys.stdout.write(json.dumps({"type": "text", "text": "steered"}) + "\n")
    sys.stdout.flush()
    sys.exit(0)

sys.stdout.write(json.dumps({"type": "text", "text": "started"}) + "\n")
sys.stdout.flush()
time.sleep(60)
`
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-pi")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "pi.log")
	statePath := filepath.Join(dir, "pi.state")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	engine := &Pi{}
	done := make(chan error, 1)
	started := make(chan struct{}, 1)
	var sessionMu sync.Mutex
	var capturedSession string
	go func() {
		result, err := engine.Run(ctx, Request{
			DispatchID: 45,
			AgentPath:  scriptPath,
			WorkDir:    dir,
			Prompt:     "initial",
			Env: map[string]string{
				"FAKE_PI_STEER_LOG":   logPath,
				"FAKE_PI_STEER_STATE": statePath,
			},
		}, func(event Event) {
			if event.Kind == "session" {
				sessionMu.Lock()
				capturedSession = event.SessionID
				sessionMu.Unlock()
			}
			if event.Kind == "text" && event.Text == "started" {
				select {
				case started <- struct{}{}:
				default:
				}
			}
		})
		if err == nil {
			sessionMu.Lock()
			if capturedSession == "" {
				capturedSession = result.SessionID
			}
			sessionMu.Unlock()
		}
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Pi first turn did not start")
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		err := engine.Steer(context.Background(), 45, "new direction")
		if err == nil {
			break
		}
		if !errors.Is(err, ErrNoActiveTurn) {
			t.Fatalf("Steer returned %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for Pi active turn")
		}
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Pi Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Pi Run did not finish after steering")
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected two Pi invocations, got %d: %q", len(lines), raw)
	}
	var first, second []string
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	sessionMu.Lock()
	sessionID := capturedSession
	sessionMu.Unlock()
	if sessionID == "" ||
		!slices.Contains(first, "--session") || !slices.Contains(first, sessionID) ||
		!slices.Contains(second, "--session") || !slices.Contains(second, sessionID) {
		t.Fatalf("Pi invocations did not use the same captured session %q: first=%#v second=%#v", sessionID, first, second)
	}
	if !slices.Contains(second, "new direction") {
		t.Fatalf("second Pi invocation did not receive steering content: %#v", second)
	}
}

func TestClaudeSteeringCancelsAndResumesSameSession(t *testing.T) {
	const script = `#!/usr/bin/env python3
import json, os, sys, time

log_path = os.environ.get("FAKE_CLAUDE_STEER_LOG", "")
args = sys.argv[1:]
if log_path:
    with open(log_path, "a", encoding="utf-8") as handle:
        handle.write(json.dumps(args) + "\n")

if "--resume" in args:
    sys.stdout.write(json.dumps({"type": "result", "session_id": "claude-session", "result": "steered"}) + "\n")
    sys.stdout.flush()
    sys.exit(0)

sys.stdout.write(json.dumps({"type": "system", "session_id": "claude-session", "subtype": "init"}) + "\n")
sys.stdout.flush()
time.sleep(60)
`
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "claude.log")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	engine := &Claude{}
	done := make(chan error, 1)
	go func() {
		_, err := engine.Run(ctx, Request{
			DispatchID: 46,
			AgentPath:  scriptPath,
			WorkDir:    dir,
			Prompt:     "initial",
			Env:        map[string]string{"FAKE_CLAUDE_STEER_LOG": logPath},
		}, func(Event) {})
		done <- err
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		err := engine.Steer(context.Background(), 46, "new direction")
		if err == nil {
			break
		}
		if !errors.Is(err, ErrNoActiveTurn) {
			t.Fatalf("Steer returned %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for Claude active turn")
		}
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Claude Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Claude Run did not finish after steering")
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected two Claude invocations, got %d: %q", len(lines), raw)
	}
	var first, second []string
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(first, "--resume") {
		t.Fatalf("first Claude invocation unexpectedly resumed a session: %#v", first)
	}
	if !slices.Contains(second, "--resume") || !slices.Contains(second, "claude-session") {
		t.Fatalf("second Claude invocation did not resume the captured session: %#v", second)
	}
	if !slices.Contains(second, "new direction") {
		t.Fatalf("second Claude invocation did not receive steering content: %#v", second)
	}
}
