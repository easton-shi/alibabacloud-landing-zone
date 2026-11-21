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
