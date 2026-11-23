// Package update implements the release-source side of executor self-update.
//
// AutoWonder's EXECUTOR_UPGRADE frame only carries a target version. A Go
// runtime therefore needs its own artifact source. This package expects:
//
//	<base>/<version>/manifest.json
//	<base>/latest/manifest.json
//
// with a per-platform artifact URL and SHA-256. An optional detached Ed25519
// signature (<manifest>.sig, base64) is verified when a public key is set.
package update

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const maxArtifactBytes = 256 << 20

// Config is the release source configuration.
type Config struct {
	BaseURL   string
	PublicKey string
	Insecure  bool
}

// Artifact describes one platform binary.
type Artifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Manifest is one released runtime version.
type Manifest struct {
	Version   string              `json:"version"`
	Artifacts map[string]Artifact `json:"artifacts"`
}

// Receipt survives the process replace and tells the new process which upgrade
// request to report.
type Receipt struct {
	SchemaVersion string    `json:"schemaVersion"`
	RequestID     string    `json:"requestId"`
	ReceiptID     string    `json:"receiptId"`
	TargetVersion string    `json:"targetVersion"`
	Phase         string    `json:"phase"`
	Error         string    `json:"error,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
}

// Updater downloads and atomically installs a release artifact.
type Updater struct {
	Config
	CurrentVersion string
	Executable     string
	ReceiptPath    string
	Client         *http.Client
}

// New builds an updater. It returns nil when no release source is configured.
func New(cfg Config, currentVersion, executable, receiptPath string) *Updater {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil
	}
	return &Updater{
		Config:         cfg,
		CurrentVersion: currentVersion,
		Executable:     executable,
		ReceiptPath:    receiptPath,
		Client:         &http.Client{Timeout: 10 * time.Minute},
	}
}

// PlatformKey returns the artifact key used by manifests, for example
// darwin-arm64 or linux-amd64.
func PlatformKey() string { return runtime.GOOS + "-" + runtime.GOARCH }

// SupportsSelfUpdate reports whether this host has a release source and can
// safely replace the running executable in place.
func (u *Updater) SupportsSelfUpdate() bool {
	return u != nil && strings.TrimSpace(u.BaseURL) != "" && runtime.GOOS != "windows" &&
		(u.Insecure || strings.TrimSpace(u.PublicKey) != "")
}

// LatestVersion resolves the version advertised under /latest.
func (u *Updater) LatestVersion(ctx context.Context) (string, error) {
	manifest, err := u.fetchManifest(ctx, "latest")
	if err != nil {
		return "", err
	}
	return manifest.Version, nil
}

// Download fetches and verifies the artifact for this platform. It returns the
// path of a temporary file in the executable's directory so Replace can rename
// it atomically.
func (u *Updater) Download(ctx context.Context, version string) (string, error) {
	manifest, err := u.fetchManifest(ctx, version)
	if err != nil {
		return "", err
	}
	if manifest.Version != version {
		return "", fmt.Errorf("update manifest version %q does not match requested %q", manifest.Version, version)
	}
	artifact, ok := manifest.Artifacts[PlatformKey()]
	if !ok || artifact.URL == "" {
		return "", fmt.Errorf("update manifest has no artifact for %s", PlatformKey())
	}
	if artifact.SHA256 == "" {
		return "", fmt.Errorf("update artifact has no sha256")
	}
	artifactURL, err := u.resolve(version, artifact.URL)
	if err != nil {
		return "", err
	}
	temp, err := os.CreateTemp(filepath.Dir(u.Executable), ".autowonder-runtime-update-*")
	if err != nil {
		return "", fmt.Errorf("create update temp file: %w", err)
	}
	path := temp.Name()
	cleanup := true
	defer func() {
		_ = temp.Close()
		if cleanup {
			_ = os.Remove(path)
		}
	}()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, artifactURL, nil)
	if err != nil {
		return "", fmt.Errorf("build update request: %w", err)
	}
	response, err := u.httpClient().Do(request)
	if err != nil {
		return "", fmt.Errorf("download update: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download update: HTTP %d", response.StatusCode)
	}
	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(temp, hasher), io.LimitReader(response.Body, maxArtifactBytes+1))
	if err != nil {
		return "", fmt.Errorf("write update: %w", err)
	}
	if written > maxArtifactBytes {
		return "", fmt.Errorf("update artifact exceeds %d bytes", maxArtifactBytes)
	}
	if artifact.Size > 0 && written != artifact.Size {
		return "", fmt.Errorf("update size mismatch: got %d want %d", written, artifact.Size)
	}
	actual := hex.EncodeToString(hasher.Sum(nil))
	expected := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(artifact.SHA256)), "sha256:")
	if !strings.EqualFold(actual, expected) {
		return "", fmt.Errorf("update sha256 mismatch: got %s want %s", actual, expected)
	}
	if err := temp.Sync(); err != nil {
		return "", fmt.Errorf("sync update: %w", err)
	}
	if err := temp.Chmod(0o755); err != nil {
		return "", fmt.Errorf("chmod update: %w", err)
	}
	cleanup = false
	return path, nil
}

// VerifyCandidate runs the downloaded executable's version command before it
// replaces the active binary.
func (u *Updater) VerifyCandidate(path, expectedVersion string) error {
	if path == "" || expectedVersion == "" {
		return fmt.Errorf("candidate path and expected version are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("candidate self-check: %w: %s", err, strings.TrimSpace(string(output)))
	}
	for _, field := range strings.Fields(string(output)) {
		if CompareVersions(field, expectedVersion) == 0 {
			return nil
		}
	}
	return fmt.Errorf("candidate self-check version mismatch: got %q want %q",
		strings.TrimSpace(string(output)), expectedVersion)
}

// RecoverPending rolls back a replacement if the new process started with a
// version older than the requested target.
func (u *Updater) RecoverPending() (bool, error) {
	receipt, err := u.ReadReceipt()
	if err != nil || receipt == nil || receipt.Phase != "restarting" {
		return false, err
	}
	if receipt.CreatedAt.IsZero() || time.Since(receipt.CreatedAt) > 24*time.Hour {
		return false, u.ClearReceipt()
	}
	if VersionAtLeast(u.CurrentVersion, receipt.TargetVersion) {
		return false, nil
	}
	if err := u.Rollback(); err != nil {
		receipt.Phase = "rollback_failed"
		receipt.Error = err.Error()
		_ = u.WriteReceipt(*receipt)
		return false, fmt.Errorf("rollback pending update: %w", err)
	}
	receipt.Phase = "rolled_back"
	receipt.Error = fmt.Sprintf("started version %s, expected %s", u.CurrentVersion, receipt.TargetVersion)
	if err := u.WriteReceipt(*receipt); err != nil {
		return true, err
	}
	return true, nil
}

// Replace installs newPath over the running executable and keeps a backup for
// rollback. Renaming over a running binary is safe on Unix; Windows refuses.
func (u *Updater) Replace(newPath string) error {
	if runtime.GOOS == "windows" {
		return fmt.Errorf("in-place self-update is not supported on Windows")
	}
	if u.Executable == "" {
		return fmt.Errorf("executable path is unavailable")
	}
	backup := u.Executable + ".bak"
	if err := copyFile(u.Executable, backup); err != nil {
		return fmt.Errorf("backup current runtime: %w", err)
	}
	if err := os.Rename(newPath, u.Executable); err != nil {
		_ = copyFile(backup, u.Executable)
		return fmt.Errorf("replace runtime: %w", err)
	}
	return nil
}

// Rollback restores the backup created by Replace.
func (u *Updater) Rollback() error {
	return copyFile(u.Executable+".bak", u.Executable)
}

// WriteReceipt persists the pre-restart state atomically.
func (u *Updater) WriteReceipt(receipt Receipt) error {
	receipt.SchemaVersion = "autowonder.upgradeReceipt.v1"
	if receipt.CreatedAt.IsZero() {
		receipt.CreatedAt = time.Now().UTC()
	}
	raw, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(u.ReceiptPath), 0o755); err != nil {
		return err
	}
	temp := u.ReceiptPath + ".tmp"
	if err := os.WriteFile(temp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temp, u.ReceiptPath)
}

// ReadReceipt loads a pending receipt, or nil when none exists.
func (u *Updater) ReadReceipt() (*Receipt, error) {
	raw, err := os.ReadFile(u.ReceiptPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var receipt Receipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return nil, err
	}
	return &receipt, nil
}

// ClearReceipt removes the pending receipt.
func (u *Updater) ClearReceipt() error {
	err := os.Remove(u.ReceiptPath)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (u *Updater) fetchManifest(ctx context.Context, version string) (*Manifest, error) {
	endpoint, err := u.resolve(version, "manifest.json")
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build manifest request: %w", err)
	}
	response, err := u.httpClient().Do(request)
	if err != nil {
		return nil, fmt.Errorf("download update manifest: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download update manifest: HTTP %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read update manifest: %w", err)
	}
	if u.PublicKey != "" {
		if err := u.verifySignature(ctx, endpoint+".sig", raw); err != nil {
			return nil, err
		}
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("parse update manifest: %w", err)
	}
	if strings.TrimSpace(manifest.Version) == "" {
		return nil, fmt.Errorf("update manifest has no version")
	}
	return &manifest, nil
}

func (u *Updater) verifySignature(ctx context.Context, signatureURL string, manifest []byte) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, signatureURL, nil)
	if err != nil {
		return fmt.Errorf("build signature request: %w", err)
	}
	response, err := u.httpClient().Do(request)
	if err != nil {
		return fmt.Errorf("download update signature: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download update signature: HTTP %d", response.StatusCode)
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return fmt.Errorf("read update signature: %w", err)
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		return fmt.Errorf("update signature is not base64: %w", err)
	}
	publicKeyDER, err := base64.StdEncoding.DecodeString(strings.TrimSpace(u.PublicKey))
	if err != nil {
		return fmt.Errorf("update public key is not base64: %w", err)
	}
	parsed, err := x509.ParsePKIXPublicKey(publicKeyDER)
	if err != nil {
		return fmt.Errorf("update public key is not PKIX: %w", err)
	}
	publicKey, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return fmt.Errorf("update public key is not Ed25519")
	}
	if !ed25519.Verify(publicKey, manifest, signature) {
		return fmt.Errorf("update manifest signature mismatch")
	}
	return nil
}

func (u *Updater) resolve(version, name string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(u.BaseURL), "/")
	if !u.Insecure && !strings.HasPrefix(strings.ToLower(base), "https://") {
		return "", fmt.Errorf("update base URL must use https (set AUTOWONDER_UPDATE_INSECURE=true for local testing)")
	}
	ref := base + "/" + url.PathEscape(version) + "/" + strings.TrimLeft(name, "/")
	if parsed, err := url.Parse(name); err == nil && parsed.IsAbs() {
		ref = name
		if !u.Insecure && !strings.EqualFold(parsed.Scheme, "https") {
			return "", fmt.Errorf("update artifact URL must use https")
		}
	}
	return ref, nil
}

func (u *Updater) httpClient() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	return http.DefaultClient
}

func copyFile(source, target string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	output, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

// CompareVersions compares dotted numeric versions. A non-numeric component
// (including the development build "dev") sorts below a numeric one.
func CompareVersions(a, b string) int {
	parse := func(value string) []int {
		value = strings.TrimPrefix(strings.TrimSpace(value), "v")
		parts := strings.Split(value, ".")
		result := make([]int, len(parts))
		for index, part := range parts {
			number := 0
			for _, r := range part {
				if r < '0' || r > '9' {
					return []int{-1}
				}
				number = number*10 + int(r-'0')
			}
			result[index] = number
		}
		return result
	}
	left, right := parse(a), parse(b)
	for index := 0; index < len(left) || index < len(right); index++ {
		var l, r int
		if index < len(left) {
			l = left[index]
		}
		if index < len(right) {
			r = right[index]
		}
		if l < r {
			return -1
		}
		if l > r {
			return 1
		}
	}
	return 0
}

// VersionAtLeast reports whether current is not older than target.
func VersionAtLeast(current, target string) bool {
	return CompareVersions(current, target) >= 0
}
