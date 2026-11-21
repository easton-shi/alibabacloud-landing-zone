package checkpoint

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestArchiveAndDecodeRoundTrip(t *testing.T) {
	document := Document{Repos: []RepoState{{
		Name: "yami-agent", BaseCommit: "0123456789012345678901234567890123456789",
		HeadCommit: "1123456789012345678901234567890123456789",
	}}}
	archive, digest, err := Archive(document)
	if err != nil {
		t.Fatal(err)
	}
	if digest == "" || len(archive) == 0 {
		t.Fatal("empty archive")
	}
	decoded, err := Decode(archive)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != SchemaVersion || len(decoded.Repos) != 1 ||
		decoded.Repos[0].HeadCommit != document.Repos[0].HeadCommit {
		t.Fatalf("unexpected decoded checkpoint: %+v", decoded)
	}
}

func TestUploadSendsMultipartFieldsAndToken(t *testing.T) {
	var receivedSeq string
	var receivedPurpose string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "dispatch-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err := r.ParseMultipartForm(2 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		receivedSeq = r.FormValue("checkpointSeq")
		receivedPurpose = r.FormValue("purpose")
		_ = json.NewEncoder(w).Encode(Receipt{CheckpointSeq: 3, SHA256: "sha256:abc", SizeBytes: 10})
	}))
	defer server.Close()
	receipt, err := Upload(context.Background(), server.URL, "dispatch-token", 1, 3,
		"codex", "session", "runtime", "7", "RECOVERY", "", []byte("archive"), "sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.CheckpointSeq != 3 || receivedSeq != "3" || receivedPurpose != "RECOVERY" {
		t.Fatalf("unexpected upload: receipt=%+v seq=%s purpose=%s", receipt, receivedSeq, receivedPurpose)
	}
}
