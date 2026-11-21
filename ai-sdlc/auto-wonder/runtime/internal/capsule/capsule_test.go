package capsule

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrepareSignedVerifiesEnvelopeBeforeExtraction(t *testing.T) {
	archive := testZip(t)
	digest := sha256.Sum256(archive)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	keyRef := sha256.Sum256(der)
	signature := Signature{
		PackageID:     "pkg_1",
		ArchiveSHA256: hex.EncodeToString(digest[:]),
		Issuer:        "autowonder-server",
		SignatureRef:  "sha256:" + hex.EncodeToString(keyRef[:]),
		PublicKey:     base64.StdEncoding.EncodeToString(der),
		ExpiresAt:     time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
	}
	payload := "autowonder.taskPackage.signature.v1\n" + signature.Issuer + "\n" + signature.SignatureRef + "\n" +
		signature.ExpiresAt + "\n" + signature.PackageID + "\n" + "sha256:" + signature.ArchiveSHA256 + "\n"
	signature.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(payload)))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archive)
	}))
	defer server.Close()
	root := t.TempDir()
	box, err := PrepareSigned(context.Background(), server.URL, "", root, 42, signature)
	if err != nil {
		t.Fatalf("PrepareSigned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(box.PackageDir, "repos.json")); err != nil {
		t.Fatalf("package was not extracted: %v", err)
	}

	tampered := signature
	tampered.Signature = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	if _, err := PrepareSigned(context.Background(), server.URL, "", t.TempDir(), 43, tampered); err == nil {
		t.Fatal("forged signature was accepted")
	}
}

func TestPrepareSignedRejectsMissingSignatureWhenRequired(t *testing.T) {
	if err := Verify(Signature{RequireSigned: true}, "abc"); err == nil {
		t.Fatal("missing signature was accepted")
	}
}

func testZip(t *testing.T) []byte {
	t.Helper()
	buffer := &bytes.Buffer{}
	writer := zip.NewWriter(buffer)
	entry, err := writer.Create("repos.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(`{"repositories":[]}`)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
