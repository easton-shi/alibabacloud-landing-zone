package memory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFetchSnapshotRendersMarkdownAndWritesFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dispatch-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"stores":[
			{"storeId":3,"scope":"AGENT","ownerRef":"7","currentRevision":4,
			 "documents":[{"path":"coding.md","contentMd":"Use tabs", "version":1}]},
			{"storeId":4,"scope":"AGENT","ownerRef":8,"currentRevision":2,
			 "documents":[{"path":"review.md","contentMd":"Check tests", "version":2}]}
		]}`))
	}))
	defer server.Close()
	markdown, err := FetchSnapshot(context.Background(), server.URL, "dispatch-token")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(markdown, "coding.md") || !strings.Contains(markdown, "Use tabs") ||
		!strings.Contains(markdown, "AGENT:7") || !strings.Contains(markdown, "AGENT:8") ||
		!strings.Contains(markdown, "review.md") || !strings.Contains(markdown, "Check tests") {
		t.Fatalf("unexpected markdown: %s", markdown)
	}
	dir := t.TempDir()
	if err := WriteSnapshot(dir, markdown); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "memory", "platform-snapshot.md")); err != nil {
		t.Fatal(err)
	}
}
