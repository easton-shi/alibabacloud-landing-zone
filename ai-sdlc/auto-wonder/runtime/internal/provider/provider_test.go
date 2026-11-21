package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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
