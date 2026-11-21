// Package capsule downloads and unpacks AutoWonder task packages.
package capsule

import (
	"archive/zip"
	"context"
	"crypto/ed25519"
	"crypto/md5"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Capsule is one dispatch workspace.
type Capsule struct {
	Root         string
	PackageDir   string
	WorkspaceDir string
	ArtifactsDir string
}

// Signature is the dispatch envelope that signs the immutable ZIP archive.
type Signature struct {
	PackageID     string
	ArchiveSHA256 string
	Issuer        string
	SignatureRef  string
	Signature     string
	Algorithm     string
	PublicKey     string
	ExpiresAt     string
	RequireSigned bool
}

// Prepare downloads, verifies and extracts a task package.
func Prepare(ctx context.Context, downloadURL, expectedMD5 string, workspaceRoot string, dispatchID int64) (*Capsule, error) {
	return PrepareSigned(ctx, downloadURL, expectedMD5, workspaceRoot, dispatchID, Signature{})
}

// PrepareSigned downloads the archive, verifies MD5, optional SHA-256 and the Ed25519 envelope before extraction.
func PrepareSigned(ctx context.Context, downloadURL, expectedMD5 string, workspaceRoot string, dispatchID int64,
	signature Signature) (*Capsule, error) {
	root := filepath.Join(workspaceRoot, fmt.Sprintf("dispatch-%d", dispatchID))
	c := &Capsule{
		Root:         root,
		PackageDir:   filepath.Join(root, "package"),
		WorkspaceDir: filepath.Join(root, "workspace"),
		ArtifactsDir: filepath.Join(root, "artifacts", "output"),
	}
	for _, dir := range []string{c.PackageDir, c.WorkspaceDir, c.ArtifactsDir, filepath.Join(c.ArtifactsDir, "deliverables")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create capsule dir: %w", err)
		}
	}
	archive := filepath.Join(root, "task-package.zip")
	digest, err := download(ctx, downloadURL, archive, expectedMD5)
	if err != nil {
		return nil, err
	}
	if err := Verify(signature, digest); err != nil {
		return nil, err
	}
	if err := extractZip(archive, c.PackageDir); err != nil {
		return nil, err
	}
	return c, nil
}

// Verify checks the Ed25519 envelope against the actual archive digest.
func Verify(signature Signature, archiveSHA256 string) error {
	if signature.ArchiveSHA256 != "" {
		expected := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(signature.ArchiveSHA256)), "sha256:")
		if !strings.EqualFold(expected, archiveSHA256) {
			return fmt.Errorf("task package sha256 mismatch: got %s want %s", archiveSHA256, expected)
		}
	}
	if signature.Signature == "" && signature.PublicKey == "" {
		if signature.RequireSigned {
			return fmt.Errorf("task package signature is required but the dispatch has none")
		}
		return nil
	}
	if signature.Signature == "" || signature.PublicKey == "" || signature.SignatureRef == "" {
		return fmt.Errorf("task package signature envelope is incomplete")
	}
	algorithm := strings.ToLower(strings.TrimSpace(signature.Algorithm))
	if algorithm != "" && algorithm != "ed25519" {
		return fmt.Errorf("unsupported task package signature algorithm %q", signature.Algorithm)
	}
	if signature.ExpiresAt != "" {
		expiresAt, err := time.Parse(time.RFC3339, signature.ExpiresAt)
		if err != nil {
			return fmt.Errorf("task package signature expiresAt is invalid: %w", err)
		}
		if time.Now().UTC().After(expiresAt) {
			return fmt.Errorf("task package signature expired at %s", signature.ExpiresAt)
		}
	}
	publicKeyDER, err := base64.StdEncoding.DecodeString(signature.PublicKey)
	if err != nil {
		return fmt.Errorf("task package public key is not base64: %w", err)
	}
	parsed, err := x509.ParsePKIXPublicKey(publicKeyDER)
	if err != nil {
		return fmt.Errorf("task package public key is not PKIX: %w", err)
	}
	publicKey, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return fmt.Errorf("task package public key is not Ed25519")
	}
	keyDigest := sha256.Sum256(publicKeyDER)
	expectedRef := "sha256:" + hex.EncodeToString(keyDigest[:])
	if !strings.EqualFold(signature.SignatureRef, expectedRef) {
		return fmt.Errorf("task package signatureRef does not match the public key")
	}
	signatureBytes, err := base64.StdEncoding.DecodeString(signature.Signature)
	if err != nil {
		return fmt.Errorf("task package signature is not base64: %w", err)
	}
	digest := archiveSHA256
	if !strings.HasPrefix(strings.ToLower(digest), "sha256:") {
		digest = "sha256:" + strings.ToLower(digest)
	}
	payload := "autowonder.taskPackage.signature.v1\n" + signature.Issuer + "\n" +
		signature.SignatureRef + "\n" + signature.ExpiresAt + "\n" + signature.PackageID + "\n" + digest + "\n"
	if !ed25519.Verify(publicKey, []byte(payload), signatureBytes) {
		return fmt.Errorf("task package signature verification failed")
	}
	return nil
}

func download(ctx context.Context, url, dest, expectedMD5 string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build download request: %w", err)
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("download task package: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download task package: HTTP %d", response.StatusCode)
	}
	file, err := os.Create(dest)
	if err != nil {
		return "", fmt.Errorf("create task package file: %w", err)
	}
	defer file.Close()
	md5Hasher := md5.New()
	sha256Hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(file, md5Hasher, sha256Hasher), response.Body); err != nil {
		return "", fmt.Errorf("write task package: %w", err)
	}
	actual := hex.EncodeToString(md5Hasher.Sum(nil))
	if expectedMD5 != "" && !strings.EqualFold(actual, expectedMD5) {
		return "", fmt.Errorf("task package md5 mismatch: got %s want %s", actual, expectedMD5)
	}
	return hex.EncodeToString(sha256Hasher.Sum(nil)), nil
}

func extractZip(archive, dest string) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("open task package: %w", err)
	}
	defer reader.Close()
	root, err := filepath.Abs(dest)
	if err != nil {
		return fmt.Errorf("resolve package root: %w", err)
	}
	for _, entry := range reader.File {
		target := filepath.Join(root, filepath.FromSlash(entry.Name))
		clean, err := filepath.Abs(target)
		if err != nil || !strings.HasPrefix(clean, root+string(os.PathSeparator)) && clean != root {
			return fmt.Errorf("task package path escapes root: %q", entry.Name)
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(clean, 0o755); err != nil {
				return fmt.Errorf("create package directory: %w", err)
			}
			continue
		}
		if entry.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("task package contains symlink: %q", entry.Name)
		}
		if err := os.MkdirAll(filepath.Dir(clean), 0o755); err != nil {
			return fmt.Errorf("create package directory: %w", err)
		}
		source, err := entry.Open()
		if err != nil {
			return fmt.Errorf("open package entry %q: %w", entry.Name, err)
		}
		file, err := os.OpenFile(clean, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, entry.Mode().Perm()|0o600)
		if err != nil {
			source.Close()
			return fmt.Errorf("create package entry %q: %w", entry.Name, err)
		}
		_, copyErr := io.Copy(file, source)
		file.Close()
		source.Close()
		if copyErr != nil {
			return fmt.Errorf("extract package entry %q: %w", entry.Name, copyErr)
		}
	}
	return nil
}

// DownloadAndExtract fetches a content-addressed archive and extracts it into
// dest. Conversation capability packages carry a SHA-256 but no Ed25519
// envelope, so they use this narrower entry point instead of PrepareSigned.
func DownloadAndExtract(ctx context.Context, downloadURL, expectedSHA256, archivePath, dest string) error {
	digest, err := download(ctx, downloadURL, archivePath, "")
	if err != nil {
		return err
	}
	expected := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(expectedSHA256)), "sha256:")
	if expected != "" && !strings.EqualFold(digest, expected) {
		return fmt.Errorf("conversation capability sha256 mismatch: got %s want %s", digest, expected)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("create conversation capability dir: %w", err)
	}
	return extractZip(archivePath, dest)
}
