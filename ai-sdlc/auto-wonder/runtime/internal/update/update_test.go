package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testUpdater(t *testing.T, manifest, artifact []byte, signature []byte) (*Updater, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/0.2.0/manifest.json":
			_, _ = w.Write(manifest)
		case "/0.2.0/manifest.json.sig":
			_, _ = w.Write(signature)
		case "/0.2.0/artifact":
			_, _ = w.Write(artifact)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	executable := filepath.Join(dir, "autowonder-runtime")
	if err := os.WriteFile(executable, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	updater := New(Config{BaseURL: server.URL, Insecure: true}, "0.1.0", executable,
		filepath.Join(dir, "state", "receipt.json"))
	return updater, executable
}

func manifestFor(version string, artifact []byte) []byte {
	sum := sha256.Sum256(artifact)
	return []byte(fmt.Sprintf(`{"version":%q,"artifacts":{"%s":{"url":"artifact","sha256":"%s","size":%d}}}`,
		version, PlatformKey(), hex.EncodeToString(sum[:]), len(artifact)))
}

func TestDownloadAndReplace(t *testing.T) {
	artifact := []byte("new-runtime")
	updater, executable := testUpdater(t, manifestFor("0.2.0", artifact), artifact, nil)
	path, err := updater.Download(context.Background(), "0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(artifact) {
		t.Fatalf("artifact = %q", got)
	}
	if err := updater.Replace(path); err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if string(installed) != string(artifact) {
		t.Fatalf("installed = %q", installed)
	}
	backup, err := os.ReadFile(executable + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != "old" {
		t.Fatalf("backup = %q", backup)
	}
}

func TestDownloadRejectsSHA256Mismatch(t *testing.T) {
	artifact := []byte("new-runtime")
	manifest := []byte(fmt.Sprintf(`{"version":"0.2.0","artifacts":{"%s":{"url":"artifact","sha256":"%s"}}}`,
		PlatformKey(), strings.Repeat("0", 64)))
	updater, _ := testUpdater(t, manifest, artifact, nil)
	if _, err := updater.Download(context.Background(), "0.2.0"); err == nil ||
		!strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("error = %v", err)
	}
}

func TestManifestSignature(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	artifact := []byte("signed-runtime")
	manifest := manifestFor("0.2.0", artifact)
	signature := ed25519.Sign(privateKey, manifest)
	updater, _ := testUpdater(t, manifest, artifact, []byte(base64.StdEncoding.EncodeToString(signature)))
	updater.PublicKey = base64.StdEncoding.EncodeToString(der)
	if _, err := updater.Download(context.Background(), "0.2.0"); err != nil {
		t.Fatal(err)
	}
	updater.PublicKey = base64.StdEncoding.EncodeToString(der)
	updater.Client = &http.Client{Timeout: time.Minute}
	manifest[0] = '['
	updater2, _ := testUpdater(t, manifest, artifact, []byte(base64.StdEncoding.EncodeToString(signature)))
	updater2.PublicKey = base64.StdEncoding.EncodeToString(der)
	if _, err := updater2.Download(context.Background(), "0.2.0"); err == nil ||
		!strings.Contains(err.Error(), "signature") {
		t.Fatalf("tampered manifest error = %v", err)
	}
}

func TestReceiptRoundTrip(t *testing.T) {
	dir := t.TempDir()
	updater := New(Config{BaseURL: "https://example.com", Insecure: false}, "0.1.0",
		filepath.Join(dir, "runtime"), filepath.Join(dir, "state", "receipt.json"))
	receipt := Receipt{RequestID: "r1", ReceiptID: "c1", TargetVersion: "0.2.0", Phase: "restarting"}
	if err := updater.WriteReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	loaded, err := updater.ReadReceipt()
	if err != nil || loaded == nil {
		t.Fatalf("receipt = %#v err=%v", loaded, err)
	}
	if loaded.SchemaVersion != "autowonder.upgradeReceipt.v1" || loaded.RequestID != "r1" {
		t.Fatalf("receipt = %#v", loaded)
	}
	if err := updater.ClearReceipt(); err != nil {
		t.Fatal(err)
	}
	if loaded, err := updater.ReadReceipt(); err != nil || loaded != nil {
		t.Fatalf("receipt after clear = %#v err=%v", loaded, err)
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.2.0", -1},
		{"v1.2.3", "1.2.3", 0},
		{"1.10.0", "1.9.9", 1},
		{"dev", "0.1.0", -1},
		{"1.0", "1.0.0", 0},
	}
	for _, test := range cases {
		if got := CompareVersions(test.a, test.b); got != test.want {
			t.Errorf("CompareVersions(%q,%q) = %d, want %d", test.a, test.b, got, test.want)
		}
	}
}

func TestVerifyCandidateRequiresExpectedVersion(t *testing.T) {
	dir := t.TempDir()
	candidate := filepath.Join(dir, "candidate")
	if err := os.WriteFile(candidate, []byte("#!/bin/sh\necho 'autowonder-runtime 0.2.0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	updater := &Updater{}
	if err := updater.VerifyCandidate(candidate, "0.2.0"); err != nil {
		t.Fatal(err)
	}
	if err := updater.VerifyCandidate(candidate, "0.3.0"); err == nil {
		t.Fatal("version mismatch was accepted")
	}
}

func TestRecoverPendingRollsBackVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "autowonder-runtime")
	if err := os.WriteFile(executable, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable+".bak", []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	updater := New(Config{BaseURL: "https://example.com"}, "0.1.0", executable,
		filepath.Join(dir, "receipt.json"))
	if err := updater.WriteReceipt(Receipt{
		RequestID: "r1", ReceiptID: "c1", TargetVersion: "0.2.0", Phase: "restarting",
	}); err != nil {
		t.Fatal(err)
	}
	rolledBack, err := updater.RecoverPending()
	if err != nil || !rolledBack {
		t.Fatalf("rolledBack=%t err=%v", rolledBack, err)
	}
	installed, err := os.ReadFile(executable)
	if err != nil || string(installed) != "old" {
		t.Fatalf("installed=%q err=%v", installed, err)
	}
	receipt, err := updater.ReadReceipt()
	if err != nil || receipt == nil || receipt.Phase != "rolled_back" {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
}

func TestSupportsSelfUpdateRequiresAuthenticatedReleaseSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("in-place update is intentionally unsupported on Windows")
	}
	if (&Updater{Config: Config{BaseURL: "https://updates.example"}}).SupportsSelfUpdate() {
		t.Fatal("unsigned update source was advertised")
	}
	if !(&Updater{Config: Config{BaseURL: "https://updates.example", PublicKey: "key"}}).SupportsSelfUpdate() {
		t.Fatal("signed update source was not advertised")
	}
}
