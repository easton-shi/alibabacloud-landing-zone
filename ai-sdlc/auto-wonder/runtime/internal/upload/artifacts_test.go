package upload

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCollectUsesArtifactOutputNames(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "deliverables"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deliverables", "report.md"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, metadata, err := collect(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || len(metadata) != 1 {
		t.Fatalf("files=%d metadata=%d", len(files), len(metadata))
	}
	if metadata[0].Name != "artifacts/output/deliverables/report.md" {
		t.Fatalf("name = %q", metadata[0].Name)
	}
	if metadata[0].SizeBytes != 2 || metadata[0].SHA256 == "" {
		t.Fatalf("metadata = %+v", metadata[0])
	}
}

func TestArtifactsSelectedUploadsOnlyReferencedFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "evidence"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "evidence", "report.md"), []byte("proof"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "unreferenced.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	var names []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, header := range r.MultipartForm.File["files"] {
			names = append(names, header.Filename)
		}
		var metadata []fileMetadata
		if err := json.Unmarshal([]byte(r.FormValue("filesMetadata")), &metadata); err != nil {
			t.Errorf("decode metadata: %v", err)
		}
		if len(metadata) != 1 || metadata[0].Name != "artifacts/output/evidence/report.md" {
			t.Errorf("metadata = %+v", metadata)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	count, err := ArtifactsSelected(context.Background(), server.URL, "token", root,
		[]string{"evidence/report.md", "evidence/report.md"})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || len(names) != 1 || names[0] != "report.md" {
		t.Fatalf("count=%d names=%v", count, names)
	}
}

func TestArtifactsSelectedRejectsEscapingRefs(t *testing.T) {
	root := t.TempDir()
	for _, ref := range []string{"../secret", "/etc/passwd", "artifacts/output/report.md"} {
		if _, err := ArtifactsSelected(context.Background(), "http://example.invalid", "token", root,
			[]string{ref}); err == nil {
			t.Fatalf("unsafe ref %q was accepted", ref)
		}
	}
}
