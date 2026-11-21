package repos

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRejectsUnsafePath(t *testing.T) {
	dir := t.TempDir()
	raw := `{"repos":[{"name":"api","path":"../escape","url":"https://example.test/api.git"}]}`
	if err := os.WriteFile(filepath.Join(dir, "repos.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected unsafe path to be rejected")
	}
}

func TestMaterializeAndWriteRevision(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	mustGit(t, "", "init", "--bare", origin)
	mustGit(t, "", "init", "-b", "main", seed)
	mustGit(t, seed, "config", "user.email", "runtime@example.test")
	mustGit(t, seed, "config", "user.name", "Runtime Test")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, seed, "add", "README.md")
	mustGit(t, seed, "commit", "-m", "first")
	first := strings.TrimSpace(mustGit(t, seed, "rev-parse", "HEAD"))
	mustGit(t, seed, "remote", "add", "origin", origin)
	mustGit(t, seed, "push", "origin", "main")

	workspace := filepath.Join(root, "workspace")
	checkouts, err := Materialize(ctx, workspace, []Spec{{
		Name: "api", URL: origin, Ref: first, DeliveryBranch: "aw/test",
		AllowNetwork: true, AllowCommit: true, AllowPush: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(checkouts) != 1 {
		t.Fatalf("checkouts = %d", len(checkouts))
	}
	if body, err := os.ReadFile(filepath.Join(checkouts[0].Dir, "README.md")); err != nil || string(body) != "one\n" {
		t.Fatalf("materialized file = %q, err=%v", body, err)
	}
	if branch := strings.TrimSpace(mustGit(t, checkouts[0].Dir, "branch", "--show-current")); branch != "aw/test" {
		t.Fatalf("branch = %q", branch)
	}
	// Publish the base branch first so the test also covers a remote branch
	// that exists but does not yet contain the agent's new local commit.
	mustGit(t, checkouts[0].Dir, "push", "-u", "origin", "aw/test")

	if err := os.WriteFile(filepath.Join(checkouts[0].Dir, "README.md"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, checkouts[0].Dir, "add", "README.md")
	mustGit(t, checkouts[0].Dir, "commit", "-m", "second")
	second := strings.TrimSpace(mustGit(t, checkouts[0].Dir, "rev-parse", "HEAD"))

	revisionPath := filepath.Join(root, "artifacts", "deliverables", "runtime-source-revision.json")
	if err := WriteRevision(ctx, checkouts, revisionPath); err != nil {
		t.Fatal(err)
	}
	beforePush := readRevisions(t, revisionPath)
	if beforePush[0].HeadCommit != second || beforePush[0].RemoteHeadCommit != "" {
		t.Fatalf("revision before push = %+v", beforePush[0])
	}

	mustGit(t, checkouts[0].Dir, "push", "-u", "origin", "aw/test")
	if err := WriteRevision(ctx, checkouts, revisionPath); err != nil {
		t.Fatal(err)
	}
	afterPush := readRevisions(t, revisionPath)
	if afterPush[0].RemoteHeadCommit != second {
		t.Fatalf("revision after push = %+v", afterPush[0])
	}
}

func readRevisions(t *testing.T, path string) []Revision {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document revisionDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	return document.Repositories
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := runGit(context.Background(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func TestSplitByModeSeparatesLazyRepositories(t *testing.T) {
	eager, lazy := SplitByMode([]Spec{
		{Name: "a", Mode: "eager"},
		{Name: "b", Mode: "LAZY"},
		{Name: "c"},
	})
	if len(eager) != 2 || eager[0].Name != "a" || eager[1].Name != "c" {
		t.Fatalf("eager = %#v", eager)
	}
	if len(lazy) != 1 || lazy[0].Name != "b" {
		t.Fatalf("lazy = %#v", lazy)
	}
}
