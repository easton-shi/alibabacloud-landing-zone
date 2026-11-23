package provider

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// agentCommand falls back to the provider's default binary when no absolute
// path was configured.
func agentCommand(configured, fallback string) string {
	if configured != "" {
		return configured
	}
	return fallback
}

type streamResult struct {
	exitCode int
	stderr   string
	err      error
}

// streamCommand runs a CLI and feeds stdout lines to onLine. It never blocks on
// stdin, which is what all one-shot engines expect.
func streamCommand(ctx context.Context, command string, args []string, workDir string, env []string,
	onLine func(string), onStderr func(string)) streamResult {
	return streamCommandWithStart(ctx, command, args, workDir, env, onLine, onStderr, func(error) {})
}

// streamCommandWithStart is streamCommand plus a start callback so steerable
// providers can report when the active turn is actually running.
func streamCommandWithStart(ctx context.Context, command string, args []string, workDir string, env []string,
	onLine func(string), onStderr func(string), onStart func(error)) streamResult {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = workDir
	cmd.Env = env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		onStart(err)
		return streamResult{err: fmt.Errorf("stdout pipe: %w", err)}
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		onStart(err)
		return streamResult{err: fmt.Errorf("stderr pipe: %w", err)}
	}
	if err := cmd.Start(); err != nil {
		onStart(err)
		return streamResult{err: fmt.Errorf("start %s: %w", command, err)}
	}
	onStart(nil)

	var stderrMu sync.Mutex
	var stderr strings.Builder
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		scanner := bufio.NewScanner(stderrPipe)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if onStderr != nil {
				onStderr(line)
			}
			stderrMu.Lock()
			stderr.WriteString(line + "\n")
			if stderr.Len() > 64*1024 {
				stderr.Reset()
			}
			stderrMu.Unlock()
		}
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		onLine(scanner.Text())
	}
	scanErr := scanner.Err()
	if scanErr != nil && !errors.Is(scanErr, io.EOF) {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	<-stderrDone

	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	stderrMu.Lock()
	stderrText := strings.TrimSpace(stderr.String())
	stderrMu.Unlock()
	return streamResult{exitCode: exitCode, stderr: stderrText, err: waitErr}
}
