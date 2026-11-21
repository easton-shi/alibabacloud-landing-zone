package hooks

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type fixtureHook struct {
	name     string
	trigger  string
	interp   string
	command  string
	script   string
	once     string
	failure  string
	cwd      string
	enabled  bool
	extraEnv string
}

func writePackage(t *testing.T, hooks []fixtureHook) string {
	t.Helper()
	root := t.TempDir()
	records := make([]map[string]any, 0, len(hooks))
	for _, hook := range hooks {
		dir := filepath.Join(root, "capabilities", "hooks", hook.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		scriptDir := filepath.Join(dir, filepath.Dir(hook.command))
		if err := os.MkdirAll(scriptDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, hook.command), []byte(hook.script), 0o755); err != nil {
			t.Fatal(err)
		}
		cwd := hook.cwd
		if cwd == "" {
			cwd = "workspace"
		}
		env := ""
		if hook.extraEnv != "" {
			env = "env:\n" + hook.extraEnv + "\n"
		}
		descriptor := "schemaVersion: autowonder.hook.v1\nname: " + hook.name +
			"\nversion: '1'\ntrigger: " + hook.trigger + "\ninterpreter: " + hook.interp +
			"\ncommand: " + hook.command + "\ncwd: " + cwd + "\ntimeoutSeconds: 10\n" +
			env + "failurePolicy: " + hook.failure + "\nonceScope: " + hook.once + "\n"
		if err := os.WriteFile(filepath.Join(dir, "hook.yaml"), []byte(descriptor), 0o644); err != nil {
			t.Fatal(err)
		}
		digest, err := treeDigest(dir)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, map[string]any{
			"name": hook.name, "version": "1", "sha256": digest, "enabled": hook.enabled,
		})
	}
	document, _ := json.Marshal(map[string]any{
		"schemaVersion": "autowonder.hooks.v1", "hooks": records,
	})
	if err := os.WriteFile(filepath.Join(root, "hooks.json"), document, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func contextFor(t *testing.T, packageDir string) Context {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	artifacts := filepath.Join(root, "artifacts", "output")
	state := filepath.Join(root, "state")
	for _, dir := range []string{workspace, artifacts, state} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return Context{RootDir: root, PackageDir: packageDir, WorkspaceDir: workspace,
		StateDir: state, ArtifactsDir: artifacts, DispatchID: 42, StepID: "700001",
		Attempt: 1, Provider: "codex"}
}

func TestLoadVerifiesDigest(t *testing.T) {
	root := writePackage(t, []fixtureHook{
		{name: "env", trigger: BeforeAgentStart, interp: "sh", command: "scripts/check.sh",
			script: "exit 0\n", once: OncePerAttempt, failure: FailureFailTask, enabled: true},
		{name: "off", trigger: Cleanup, interp: "sh", command: "scripts/off.sh",
			script: "exit 0\n", once: OnceNone, failure: FailureContinue, enabled: false},
	})
	set, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Hooks) != 1 || set.Hooks[0].Name != "env" {
		t.Fatalf("unexpected hooks: %+v", set.Hooks)
	}
	if set.Hooks[0].TimeoutSeconds != 10 || set.Hooks[0].Cwd != "workspace" {
		t.Fatalf("defaults not applied: %+v", set.Hooks[0])
	}
	if err := os.WriteFile(filepath.Join(root, "capabilities", "hooks", "env", "scripts", "check.sh"),
		[]byte("tampered\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("expected digest mismatch, got %v", err)
	}
}

func TestLoadRejectsPathEscapeAndUnknownTrigger(t *testing.T) {
	for _, hook := range []fixtureHook{
		{name: "escape", trigger: BeforeAgentStart, interp: "sh", command: "../outside.sh",
			script: "exit 0\n", once: OnceNone, failure: FailureFailTask, enabled: true},
		{name: "unknown", trigger: "beforeLunch", interp: "sh", command: "scripts/check.sh",
			script: "exit 0\n", once: OnceNone, failure: FailureFailTask, enabled: true},
	} {
		if _, err := Load(writePackage(t, []fixtureHook{hook})); err == nil {
			t.Fatalf("expected load failure for %s", hook.name)
		}
	}
}

func TestRunInjectsEnvironmentAndCwd(t *testing.T) {
	root := writePackage(t, []fixtureHook{{name: "env", trigger: BeforeStep, interp: "sh",
		command: "scripts/check.sh", script: "printf '%s|%s|%s|%s\\n' \"$AUTOWONDER_STEP_ID\" \"$AUTOWONDER_DISPATCH_ID\" \"$CUSTOM\" \"$(pwd)\" > \"$AUTOWONDER_ARTIFACTS_OUTPUT_DIR/env.txt\"\n",
		once: OnceNone, failure: FailureFailTask, enabled: true, extraEnv: "  CUSTOM: packaged"}})
	set, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	hookContext := contextFor(t, root)
	hookContext.DispatchEnv = map[string]string{"CUSTOM": "dispatch"}
	results, err := set.Run(context.Background(), BeforeStep, hookContext)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Success {
		t.Fatalf("hook did not succeed: %+v", results)
	}
	raw, err := os.ReadFile(filepath.Join(hookContext.ArtifactsDir, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(raw))
	parts := strings.Split(line, "|")
	if len(parts) != 4 || parts[0] != "700001" || parts[1] != "42" || parts[2] != "packaged" {
		t.Fatalf("unexpected hook environment: %q", line)
	}
	if filepath.Clean(parts[3]) != filepath.Clean(hookContext.WorkspaceDir) {
		t.Fatalf("unexpected cwd: %q", parts[3])
	}
	if _, err := os.Stat(filepath.Join(results[0].LogDir, "stdout.log")); err != nil {
		t.Fatalf("missing stdout log: %v", err)
	}
}

func TestRunOnceScopes(t *testing.T) {
	root := writePackage(t, []fixtureHook{{name: "once", trigger: BeforeStep, interp: "sh",
		command: "scripts/check.sh", script: "printf 'run\\n' >> \"$AUTOWONDER_STATE_DIR/count.txt\"\n",
		once: OncePerAttempt, failure: FailureFailTask, enabled: true}})
	set, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	hookContext := contextFor(t, root)
	for index := 0; index < 2; index++ {
		if _, err := set.Run(context.Background(), BeforeStep, hookContext); err != nil {
			t.Fatal(err)
		}
	}
	if count := strings.Count(readFile(t, filepath.Join(hookContext.StateDir, "count.txt")), "run"); count != 1 {
		t.Fatalf("once-per-attempt ran %d times", count)
	}

	root = writePackage(t, []fixtureHook{{name: "step", trigger: BeforeStep, interp: "sh",
		command: "scripts/check.sh", script: "printf 'run\\n' >> \"$AUTOWONDER_STATE_DIR/count.txt\"\n",
		once: OncePerStep, failure: FailureFailTask, enabled: true}})
	set, err = Load(root)
	if err != nil {
		t.Fatal(err)
	}
	hookContext = contextFor(t, root)
	for index := 0; index < 2; index++ {
		if _, err := set.Run(context.Background(), BeforeStep, hookContext); err != nil {
			t.Fatal(err)
		}
	}
	hookContext.StepID = "700002"
	if _, err := set.Run(context.Background(), BeforeStep, hookContext); err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(readFile(t, filepath.Join(hookContext.StateDir, "count.txt")), "run"); count != 2 {
		t.Fatalf("once-per-step ran %d times, want 2", count)
	}
}

func TestRunFailurePolicies(t *testing.T) {
	root := writePackage(t, []fixtureHook{{name: "soft", trigger: BeforeAgentStart, interp: "sh",
		command: "scripts/check.sh", script: "echo boom >&2\nexit 7\n",
		once: OnceNone, failure: FailureContinue, enabled: true}})
	set, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	results, err := set.Run(context.Background(), BeforeAgentStart, contextFor(t, root))
	if err != nil {
		t.Fatalf("continue policy returned error: %v", err)
	}
	if results[0].Success || results[0].ExitCode != 7 || !strings.Contains(results[0].Stderr, "boom") {
		t.Fatalf("unexpected result: %+v", results[0])
	}

	root = writePackage(t, []fixtureHook{{name: "hard", trigger: BeforeAgentStart, interp: "sh",
		command: "scripts/check.sh", script: "echo boom >&2\nexit 7\n",
		once: OnceNone, failure: FailureFailTask, enabled: true}})
	set, err = Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.Run(context.Background(), BeforeAgentStart, contextFor(t, root)); err == nil {
		t.Fatal("expected fail-task error")
	}
}

func TestInstallGitHooksAndRunChain(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	root := writePackage(t, []fixtureHook{{name: "commit-check", trigger: BeforeCommit, interp: "sh",
		command: "scripts/check.sh", script: "printf '%s\\n' \"$(pwd)\" > \"$AUTOWONDER_ARTIFACTS_OUTPUT_DIR/commit.txt\"\n",
		once: OnceNone, failure: FailureFailTask, cwd: "triggeringRepo", enabled: true}})
	set, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init", "-q")

	original := filepath.Join(repo, ".git", "hooks")
	if err := os.WriteFile(filepath.Join(original, "pre-commit"),
		[]byte("#!/bin/sh\nprintf 'original\\n' >> \"$ORIGINAL_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeRuntime := filepath.Join(t.TempDir(), "fake-runtime")
	if err := os.WriteFile(fakeRuntime, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$FAKE_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeLog := filepath.Join(t.TempDir(), "fake.log")
	gitContext := GitContext{RootDir: t.TempDir(), PackageDir: root, WorkspaceDir: filepath.Join(t.TempDir(), "workspace"),
		StateDir: filepath.Join(t.TempDir(), "state"), ArtifactsDir: t.TempDir(), DispatchID: 42,
		StepID: "700001", Attempt: 1, Provider: "codex", Repos: []GitRepo{{Name: "source", Dir: repo}}}
	if err := InstallGitHooksWithExecutable(filepath.Dir(gitContext.PackageDir), set, gitContext, fakeRuntime); err != nil {
		t.Fatal(err)
	}
	configured, err := gitOutput(repo, "config", "core.hooksPath")
	if err != nil || configured == "" {
		t.Fatalf("core.hooksPath not configured: %v", err)
	}
	command := exec.Command(filepath.Join(configured, "pre-commit"))
	command.Dir = repo
	command.Env = append(os.Environ(), "FAKE_LOG="+fakeLog)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated hook failed: %v: %s", err, output)
	}
	if raw := readFile(t, fakeLog); !strings.Contains(raw, "hook run --context") ||
		!strings.Contains(raw, "--trigger beforeCommit") {
		t.Fatalf("unexpected generated hook invocation: %q", raw)
	}

	originalLog := filepath.Join(t.TempDir(), "original.log")
	t.Setenv("ORIGINAL_LOG", originalLog)
	if err := RunGitHook(context.Background(), filepath.Join(configured, "hook-context.json"),
		BeforeCommit, repo, original, nil); err != nil {
		t.Fatal(err)
	}
	if raw := strings.TrimSpace(readFile(t, filepath.Join(gitContext.ArtifactsDir, "commit.txt"))); filepath.Clean(raw) != filepath.Clean(repo) {
		t.Fatalf("hook cwd was %q, want %q", raw, repo)
	}
	if !strings.Contains(readFile(t, originalLog), "original") {
		t.Fatal("original pre-commit hook was not chained")
	}
}

func TestRunGitHookBlocksOnFailTask(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	root := writePackage(t, []fixtureHook{{name: "block", trigger: BeforeCommit, interp: "sh",
		command: "scripts/check.sh", script: "echo blocked >&2\nexit 1\n",
		once: OnceNone, failure: FailureFailTask, cwd: "triggeringRepo", enabled: true}})
	set, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "repo")
	_ = os.MkdirAll(repo, 0o755)
	runGit(t, repo, "init", "-q")
	gitContext := GitContext{RootDir: t.TempDir(), PackageDir: root, WorkspaceDir: t.TempDir(),
		StateDir: t.TempDir(), ArtifactsDir: t.TempDir(), DispatchID: 1, Provider: "codex",
		Repos: []GitRepo{{Name: "source", Dir: repo}}}
	if err := InstallGitHooksWithExecutable(t.TempDir(), set, gitContext, "/bin/true"); err != nil {
		t.Fatal(err)
	}
	contextPath := filepath.Join(filepath.Dir(gitContext.PackageDir), "git-hooks", "hook-context.json")
	if err := RunGitHook(context.Background(), contextPath, BeforeCommit, repo, "", nil); err == nil {
		t.Fatal("expected fail-task hook to block commit")
	}
}

func TestAllPlatformTriggersAreRegistered(t *testing.T) {
	for _, definition := range []string{BeforeRepoPrepare, BeforeStep, BeforeAgentStart,
		BeforeTool, AfterTool, AfterAgentExit, BeforeCommit, BeforePush, Cleanup} {
		if !knownTriggers[definition] {
			t.Fatalf("trigger %s is not registered", definition)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(raw)
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
