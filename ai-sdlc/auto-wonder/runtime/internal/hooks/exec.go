package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	maxLogBytes    = 1 << 20
	maxResultBytes = 8 << 10
)

// Context carries one invocation's environment and workspace locations.
type Context struct {
	RootDir            string
	PackageDir         string
	WorkspaceDir       string
	StateDir           string
	ArtifactsDir       string
	DispatchID         int64
	StepID             string
	Attempt            int
	Provider           string
	TriggeringRepoDir  string
	TriggeringRepoName string
	DispatchEnv        map[string]string
	Stdin              []byte
}

// Result is the observable outcome of one hook invocation.
type Result struct {
	Name     string        `json:"name"`
	Trigger  string        `json:"trigger"`
	Success  bool          `json:"success"`
	Skipped  bool          `json:"skipped,omitempty"`
	ExitCode int           `json:"exitCode"`
	Duration time.Duration `json:"-"`
	LogDir   string        `json:"logDir"`
	Stdout   string        `json:"stdout,omitempty"`
	Stderr   string        `json:"stderr,omitempty"`
	Error    string        `json:"error,omitempty"`
}

// Run executes every hook for the trigger in declaration order. It returns an
// error only when a hook with failurePolicy=fail-task fails (or a hook cannot
// start at all under that policy). Continue-policy failures remain in Results.
func (s *Set) Run(ctx context.Context, trigger string, hookContext Context) ([]Result, error) {
	var results []Result
	for _, definition := range s.Select(trigger) {
		if definition.OnceScope != OnceNone {
			skip, err := stateSaysSkip(hookContext.StateDir, definition, hookContext.StepID)
			if err != nil {
				return results, fmt.Errorf("hook %s state: %w", definition.Name, err)
			}
			if skip {
				results = append(results, Result{Name: definition.Name, Trigger: definition.Trigger,
					Success: true, Skipped: true, LogDir: logDir(hookContext, definition)})
				continue
			}
		}
		result := runOne(ctx, definition, hookContext)
		results = append(results, result)
		if result.Success && definition.OnceScope != OnceNone {
			if err := markState(hookContext.StateDir, definition, hookContext.StepID); err != nil {
				return results, fmt.Errorf("hook %s state: %w", definition.Name, err)
			}
		}
		if !result.Success && definition.FailurePolicy == FailureFailTask {
			return results, fmt.Errorf("hook %s (%s) failed: %s", definition.Name, definition.Trigger, result.message())
		}
	}
	return results, nil
}

func (r Result) message() string {
	if r.Stderr != "" {
		return r.Stderr
	}
	if r.Stdout != "" {
		return r.Stdout
	}
	if r.Error != "" {
		return r.Error
	}
	return "exit code " + strconv.Itoa(r.ExitCode)
}

func runOne(ctx context.Context, definition Definition, hookContext Context) Result {
	started := time.Now()
	result := Result{Name: definition.Name, Trigger: definition.Trigger,
		LogDir: logDir(hookContext, definition)}
	if err := os.MkdirAll(result.LogDir, 0o755); err != nil {
		result.Error = "create hook log dir: " + err.Error()
		return result
	}
	stdoutPath := filepath.Join(result.LogDir, "stdout.log")
	stderrPath := filepath.Join(result.LogDir, "stderr.log")
	stdoutFile, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		result.Error = "open stdout log: " + err.Error()
		return result
	}
	defer stdoutFile.Close()
	stderrFile, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		result.Error = "open stderr log: " + err.Error()
		return result
	}
	defer stderrFile.Close()

	commandPath, err := resolveCommand(definition.Dir, definition.Command)
	if err != nil {
		result.Error = err.Error()
		result.Stderr = result.Error
		writeResult(result.LogDir, definition, result, started)
		return result
	}
	cwd, err := workingDir(definition, hookContext)
	if err != nil {
		result.Error = err.Error()
		result.Stderr = result.Error
		writeResult(result.LogDir, definition, result, started)
		return result
	}

	timeout := time.Duration(definition.TimeoutSeconds) * time.Second
	runContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var command *exec.Cmd
	args := append([]string{commandPath}, definition.Args...)
	if definition.Interpreter != "" {
		interpreter := definition.Interpreter
		if resolved, lookErr := exec.LookPath(interpreter); lookErr == nil {
			interpreter = resolved
		}
		command = exec.CommandContext(runContext, interpreter, args...)
	} else {
		command = exec.CommandContext(runContext, commandPath, definition.Args...)
	}
	command.Dir = cwd
	command.Env = buildEnv(definition, hookContext)
	if hookContext.Stdin != nil {
		command.Stdin = bytes.NewReader(hookContext.Stdin)
	}
	stdout := &limitedWriter{writer: stdoutFile, limit: maxLogBytes}
	stderr := &limitedWriter{writer: stderrFile, limit: maxLogBytes}
	command.Stdout = stdout
	command.Stderr = stderr

	runErr := command.Run()
	result.Duration = time.Since(started)
	result.ExitCode = 0
	if command.ProcessState != nil {
		result.ExitCode = command.ProcessState.ExitCode()
	}
	if runErr != nil {
		result.Error = runErr.Error()
		if errors.Is(runContext.Err(), context.DeadlineExceeded) {
			result.Error = fmt.Sprintf("hook timed out after %ds", definition.TimeoutSeconds)
		} else {
			var exitErr *exec.ExitError
			if errors.As(runErr, &exitErr) {
				result.Error = fmt.Sprintf("exit code %d", exitErr.ExitCode())
			}
		}
	}
	result.Success = runErr == nil
	result.Stdout = tail(stdoutPath, maxResultBytes)
	result.Stderr = tail(stderrPath, maxResultBytes)
	if result.Success && stdout.truncated {
		result.Error = ""
	}
	writeResult(result.LogDir, definition, result, started)
	return result
}

func workingDir(definition Definition, hookContext Context) (string, error) {
	switch definition.Cwd {
	case "workspace":
		return hookContext.WorkspaceDir, nil
	case "triggeringRepo":
		if hookContext.TriggeringRepoDir == "" {
			return "", fmt.Errorf("cwd=triggeringRepo requires a triggering repository")
		}
		return hookContext.TriggeringRepoDir, nil
	default:
		return "", fmt.Errorf("unsupported cwd %q", definition.Cwd)
	}
}

func buildEnv(definition Definition, hookContext Context) []string {
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		if key, value, ok := strings.Cut(entry, "="); ok {
			values[key] = value
		}
	}
	for key, value := range hookContext.DispatchEnv {
		values[key] = value
	}
	for key, value := range definition.Env {
		values[key] = value
	}
	values["AUTOWONDER_HOOK_NAME"] = definition.Name
	values["AUTOWONDER_HOOK_TRIGGER"] = definition.Trigger
	values["AUTOWONDER_PROVIDER"] = hookContext.Provider
	values["AUTOWONDER_DISPATCH_ID"] = strconv.FormatInt(hookContext.DispatchID, 10)
	values["AUTOWONDER_STEP_ID"] = hookContext.StepID
	values["AUTOWONDER_STEP_ATTEMPT"] = strconv.Itoa(hookContext.Attempt)
	values["AUTOWONDER_STATE_DIR"] = hookContext.StateDir
	values["AUTOWONDER_ARTIFACTS_OUTPUT_DIR"] = hookContext.ArtifactsDir
	values["AUTOWONDER_WORKSPACE_DIR"] = hookContext.WorkspaceDir
	values["AUTOWONDER_PACKAGE_DIR"] = hookContext.PackageDir
	values["AUTOWONDER_TASK_ROOT"] = hookContext.RootDir
	if hookContext.TriggeringRepoName != "" {
		values["AUTOWONDER_TRIGGERING_REPO"] = hookContext.TriggeringRepoName
		values["AUTOWONDER_TRIGGERING_REPO_PATH"] = hookContext.TriggeringRepoDir
	}
	env := make([]string, 0, len(values))
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	return env
}

func logDir(hookContext Context, definition Definition) string {
	return filepath.Join(hookContext.RootDir, "logs", "hooks", definition.Trigger, definition.Name)
}

func writeResult(dir string, definition Definition, result Result, started time.Time) {
	payload := map[string]any{
		"name":        definition.Name,
		"trigger":     definition.Trigger,
		"version":     definition.Version,
		"success":     result.Success,
		"skipped":     result.Skipped,
		"exitCode":    result.ExitCode,
		"durationMs":  time.Since(started).Milliseconds(),
		"error":       result.Error,
		"stdoutTail":  result.Stdout,
		"stderrTail":  result.Stderr,
		"failureMode": definition.FailurePolicy,
		"onceScope":   definition.OnceScope,
	}
	raw, _ := json.MarshalIndent(payload, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "result.json"), raw, 0o600)
}

func tail(path string, limit int64) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ""
	}
	offset := int64(0)
	if info.Size() > limit {
		offset = info.Size() - limit
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	raw, _ := io.ReadAll(io.LimitReader(file, limit))
	return strings.TrimSpace(string(raw))
}

type limitedWriter struct {
	writer    io.Writer
	limit     int64
	written   int64
	truncated bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.written >= w.limit {
		w.truncated = true
		return len(p), nil
	}
	remaining := w.limit - w.written
	if int64(len(p)) > remaining {
		p = p[:remaining]
		w.truncated = true
	}
	n, err := w.writer.Write(p)
	w.written += int64(n)
	if err != nil {
		return n, err
	}
	return len(p), nil
}

// ToolHookInput is the provider-neutral stdin contract for tool hooks.
type ToolHookInput struct {
	SchemaVersion string          `json:"schemaVersion"`
	Trigger       string          `json:"trigger"`
	Provider      string          `json:"provider"`
	Cwd           string          `json:"cwd"`
	ToolName      string          `json:"toolName"`
	ToolInput     json.RawMessage `json:"toolInput"`
	CallID        string          `json:"callId,omitempty"`
	ToolOutput    json.RawMessage `json:"toolOutput,omitempty"`

	// Compatibility aliases documented by the platform.
	ToolNameAlias   string          `json:"tool_name"`
	ToolInputAlias  json.RawMessage `json:"tool_input"`
	ToolOutputAlias json.RawMessage `json:"tool_output,omitempty"`
}

// EncodeToolInput renders the canonical tool-hook stdin payload.
func EncodeToolInput(input ToolHookInput) ([]byte, error) {
	input.SchemaVersion = "autowonder.toolHookContext.v1"
	input.ToolNameAlias = input.ToolName
	input.ToolInputAlias = input.ToolInput
	input.ToolOutputAlias = input.ToolOutput
	return json.Marshal(input)
}

// RunToolHooks executes every beforeTool/afterTool hook with the shared stdin
// contract. A fail-task result returns an error so the provider adapter can
// block the call (beforeTool) or fail the task (afterTool).
func RunToolHooks(ctx context.Context, contextPath, trigger, providerName string,
	input ToolHookInput) ([]Result, error) {
	runtimeContext, err := LoadGitContext(contextPath)
	if err != nil {
		return nil, fmt.Errorf("load tool hook context: %w", err)
	}
	set, err := Load(runtimeContext.PackageDir)
	if err != nil {
		return nil, err
	}
	stdin, err := EncodeToolInput(input)
	if err != nil {
		return nil, err
	}
	repoDir := ""
	repoName := ""
	if input.Cwd != "" {
		repoDir = input.Cwd
		repoName = filepath.Base(input.Cwd)
	}
	return set.Run(ctx, trigger, Context{
		RootDir:            runtimeContext.RootDir,
		PackageDir:         runtimeContext.PackageDir,
		WorkspaceDir:       runtimeContext.WorkspaceDir,
		StateDir:           runtimeContext.StateDir,
		ArtifactsDir:       runtimeContext.ArtifactsDir,
		DispatchID:         runtimeContext.DispatchID,
		StepID:             runtimeContext.StepID,
		Attempt:            runtimeContext.Attempt,
		Provider:           providerName,
		TriggeringRepoDir:  repoDir,
		TriggeringRepoName: repoName,
		DispatchEnv:        runtimeContext.DispatchEnv,
		Stdin:              stdin,
	})
}

// MarkFailure writes the dispatch failure marker used by afterTool hooks.
func MarkFailure(artifactsDir, message string) error {
	if artifactsDir == "" {
		return nil
	}
	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(artifactsDir, "FAILURE"), []byte(message+"\n"), 0o600)
}
