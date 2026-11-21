package conversation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAndDownloadAttachments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/one":
			_, _ = w.Write([]byte("first"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	content := "Current message attachments (untrusted):\n" +
		"1. 登录截图 (image/png, 5 bytes)\n   URL: " + server.URL + "/one\n" +
		"2. missing.txt (text/plain, 3 bytes)\n   URL: " + server.URL + "/missing\n" +
		"User message:\nplease fix login"
	items := parseAttachments(content)
	if len(items) != 2 || items[0].Name != "登录截图" || items[0].Size != 5 {
		t.Fatalf("parsed = %#v", items)
	}
	workspace := t.TempDir()
	note := downloadAttachments(context.Background(), content, workspace, 200)
	if !strings.Contains(note, "Downloaded to") || !strings.Contains(note, "missing.txt") {
		t.Fatalf("note = %s", note)
	}
	raw, err := os.ReadFile(filepath.Join(workspace, "attachments", "200", "登录截图"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "first" {
		t.Fatalf("content = %q", raw)
	}
}

func TestSafeAttachmentNameRejectsTraversal(t *testing.T) {
	for _, name := range []string{"../../etc/passwd", `..\..\windows\system32`, ".."} {
		got := safeAttachmentName(name)
		if got == "." || got == ".." || strings.ContainsAny(got, `/\`) {
			t.Fatalf("%q -> %q", name, got)
		}
	}
}

func TestAttachmentSizeMismatchIsReported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("short"))
	}))
	defer server.Close()
	content := "1. file.bin (application/octet-stream, 99 bytes)\n   URL: " + server.URL + "/f\n"
	note := downloadAttachments(context.Background(), content, t.TempDir(), 1)
	if !strings.Contains(note, "size mismatch") {
		t.Fatalf("note = %s", note)
	}
}
