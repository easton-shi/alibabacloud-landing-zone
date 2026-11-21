package hooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func toolContextFile(t *testing.T, hookContext Context) string {
	t.Helper()
	path := filepath.Join(hookContext.RootDir, "tool-hooks", "hook-context.json")
	err := WriteGitContext(path, GitContext{
		RootDir: hookContext.RootDir, PackageDir: hookContext.PackageDir,
		WorkspaceDir: hookContext.WorkspaceDir, StateDir: hookContext.StateDir,
		ArtifactsDir: hookContext.ArtifactsDir, DispatchID: hookContext.DispatchID,
		StepID: hookContext.StepID, Attempt: hookContext.Attempt, Provider: "claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunToolHooksPassesCanonicalStdin(t *testing.T) {
	root := writePackage(t, []fixtureHook{{
		name: "tool-guard", trigger: BeforeTool, interp: "sh", command: "scripts/guard.sh",
		script: "cat > \"$AUTOWONDER_ARTIFACTS_OUTPUT_DIR/tool-input.json\"\nexit 0\n",
		once:   OnceNone, failure: FailureFailTask, enabled: true,
	}})
	hookContext := contextFor(t, root)
	contextPath := toolContextFile(t, hookContext)
	results, err := RunToolHooks(context.Background(), contextPath, BeforeTool, "claude", ToolHookInput{
		Trigger: BeforeTool, Provider: "claude", Cwd: hookContext.WorkspaceDir,
		ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"ls"}`), CallID: "call-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Success {
		t.Fatalf("results = %+v", results)
	}
	raw, err := os.ReadFile(filepath.Join(hookContext.ArtifactsDir, "tool-input.json"))
	if err != nil {
		t.Fatal(err)
	}
	var input ToolHookInput
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	if input.SchemaVersion != "autowonder.toolHookContext.v1" || input.Trigger != BeforeTool ||
		input.Provider != "claude" || input.ToolName != "Bash" || input.ToolNameAlias != "Bash" ||
		input.CallID != "call-1" || string(input.ToolInput) != `{"command":"ls"}` ||
		string(input.ToolInputAlias) != `{"command":"ls"}` {
		t.Fatalf("stdin = %s", raw)
	}
}

func TestRunToolHooksFailTaskReturnsError(t *testing.T) {
	root := writePackage(t, []fixtureHook{{
		name: "tool-guard", trigger: BeforeTool, interp: "sh", command: "scripts/guard.sh",
		script: "echo blocked >&2\nexit 2\n", once: OnceNone, failure: FailureFailTask, enabled: true,
	}})
	hookContext := contextFor(t, root)
	contextPath := toolContextFile(t, hookContext)
	_, err := RunToolHooks(context.Background(), contextPath, BeforeTool, "claude", ToolHookInput{
		ToolName: "Bash", ToolInput: json.RawMessage(`{}`),
	})
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("error = %v", err)
	}
}

func TestWriteClaudeToolSettings(t *testing.T) {
	root := writePackage(t, []fixtureHook{
		{name: "before", trigger: BeforeTool, interp: "sh", command: "scripts/a.sh",
			script: "exit 0\n", once: OnceNone, failure: FailureFailTask, enabled: true},
		{name: "after", trigger: AfterTool, interp: "sh", command: "scripts/b.sh",
			script: "exit 0\n", once: OnceNone, failure: FailureContinue, enabled: true},
	})
	set, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	path, err := WriteClaudeToolSettings(filepath.Join(root, "settings.json"),
		"/tmp/hook context.json", "/opt/my runtime/autowonder-runtime", set)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, expected := range []string{"PreToolUse", "PostToolUse", "beforeTool", "afterTool",
		"'/opt/my runtime/autowonder-runtime'", "'/tmp/hook context.json'"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("settings missing %q: %s", expected, text)
		}
	}
}
