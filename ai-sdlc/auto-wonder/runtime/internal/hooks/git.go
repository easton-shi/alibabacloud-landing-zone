package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GitRepo identifies one materialized checkout for Git-triggered hooks.
type GitRepo struct {
	Name          string `json:"name"`
	Dir           string `json:"dir"`
	OriginalHooks string `json:"originalHooks"`
}

// GitContext is persisted next to the Git hook wrappers so a child process can
// reproduce the dispatch environment without parsing the task package again.
type GitContext struct {
	RootDir      string            `json:"rootDir"`
	PackageDir   string            `json:"packageDir"`
	WorkspaceDir string            `json:"workspaceDir"`
	StateDir     string            `json:"stateDir"`
	ArtifactsDir string            `json:"artifactsDir"`
	DispatchID   int64             `json:"dispatchId"`
	StepID       string            `json:"stepId"`
	Attempt      int               `json:"attempt"`
	Provider     string            `json:"provider"`
	Repos        []GitRepo         `json:"repos"`
	DispatchEnv  map[string]string `json:"dispatchEnv,omitempty"`
}

// WriteGitContext persists the context used by generated Git hooks.
func WriteGitContext(path string, gitContext GitContext) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(gitContext, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// LoadGitContext reads a persisted Git hook context.
func LoadGitContext(path string) (GitContext, error) {
	var gitContext GitContext
	raw, err := os.ReadFile(path)
	if err != nil {
		return gitContext, err
	}
	err = json.Unmarshal(raw, &gitContext)
	return gitContext, err
}

// InstallGitHooks points each repository at a shared hooks directory when the
// package declares beforeCommit/beforePush. Existing repository hooks remain
// installed and are chained after the Runtime hooks.
func InstallGitHooks(root string, set *Set, gitContext GitContext) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve runtime executable: %w", err)
	}
	return InstallGitHooksWithExecutable(root, set, gitContext, executable)
}

// InstallGitHooksWithExecutable is InstallGitHooks with an injectable binary.
func InstallGitHooksWithExecutable(root string, set *Set, gitContext GitContext,
	executable string) error {
	triggers := make([]string, 0, 2)
	if set.Has(BeforeCommit) {
		triggers = append(triggers, BeforeCommit)
	}
	if set.Has(BeforePush) {
		triggers = append(triggers, BeforePush)
	}
	if len(triggers) == 0 {
		return nil
	}
	hooksDir := filepath.Join(root, "git-hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return err
	}
	contextPath := filepath.Join(hooksDir, "hook-context.json")
	for index, repo := range gitContext.Repos {
		original, err := gitOutput(repo.Dir, "rev-parse", "--git-path", "hooks")
		if err != nil {
			return fmt.Errorf("resolve original hooks for %s: %w", repo.Name, err)
		}
		if !filepath.IsAbs(original) {
			original = filepath.Join(repo.Dir, original)
		}
		gitContext.Repos[index].OriginalHooks = filepath.Clean(original)
	}
	if err := WriteGitContext(contextPath, gitContext); err != nil {
		return err
	}
	for _, trigger := range triggers {
		name := "pre-commit"
		if trigger == BeforePush {
			name = "pre-push"
		}
		script := "#!/bin/sh\nset -u\nrepo=$(git rev-parse --show-toplevel 2>/dev/null) || exit 1\nexec " +
			shellQuote(executable) + " hook run --context " + shellQuote(contextPath) +
			" --trigger " + trigger + " --repo \"$repo\" -- \"$@\"\n"
		path := filepath.Join(hooksDir, name)
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			return err
		}
	}
	for _, repo := range gitContext.Repos {
		if _, err := gitOutput(repo.Dir, "config", "core.hooksPath", hooksDir); err != nil {
			return fmt.Errorf("configure hooks for %s: %w", repo.Name, err)
		}
	}
	return nil
}

// RunGitHook executes Runtime Git-triggered hooks and then chains the
// repository's original hook. A non-zero original hook is propagated.
func RunGitHook(ctx context.Context, contextPath, trigger, repoDir, originalHooks string,
	gitArgs []string) error {
	gitContext, err := LoadGitContext(contextPath)
	if err != nil {
		return fmt.Errorf("load git hook context: %w", err)
	}
	repo, err := matchRepo(gitContext.Repos, repoDir)
	if err != nil {
		return err
	}
	if originalHooks == "" {
		originalHooks = repo.OriginalHooks
	}
	set, err := Load(gitContext.PackageDir)
	if err != nil {
		return err
	}
	results, runErr := set.Run(ctx, trigger, Context{
		RootDir:            gitContext.RootDir,
		PackageDir:         gitContext.PackageDir,
		WorkspaceDir:       gitContext.WorkspaceDir,
		StateDir:           gitContext.StateDir,
		ArtifactsDir:       gitContext.ArtifactsDir,
		DispatchID:         gitContext.DispatchID,
		StepID:             gitContext.StepID,
		Attempt:            gitContext.Attempt,
		Provider:           gitContext.Provider,
		TriggeringRepoDir:  repo.Dir,
		TriggeringRepoName: repo.Name,
		DispatchEnv:        gitContext.DispatchEnv,
	})
	for _, result := range results {
		if result.Skipped {
			continue
		}
		if !result.Success {
			fmt.Fprintf(os.Stderr, "autowonder hook %s failed: %s\n", result.Name, result.message())
		}
	}
	if runErr != nil {
		return runErr
	}
	return chainOriginal(ctx, trigger, repoDir, originalHooks,
		filepath.Join(gitContext.RootDir, "git-hooks"), gitArgs)
}

func chainOriginal(ctx context.Context, trigger, repoDir, originalHooks, runtimeHooksDir string,
	gitArgs []string) error {
	if originalHooks == "" || samePath(originalHooks, runtimeHooksDir) {
		return nil
	}
	name := "pre-commit"
	if trigger == BeforePush {
		name = "pre-push"
	}
	path := filepath.Join(originalHooks, name)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return nil
	}
	command := exec.CommandContext(ctx, path, gitArgs...)
	command.Dir = repoDir
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Env = os.Environ()
	if err := command.Run(); err != nil {
		return fmt.Errorf("original %s hook failed: %w", name, err)
	}
	return nil
}

func matchRepo(repos []GitRepo, dir string) (GitRepo, error) {
	target := canonical(dir)
	for _, repo := range repos {
		if canonical(repo.Dir) == target {
			return repo, nil
		}
	}
	return GitRepo{}, fmt.Errorf("repository %s is not owned by this dispatch", dir)
}

func canonical(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		absolute = path
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		absolute = resolved
	}
	return filepath.Clean(absolute)
}

func samePath(left, right string) bool {
	return canonical(left) == canonical(right)
}

func gitOutput(dir string, args ...string) (string, error) {
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err,
			strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
