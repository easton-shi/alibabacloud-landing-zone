package config

import (
	"strings"
	"testing"
)

func TestEndpointAcceptsBaseOrFullWebSocketURL(t *testing.T) {
	cfg := Config{
		ServerWSURL:   "wss://autowonder.example.com/ws/executor/",
		ExecutorID:    "10001",
		ExecutorToken: "token with spaces",
		MaxConcurrent: 4,
	}
	endpoint := cfg.Endpoint()
	if !strings.HasPrefix(endpoint, "wss://autowonder.example.com/ws/executor?") {
		t.Fatalf("endpoint = %s", endpoint)
	}
	if strings.Contains(endpoint, "/ws/executor/ws/executor") {
		t.Fatalf("endpoint duplicated path: %s", endpoint)
	}
	if !strings.Contains(endpoint, "token=token+with+spaces") {
		t.Fatalf("token was not query-escaped: %s", endpoint)
	}
}

func TestEnvListTrimsAndDropsEmptyEntries(t *testing.T) {
	t.Setenv("AUTOWONDER_TEST_HOSTS", " files.example, *.aliyuncs.com, , cdn.example ")
	got := envList("AUTOWONDER_TEST_HOSTS")
	want := []string{"files.example", "*.aliyuncs.com", "cdn.example"}
	if len(got) != len(want) {
		t.Fatalf("env list = %v", got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("env list = %v, want %v", got, want)
		}
	}
}

func TestLoadParsesWorkspaceGCConfig(t *testing.T) {
	t.Setenv("AUTOWONDER_SERVER_WS_URL", "wss://autowonder.example")
	t.Setenv("AUTOWONDER_EXECUTOR_ID", "7")
	t.Setenv("AUTOWONDER_EXECUTOR_TOKEN", "token")
	t.Setenv("AUTOWONDER_WORKSPACE_GC_ENABLED", "true")
	t.Setenv("AUTOWONDER_WORKSPACE_GC_INITIAL_DELAY", "45s")
	t.Setenv("AUTOWONDER_WORKSPACE_GC_INTERVAL", "20m")
	t.Setenv("AUTOWONDER_WORKSPACE_GC_TASK_TTL", "24h")
	t.Setenv("AUTOWONDER_WORKSPACE_GC_CONVERSATION_TTL", "240h")
	cfg, err := load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.WorkspaceGCEnabled || cfg.WorkspaceGCInitialDelay.String() != "45s" ||
		cfg.WorkspaceGCInterval.String() != "20m0s" || cfg.WorkspaceGCTaskTTL.String() != "24h0m0s" ||
		cfg.WorkspaceGCConversationTTL.String() != "240h0m0s" {
		t.Fatalf("workspace GC config = %+v", cfg)
	}
}
