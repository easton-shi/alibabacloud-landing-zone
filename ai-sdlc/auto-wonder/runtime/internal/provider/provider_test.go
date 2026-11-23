package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCodexArgs(t *testing.T) {
	got := buildCodexArgs(true, "", "")
	want := []string{"--dangerously-bypass-hook-trust", "app-server", "--listen", "stdio://"}
	if !slices.Equal(got, want) {
		t.Fatalf("codex args = %v, want %v", got, want)
	}
	if got := buildCodexArgs(false, "", ""); len(got) != 3 || got[0] != "app-server" {
		t.Fatalf("unattended=false args = %v", got)
	}
	tuned := buildCodexArgs(false, "high", "128000")
	wantTuned := []string{"-c", "model_reasoning_effort=high", "-c",
		"model_context_window=128000", "app-server", "--listen", "stdio://"}
	if !slices.Equal(tuned, wantTuned) {
		t.Fatalf("tuned args = %v, want %v", tuned, wantTuned)
	}
}

func TestOpenCodeArgsPublicShape(t *testing.T) {
	got := buildOpenCodeArgs(Request{
		Prompt: "do work", Model: "litellm/deepseek", SessionID: "ses_1",
		PermissionFlag: "--auto", PromptFlag: "", Unattended: true,
	})
	want := []string{"run", "--format", "json", "--auto", "--model", "litellm/deepseek", "--session", "ses_1", "do work"}
	if !slices.Equal(got, want) {
		t.Fatalf("opencode args = %v, want %v", got, want)
	}
}

func TestOpenCodeArgsClosedSourceShape(t *testing.T) {
	got := buildOpenCodeArgs(Request{
		Prompt: "do work", PermissionFlag: "--dangerously-skip-permissions", PromptFlag: "--prompt", Unattended: true,
	})
	want := []string{"run", "--format", "json", "--dangerously-skip-permissions", "--prompt", "do work"}
	if !slices.Equal(got, want) {
		t.Fatalf("opencode args = %v, want %v", got, want)
	}
}

func TestPiArgs(t *testing.T) {
	got := buildPiArgs(Request{
		Prompt: "do work", Model: "anthropic/claude", SessionID: "/tmp/session.jsonl",
		SystemPrompt: "be terse",
	})
	want := []string{
		"-p", "--mode", "json", "--session", "/tmp/session.jsonl",
		"--provider", "anthropic", "--model", "claude",
		"--append-system-prompt", "be terse", "do work",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("pi args = %v, want %v", got, want)
	}
}

func TestClaudeArgsOnlyBypassWhenUnattended(t *testing.T) {
	args := buildClaudeArgs(Request{Prompt: "do work", Unattended: true})
	if !slices.Contains(args, "bypassPermissions") {
		t.Fatalf("unattended claude args = %v", args)
	}
	args = buildClaudeArgs(Request{Prompt: "do work"})
	if slices.Contains(args, "bypassPermissions") {
		t.Fatalf("attended claude args must not bypass permissions: %v", args)
	}
	if slices.Contains(args, "--mcp-config") {
		t.Fatalf("empty MCP path should be omitted: %v", args)
	}
}

func TestOpenCodeArgsDoNotAutoApproveWhenAttended(t *testing.T) {
	args := buildOpenCodeArgs(Request{
		Prompt: "do work", PermissionFlag: "--auto", Unattended: false,
	})
	if slices.Contains(args, "--auto") {
		t.Fatalf("attended opencode args = %v", args)
	}
}

func TestParseOpenCodeEvent(t *testing.T) {
	kind, text, session, _ := parseOpenCodeEvent(`{"type":"text","sessionID":"ses_9","part":{"text":"hello"}}`)
	if kind != "text" || text != "hello" || session != "ses_9" {
		t.Fatalf("got kind=%q text=%q session=%q", kind, text, session)
	}
	kind, text, _, _ = parseOpenCodeEvent(`{"type":"error","error":"boom"}`)
	if kind != "error" || text != "boom" {
		t.Fatalf("error event parsed as kind=%q text=%q", kind, text)
	}
}

func TestParsePiLine(t *testing.T) {
	kind, text, session, _ := parsePiLine(`{"type":"assistant_message","text":"hi","sessionId":"s1"}`)
	if kind != "text" || text != "hi" || session != "s1" {
		t.Fatalf("got kind=%q text=%q session=%q", kind, text, session)
	}
	kind, text, _, _ = parsePiLine("plain output")
	if kind != "text" || text != "plain output" {
		t.Fatalf("plain line parsed as kind=%q text=%q", kind, text)
	}
	kind, _, _, _ = parsePiLine("call: read_file")
	if kind != "tool" {
		t.Fatalf("control line parsed as %q", kind)
	}
}

func TestOpenCodeConfigContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	raw, _ := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			"autowonder": map[string]any{
				"type": "http", "url": "http://localhost/api/mcp",
				"headers": map[string]string{"Authorization": "Bearer t"},
			},
		},
	})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	content := openCodeConfigContent(path)
	var parsed map[string]any
	if json.Unmarshal([]byte(content), &parsed) != nil {
		t.Fatalf("invalid content: %s", content)
	}
	if _, ok := parsed["mcp"]; !ok {
		t.Fatalf("missing mcp key: %s", content)
	}
}

func TestCodexMCPProjectionKeepsBearerTokenOutOfArgs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	raw := `{"mcpServers":{
  "autowonder":{"type":"http","url":"https://autowonder.example/api/mcp","headers":{"Authorization":"Bearer super-secret"}},
  "local":{"command":"local-mcp","args":["--stdio"]}
}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	args, env, err := buildCodexMCPProjection(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range args {
		if strings.Contains(arg, "super-secret") {
			t.Fatalf("bearer token leaked into argv: %q", args)
		}
	}
	if !strings.Contains(strings.Join(args, "\n"), "bearer_token_env_var") {
		t.Fatalf("bearer token env var missing: %q", args)
	}
	if len(env) != 1 || env["AUTOWONDER_MCP_TOKEN_AUTOWONDER"] != "super-secret" {
		t.Fatalf("mcp environment = %#v", env)
	}
}

type recordingWriteCloser struct {
	buffer bytes.Buffer
	err    error
	order  *[]string
}

func (w *recordingWriteCloser) Write(p []byte) (int, error) {
	if w.order != nil {
		*w.order = append(*w.order, "write")
	}
	if w.err != nil {
		return 0, w.err
	}
	return w.buffer.Write(p)
}

func (w *recordingWriteCloser) Close() error { return nil }

func TestCodexApprovalOnlyApprovesExplicitAllow(t *testing.T) {
	tests := []struct {
		name   string
		answer InputAnswer
		ok     bool
		want   string
	}{
		{name: "accept", answer: InputAnswer{Action: "accept"}, ok: true, want: "approved"},
		{name: "approve", answer: InputAnswer{Action: "approve"}, ok: true, want: "approved"},
		{name: "decline", answer: InputAnswer{Action: "decline"}, ok: true, want: "denied"},
		{name: "cancel", answer: InputAnswer{Action: "cancel"}, ok: true, want: "denied"},
		{name: "no answer", ok: false, want: "denied"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stdin := &recordingWriteCloser{}
			client := &codexClient{
				stdin: stdin,
				emit:  func(Event) {},
				ask: func(context.Context, InputRequest) (InputAnswer, error) {
					if !test.ok {
						return InputAnswer{}, errors.New("input unavailable")
					}
					return test.answer, nil
				},
			}

			client.handleServerRequest(7, "item/permissions/requestApproval", json.RawMessage(`{"message":"allow?"}`))

			var response struct {
				Result struct {
					Decision string `json:"decision"`
				} `json:"result"`
			}
			if err := json.Unmarshal(bytes.TrimSpace(stdin.buffer.Bytes()), &response); err != nil {
				t.Fatalf("decode response: %v; raw=%s", err, stdin.buffer.String())
			}
			if response.Result.Decision != test.want {
				t.Fatalf("decision = %q, want %q", response.Result.Decision, test.want)
			}
		})
	}
}

func TestCodexAcknowledgesAnswerOnlyAfterWrite(t *testing.T) {
	var order []string
	stdin := &recordingWriteCloser{order: &order}
	client := &codexClient{
		stdin: stdin,
		emit:  func(Event) {},
		ask: func(context.Context, InputRequest) (InputAnswer, error) {
			return InputAnswer{
				Action: "accept",
				Written: func(err error) {
					order = append(order, "ack")
					if err != nil {
						t.Fatalf("unexpected write error: %v", err)
					}
				},
			}, nil
		},
	}

	client.handleServerRequest(8, "item/permissions/requestApproval", json.RawMessage(`{"message":"allow?"}`))

	if !slices.Equal(order, []string{"write", "ack"}) {
		t.Fatalf("write/ack order = %v", order)
	}
}

func TestCodexReportsAnswerWriteFailure(t *testing.T) {
	writeErr := errors.New("write failed")
	stdin := &recordingWriteCloser{err: writeErr}
	var reported error
	client := &codexClient{
		stdin: stdin,
		emit:  func(Event) {},
		ask: func(context.Context, InputRequest) (InputAnswer, error) {
			return InputAnswer{
				Action: "accept",
				Written: func(err error) {
					reported = err
				},
			}, nil
		},
	}

	client.handleServerRequest(9, "item/permissions/requestApproval", json.RawMessage(`{"message":"allow?"}`))

	if !errors.Is(reported, writeErr) {
		t.Fatalf("reported write error = %v, want %v", reported, writeErr)
	}
}

func TestMergeEnvironmentOverridesCaseInsensitively(t *testing.T) {
	got, err := MergeEnvironment(
		[]string{"PATH=/base", "API_KEY=old", "UNCHANGED=yes"},
		map[string]string{"path": "/platform", "api_key": "new", "NEW_VALUE": "present"},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"UNCHANGED=yes", "API_KEY=new", "NEW_VALUE=present", "PATH=/platform"}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("merged environment = %v, want %v", got, want)
	}
}

func TestValidateTaskEnvironmentRejectsRuntimeReservedNames(t *testing.T) {
	for _, name := range []string{"CODEX_HOME", "AUTOWONDER_WORKSPACE_ROOT", "Claude_Config_Dir"} {
		if err := ValidateTaskEnvironment(map[string]string{name: "value"}); err == nil {
			t.Fatalf("reserved variable %q was accepted", name)
		}
	}
}

func TestMergeEnvironmentRejectsMalformedName(t *testing.T) {
	if _, err := MergeEnvironment(nil, map[string]string{"BAD-NAME": "value"}); err == nil {
		t.Fatal("malformed environment name was accepted")
	}
}

func TestMergeEnvironmentAllowsRuntimeOwnedEntries(t *testing.T) {
	got, err := MergeEnvironment(nil, map[string]string{
		"AUTOWONDER_DISPATCH_ID": "42",
		"CODEX_HOME":             "/managed/codex-home",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("environment = %v", got)
	}
}

var _ io.WriteCloser = (*recordingWriteCloser)(nil)
