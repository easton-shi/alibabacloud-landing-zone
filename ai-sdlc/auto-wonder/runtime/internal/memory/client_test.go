package memory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientUsesDispatchTokenAndQuery(t *testing.T) {
	var authorization, query, body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		query = r.URL.RawQuery
		raw := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(raw)
		body = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"storeId":7,"currentRevision":3,"changes":[],"hasMore":false}`))
	}))
	defer server.Close()

	client := New(server.URL, "awdispatch_token", 42)
	raw, status, err := client.Changes(context.Background(), "/api/daemon/dispatches/42/memory/changes", 7, 2, 50)
	if err != nil || status != 200 || !strings.Contains(string(raw), "currentRevision") {
		t.Fatalf("unexpected response: %d %v %s", status, err, raw)
	}
	if authorization != "Bearer awdispatch_token" {
		t.Fatalf("unexpected authorization: %q", authorization)
	}
	if !strings.Contains(query, "storeId=7") || !strings.Contains(query, "afterRevision=2") ||
		!strings.Contains(query, "limit=50") {
		t.Fatalf("unexpected query: %q", query)
	}

	_, _, err = client.Mutate(context.Background(), "/api/daemon/dispatches/42/memory/mutations", 7,
		[]byte(`{"operation":"CREATE","path":"m.md","contentMd":"x"}`))
	if err != nil || !strings.Contains(query, "storeId=7") || !strings.Contains(body, `"CREATE"`) {
		t.Fatalf("mutation not forwarded: %q %q %v", query, body, err)
	}
}

func TestClientPropagatesUpstreamStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusLocked)
		_, _ = w.Write([]byte(`{"error":"MEMORY_MAINTENANCE_LOCKED"}`))
	}))
	defer server.Close()
	client := New(server.URL, "token", 1)
	raw, status, err := client.Mutate(context.Background(), "/mutations", 9, []byte(`{}`))
	if err == nil || status != http.StatusLocked || !strings.Contains(string(raw), "LOCKED") {
		t.Fatalf("unexpected status: %d %v %s", status, err, raw)
	}
}

func TestMaintenanceEndpoint(t *testing.T) {
	got := MaintenanceEndpoint("/api/daemon/dispatches/42/memory/snapshot", "claims")
	if got != "/api/daemon/dispatches/42/memory/maintenance/claims" {
		t.Fatalf("unexpected endpoint: %s", got)
	}
}

func TestSnapshotIncludesComposedIndex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"composedIndex":"- [Deploy](deploy.md)","stores":[{"id":1,"scope":"AGENT","currentRevision":2,"documents":[]}]}`))
	}))
	defer server.Close()
	markdown, err := FetchSnapshot(context.Background(), server.URL+"/snapshot", "token")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(markdown, "Platform memory index") || !strings.Contains(markdown, "deploy.md") {
		t.Fatalf("composed index missing: %q", markdown)
	}
}
