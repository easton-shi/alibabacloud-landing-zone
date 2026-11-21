package mcpconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteMergesBuiltinAndCapabilityServers(t *testing.T) {
	packageDir := t.TempDir()
	raw := `{
  "mcpServers": [
    {"name":"autowonder","transport":"http","url":"https://ignored/api/mcp","authType":"autowonder-dispatch"},
    {"name":"search","transport":"http","url":"https://search.example/mcp",
     "headerSecretRefs":{"Authorization":"ref-1"}},
    {"name":"local-tools","transport":"stdio","command":"local-mcp","args":["--stdio"],
     "envSecretRefs":{"API_KEY":"ref-2"}}
  ]
}`
	if err := os.WriteFile(filepath.Join(packageDir, "skills.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := Write(packageDir, "https://autowonder.example", "dispatch-token",
		map[string]string{"ref-1": "Bearer secret", "ref-2": "env-secret"},
		filepath.Join(t.TempDir(), "mcp.config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatal("expected config path")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(content, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.MCPServers) != 3 {
		t.Fatalf("servers = %d", len(document.MCPServers))
	}
	builtin := document.MCPServers["autowonder"]
	if builtin["url"] != "https://autowonder.example/api/mcp" {
		t.Fatalf("builtin = %+v", builtin)
	}
	searchHeaders, _ := document.MCPServers["search"]["headers"].(map[string]any)
	if searchHeaders["Authorization"] != "Bearer secret" {
		t.Fatalf("search headers = %+v", searchHeaders)
	}
	localEnv, _ := document.MCPServers["local-tools"]["env"].(map[string]any)
	if localEnv["API_KEY"] != "env-secret" {
		t.Fatalf("local env = %+v", localEnv)
	}
}

func TestWriteRejectsMissingSecretReference(t *testing.T) {
	packageDir := t.TempDir()
	raw := `{"mcpServers":[{"name":"search","transport":"http","url":"https://example/mcp","headerSecretRefs":{"Authorization":"missing"}}]}`
	if err := os.WriteFile(filepath.Join(packageDir, "skills.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(packageDir, "https://autowonder.example", "token", nil, filepath.Join(t.TempDir(), "mcp.json")); err == nil {
		t.Fatal("expected missing secret to fail")
	}
}
