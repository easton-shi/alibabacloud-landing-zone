package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/capsule"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/repos"
)

func TestBuildIncludesOfficialSdlcShapeSkillsAndRepositories(t *testing.T) {
	root := t.TempDir()
	packageDir := filepath.Join(root, "package")
	workspaceDir := filepath.Join(root, "workspace")
	artifactsDir := filepath.Join(root, "artifacts", "output")
	for _, dir := range []string{packageDir, workspaceDir, artifactsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"identity.json": `{"roleName":"Backend Developer","roleCode":"AW_RD","agentMd":"Follow the repository policy."}`,
		"sdlc.json": `{
  "currentStepId":"700002",
  "steps":[
    {"id":"700001","name":"plan","kind":"AGENT","instruction":"write a plan"},
    {"id":"700002","name":"implement","kind":"AGENT","instruction":"implement the fix",
     "checklist":[{"text":"add tests","checked":false}]}
  ]
}`,
		"workitem.md": "# Fix login\n\nLogin fails.",
		"skills.json": `{
  "skills":[{"name":"repo-review","path":"capabilities/skills/repo-review","version":"3","required":true,"description":"Review changes"}],
  "mcpServers":[{"name":"search","transport":"http","url":"https://search.example/mcp"}]
}`,
		"memory/team.md": "Use the shared error envelope.",
		"repo-map.json":  `{"boundRepoIds":[1001],"relations":[{"fromRepoName":"order-service","toRepoName":"payment-service","relationType":"CALLS"}]}`,
	}
	for name, body := range files {
		path := filepath.Join(packageDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	box := &capsule.Capsule{Root: root, PackageDir: packageDir, WorkspaceDir: workspaceDir, ArtifactsDir: artifactsDir}
	prompt, err := Build(box, BuildWork{Checkouts: []repos.Checkout{{
		Spec: repos.Spec{
			Name: "order-service", Ref: "main", DeliveryBranch: "aw/fix-login",
			AllowCommit: true, AllowPush: true, AllowNetwork: true,
		},
		Dir: filepath.Join(workspaceDir, "repos", "order-service"), BaseCommit: "1234567890abcdef",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"Backend Developer",
		"Step: implement (AGENT)",
		"implement the fix",
		"add tests",
		"capabilities/skills/repo-review/SKILL.md",
		"order-service",
		"aw/fix-login",
		"Use the shared error envelope.",
		"payment-service",
		"Repository relations",
		artifactsDir + "/deliverables/",
		"runtime-source-revision.json",
	} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("prompt missing %q:\n%s", expected, prompt)
		}
	}
	if strings.Contains(prompt, "write a plan") {
		t.Fatalf("prompt selected the wrong SDLC step:\n%s", prompt)
	}
}

func TestBuildConversationListsLazyRepositoriesWithoutCloning(t *testing.T) {
	root := t.TempDir()
	packageDir := filepath.Join(root, "package")
	if err := os.MkdirAll(packageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "skills.json"),
		[]byte(`{"skills":[{"name":"repo-review","path":"capabilities/skills/repo-review","version":"1"}]}`),
		0o600); err != nil {
		t.Fatal(err)
	}
	prompt := BuildConversation("You are the platform assistant.", "Which repo should change?",
		packageDir, filepath.Join(root, "workspace"), nil, []repos.Spec{{
			Name: "payment-service", URL: "git@example.com:org/payment-service.git",
			Path: "payment-service", Ref: "main", Mode: "lazy",
		}})
	for _, expected := range []string{
		"You are the platform assistant.",
		"Which repo should change?",
		"repo-review/SKILL.md",
		"Repositories available on demand",
		"payment-service",
		"git@example.com:org/payment-service.git",
	} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("conversation prompt missing %q:\n%s", expected, prompt)
		}
	}
	if strings.Contains(prompt, "These repositories are already checked out") {
		t.Fatalf("lazy repo reported as checked out:\n%s", prompt)
	}
}

func TestBuildIncludesLocalCompletionProtocol(t *testing.T) {
	root := t.TempDir()
	packageDir := filepath.Join(root, "package")
	if err := os.MkdirAll(packageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	box := &capsule.Capsule{
		Root: root, PackageDir: packageDir,
		WorkspaceDir: filepath.Join(root, "workspace"),
		ArtifactsDir: filepath.Join(root, "artifacts", "output"),
	}
	prompt, err := Build(box, BuildWork{
		LocalAPIEndpoint:        "http://127.0.0.1:4567",
		DispatchID:              42,
		StepID:                  "700001",
		StepAttempt:             2,
		ArtifactProtocolVersion: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"http://127.0.0.1:4567/dispatches/42/step-events",
		"completion_requested",
		"evidenceRefs",
		"HTTP 202",
		`"stepAttempt":2`,
	} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("prompt missing %q:\n%s", expected, prompt)
		}
	}
}
