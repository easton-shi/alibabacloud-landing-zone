package upload

import (
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
