// Package memory fetches the platform-managed memory snapshot for a dispatch.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type snapshot struct {
	ComposedIndex string  `json:"composedIndex"`
	Stores        []store `json:"stores"`
}

type store struct {
	ID        int64      `json:"storeId"`
	Scope     string     `json:"scope"`
	OwnerRef  string     `json:"ownerRef"`
	Revision  int64      `json:"currentRevision"`
	Documents []document `json:"documents"`
}

type document struct {
	Path      string `json:"path"`
	ContentMD string `json:"contentMd"`
	Version   int64  `json:"version"`
}

// FetchSnapshot GETs the dispatch snapshot and renders it as Markdown.
func FetchSnapshot(ctx context.Context, endpoint, dispatchToken string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("build memory snapshot request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+dispatchToken)
	response, err := (&http.Client{Timeout: time.Minute}).Do(request)
	if err != nil {
		return "", fmt.Errorf("fetch memory snapshot: %w", err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 8*1024*1024))
	if response.StatusCode/100 != 2 {
		return "", fmt.Errorf("fetch memory snapshot: HTTP %d %s", response.StatusCode, strings.TrimSpace(string(raw)))
	}
	var value snapshot
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("decode memory snapshot: %w", err)
	}
	return render(value), nil
}

// WriteSnapshot stores the rendered snapshot under package/memory.
func WriteSnapshot(packageDir, markdown string) error {
	if strings.TrimSpace(markdown) == "" {
		return nil
	}
	dir := filepath.Join(packageDir, "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create memory dir: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, "platform-snapshot.md"), []byte(markdown), 0o600)
}

func render(value snapshot) string {
	var parts []string
	if strings.TrimSpace(value.ComposedIndex) != "" {
		parts = append(parts, "### Platform memory index\n"+value.ComposedIndex)
	}
	for _, store := range value.Stores {
		if len(store.Documents) == 0 {
			continue
		}
		header := fmt.Sprintf("### Platform store %d (%s", store.ID, store.Scope)
		if store.OwnerRef != "" {
			header += ":" + store.OwnerRef
		}
		header += fmt.Sprintf(", revision %d)", store.Revision)
		parts = append(parts, header)
		for _, document := range store.Documents {
			parts = append(parts, "#### "+document.Path+"\n"+document.ContentMD)
		}
	}
	return strings.Join(parts, "\n\n")
}
