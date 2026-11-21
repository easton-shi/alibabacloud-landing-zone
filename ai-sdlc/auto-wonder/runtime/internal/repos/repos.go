// Package repos materializes task-package repositories into the dispatch workspace.
package repos

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Spec mirrors one entry in repos.json.
type Spec struct {
	RepoID                     int64    `json:"repoId"`
	Name                       string   `json:"name"`
	URL                        string   `json:"url"`
	Ref                        string   `json:"ref"`
	Path                       string   `json:"path"`
	Mode                       string   `json:"mode"`
	DeliveryBranch             string   `json:"deliveryBranch"`
	DeliveryBaseCommit         string   `json:"deliveryBaseCommit"`
	SourceRevisionFallbackRefs []string `json:"sourceRevisionFallbackRefs"`
	PinnedBaseCommit           string   `json:"pinnedBaseCommit"`
	AllowCommit                bool     `json:"allowCommit"`
	AllowPush                  bool     `json:"allowPush"`
	AllowNetwork               bool     `json:"allowNetwork"`
	AllowedBranchPatterns      []string `json:"allowedBranchPatterns"`
	DeliveryUnpushedHeadCommit string   `json:"deliveryUnpushedHeadCommit"`
}

// Checkout describes one materialized repository.
type Checkout struct {
	Spec       Spec
	Dir        string
	BaseCommit string
}

// Head returns the current HEAD commit of a checkout.
func Head(ctx context.Context, dir string) (string, error) {
	head, err := runGit(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("read repository head: %w", err)
	}
	return strings.TrimSpace(head), nil
}

// Branch returns the current branch, or an empty string for a detached HEAD.
func Branch(ctx context.Context, dir string) (string, error) {
	branch, err := runGit(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(branch), nil
}

// Restore checks each named repository back to the commit recorded in a checkpoint.
func Restore(ctx context.Context, checkouts []Checkout, heads map[string]string) error {
	for _, checkout := range checkouts {
		head := strings.TrimSpace(heads[checkout.Spec.Name])
		if head == "" {
			continue
		}
		if _, err := runGit(ctx, checkout.Dir, "cat-file", "-e", head+"^{commit}"); err != nil {
			if !checkout.Spec.AllowNetwork {
				return redactError(checkout.Spec, "restore checkpoint commit", err)
			}
			if _, fetchErr := runGit(ctx, checkout.Dir, "fetch", "--no-tags", "origin", head); fetchErr != nil {
				return redactError(checkout.Spec, "fetch checkpoint commit", fetchErr)
			}
		}
		if _, err := runGit(ctx, checkout.Dir, "checkout", "--detach", head, "--"); err != nil {
			return redactError(checkout.Spec, "restore checkpoint commit", err)
		}
	}
	return nil
}

type packageDocument struct {
	Repos []Spec `json:"repos"`
}

// Load reads repository specifications from a task package.
func Load(packageDir string) ([]Spec, error) {
	raw, err := os.ReadFile(filepath.Join(packageDir, "repos.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read repos.json: %w", err)
	}
	var document packageDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("parse repos.json: %w", err)
	}
	for index := range document.Repos {
		spec := &document.Repos[index]
		if err := normalize(spec); err != nil {
			return nil, err
		}
	}
	return document.Repos, nil
}

// SplitByMode separates eager checkouts from lazy repositories that must only
// be cloned when the agent decides it needs them.
func SplitByMode(specs []Spec) (eager, lazy []Spec) {
	for _, spec := range specs {
		if strings.EqualFold(spec.Mode, "lazy") {
			lazy = append(lazy, spec)
		} else {
			eager = append(eager, spec)
		}
	}
	return eager, lazy
}

// Materialize clones or refreshes every repository and checks out its pinned
// revision. The implementation deliberately uses the host git credential
// helper, SSH agent and other standard git configuration; credentials are never
// copied into the task workspace by the runtime.
func Materialize(ctx context.Context, workspaceDir string, specs []Spec) ([]Checkout, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	root := filepath.Join(workspaceDir, "repos")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create repository root: %w", err)
	}
	checkouts := make([]Checkout, 0, len(specs))
	for _, raw := range specs {
		spec := raw
		if err := normalize(&spec); err != nil {
			return nil, err
		}
		checkout, err := materializeOne(ctx, root, spec)
		if err != nil {
			return nil, err
		}
		checkouts = append(checkouts, checkout)
	}
	return checkouts, nil
}

func normalize(spec *Spec) error {
	if spec.Path == "" {
		spec.Path = spec.Name
	}
	if err := validateSegment("repository name", spec.Name); err != nil {
		return err
	}
	if err := validateSegment("repository path", spec.Path); err != nil {
		return err
	}
	if spec.URL == "" || strings.HasPrefix(spec.URL, "-") {
		return fmt.Errorf("repository %q has an invalid URL", spec.Name)
	}
	if spec.Mode == "" {
		spec.Mode = "eager"
	}
	spec.Mode = strings.ToLower(spec.Mode)
	return nil
}

func materializeOne(ctx context.Context, root string, spec Spec) (Checkout, error) {
	target := filepath.Join(root, spec.Path)
	if err := ensureClone(ctx, spec, target); err != nil {
		return Checkout{}, err
	}
	commit, err := resolveCommit(ctx, target, spec)
	if err != nil {
		return Checkout{}, err
	}
	if _, err := runGit(ctx, target, "checkout", "--detach", commit, "--"); err != nil {
		return Checkout{}, redactError(spec, "checkout revision", err)
	}
	if spec.DeliveryBranch != "" && validBranch(spec.DeliveryBranch) {
		if _, err := runGit(ctx, target, "switch", "-C", spec.DeliveryBranch, commit); err != nil {
			return Checkout{}, redactError(spec, "create delivery branch", err)
		}
	}
	head, err := runGit(ctx, target, "rev-parse", "HEAD")
	if err != nil {
		return Checkout{}, redactError(spec, "read checked-out revision", err)
	}
	return Checkout{Spec: spec, Dir: target, BaseCommit: strings.TrimSpace(head)}, nil
}

func ensureClone(ctx context.Context, spec Spec, target string) error {
	if info, err := os.Stat(filepath.Join(target, ".git")); err == nil && info.IsDir() {
		if spec.AllowNetwork {
			if _, fetchErr := runGit(ctx, target, "fetch", "--prune", "--tags", "origin"); fetchErr != nil {
				return redactError(spec, "fetch repository", fetchErr)
			}
		}
		return nil
	}
	if !spec.AllowNetwork {
		return fmt.Errorf("repository %q is not materialized and allowNetwork=false", spec.Name)
	}
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("repository path already exists but is not a git checkout: %s", target)
	}
	temp, err := os.MkdirTemp(filepath.Dir(target), "."+filepath.Base(target)+".clone-")
	if err != nil {
		return fmt.Errorf("create clone directory: %w", err)
	}
	if err := os.Remove(temp); err != nil {
		return fmt.Errorf("prepare clone directory: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(temp)
		}
	}()
	if _, err := runGit(ctx, "", "clone", "--no-checkout", "--origin", "origin", "--", spec.URL, temp); err != nil {
		return redactError(spec, "clone repository", err)
	}
	if err := os.Rename(temp, target); err != nil {
		return fmt.Errorf("install repository checkout %q: %w", spec.Name, err)
	}
	cleanup = false
	return nil
}

func resolveCommit(ctx context.Context, dir string, spec Spec) (string, error) {
	candidates := make([]string, 0, len(spec.SourceRevisionFallbackRefs)+4)
	candidates = append(candidates, spec.PinnedBaseCommit, spec.Ref, spec.DeliveryBranch)
	candidates = append(candidates, spec.SourceRevisionFallbackRefs...)
	candidates = append(candidates, "origin/HEAD")
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || strings.HasPrefix(candidate, "-") {
			continue
		}
		if commit, err := runGit(ctx, dir, "rev-parse", "--verify", "--quiet", candidate+"^{commit}"); err == nil {
			return strings.TrimSpace(commit), nil
		}
		if strings.HasPrefix(candidate, "origin/") || isCommit(candidate) {
			continue
		}
		if commit, err := runGit(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+candidate+"^{commit}"); err == nil {
			return strings.TrimSpace(commit), nil
		}
		if spec.AllowNetwork {
			if _, err := runGit(ctx, dir, "fetch", "--no-tags", "origin", candidate); err == nil {
				if commit, fetchErr := runGit(ctx, dir, "rev-parse", "--verify", "FETCH_HEAD^{commit}"); fetchErr == nil {
					return strings.TrimSpace(commit), nil
				}
			}
		}
	}
	return "", redactError(spec, "resolve repository revision", fmt.Errorf("no checkout revision could be resolved"))
}

// Revision is the runtime-source-revision.json record consumed by the server
// when it packages the next dispatch.
type Revision struct {
	Name             string `json:"name"`
	Branch           string `json:"branch,omitempty"`
	BaseCommit       string `json:"baseCommit,omitempty"`
	HeadCommit       string `json:"headCommit"`
	RemoteHeadCommit string `json:"remoteHeadCommit,omitempty"`
}

type revisionDocument struct {
	SchemaVersion string     `json:"schemaVersion"`
	Repositories  []Revision `json:"repositories"`
}

// WriteRevision records the final branches and commits after the agent runs.
func WriteRevision(ctx context.Context, checkouts []Checkout, path string) error {
	if len(checkouts) == 0 {
		return nil
	}
	document := revisionDocument{
		SchemaVersion: "autowonder.runtimeSourceRevision.v1",
		Repositories:  make([]Revision, 0, len(checkouts)),
	}
	for _, checkout := range checkouts {
		head, err := runGit(ctx, checkout.Dir, "rev-parse", "HEAD")
		if err != nil {
			return redactError(checkout.Spec, "read final revision", err)
		}
		branch, _ := runGit(ctx, checkout.Dir, "symbolic-ref", "--quiet", "--short", "HEAD")
		branch = strings.TrimSpace(branch)
		revision := Revision{
			Name:       checkout.Spec.Name,
			Branch:     branch,
			BaseCommit: checkout.BaseCommit,
			HeadCommit: strings.TrimSpace(head),
		}
		if branch != "" && checkout.Spec.AllowNetwork {
			if remote, err := remoteHead(ctx, checkout.Dir, branch); err == nil {
				if remote == revision.HeadCommit {
					revision.RemoteHeadCommit = remote
				}
			}
		}
		document.Repositories = append(document.Repositories, revision)
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("encode runtime source revision: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create revision directory: %w", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("write runtime source revision: %w", err)
	}
	return nil
}

func remoteHead(ctx context.Context, dir, branch string) (string, error) {
	if !validBranch(branch) {
		return "", fmt.Errorf("invalid branch")
	}
	output, err := runGit(ctx, dir, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(output)
	if len(fields) == 0 || !isCommit(fields[0]) {
		return "", fmt.Errorf("branch %q is not present on origin", branch)
	}
	return fields[0], nil
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		command.Dir = dir
	}
	output, err := command.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		if text == "" {
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, text)
	}
	return text, nil
}

func validateSegment(label, value string) error {
	if value == "" || value == "." || value == ".." || filepath.Base(value) != value ||
		strings.ContainsAny(value, `/\`) || strings.ContainsRune(value, 0) {
		return fmt.Errorf("%s %q must be a single path segment", label, value)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s %q contains a control character", label, value)
		}
	}
	return nil
}

func validBranch(value string) bool {
	if value == "" || strings.HasPrefix(value, "-") || strings.HasPrefix(value, "/") ||
		strings.HasSuffix(value, "/") || strings.HasSuffix(value, ".") ||
		strings.HasSuffix(value, ".lock") || strings.Contains(value, "..") ||
		strings.Contains(value, "@{") || strings.Contains(value, "//") || value == "@" {
		return false
	}
	if strings.ContainsAny(value, " ~^:?*[\\") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

func isCommit(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func redactError(spec Spec, action string, err error) error {
	message := strings.ReplaceAll(err.Error(), spec.URL, redactURL(spec.URL))
	return fmt.Errorf("%s %q: %s", action, spec.Name, message)
}

func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return raw
	}
	parsed.User = url.User("***")
	return parsed.String()
}
