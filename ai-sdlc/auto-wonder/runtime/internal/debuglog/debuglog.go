// Package debuglog records one dispatch's runtime diagnostics and uploads them when DEBUG_LOG_V1 is negotiated.
package debuglog

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/protocol"
)

// Recorder appends bounded, timestamped diagnostic lines.
type Recorder struct {
	mu        sync.Mutex
	file      *os.File
	path      string
	maxBytes  int64
	written   int64
	truncated bool
	secrets   []string
}

// New creates a recorder under dir. maxBytes <= 0 falls back to 5 MiB.
func New(dir string, maxBytes int64) (*Recorder, error) {
	if maxBytes <= 0 {
		maxBytes = 5 * 1024 * 1024
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create debug log dir: %w", err)
	}
	path := filepath.Join(dir, "runtime-debug.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create debug log: %w", err)
	}
	return &Recorder{file: file, path: path, maxBytes: maxBytes}, nil
}

// AddSecret registers a value that must never appear in an uploaded log.
func (r *Recorder) AddSecret(value string) {
	if r == nil || len(value) < 8 {
		return
	}
	r.mu.Lock()
	for _, existing := range r.secrets {
		if existing == value {
			r.mu.Unlock()
			return
		}
	}
	r.secrets = append(r.secrets, value)
	r.mu.Unlock()
}

// Printf appends one line, dropping lines that would exceed the negotiated cap.
func (r *Recorder) Printf(format string, args ...any) {
	if r == nil {
		return
	}
	message := fmt.Sprintf(format, args...)
	r.mu.Lock()
	for _, secret := range r.secrets {
		message = strings.ReplaceAll(message, secret, "***")
	}
	line := fmt.Sprintf("%s %s\n", time.Now().UTC().Format(time.RFC3339), message)
	defer r.mu.Unlock()
	if r.written+int64(len(line)) > r.maxBytes {
		r.truncated = true
		return
	}
	if _, err := r.file.WriteString(line); err == nil {
		r.written += int64(len(line))
	}
}

// Close flushes and closes the log file.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.file.Close()
}

// Upload gzips the log, requests a direct upload URL and reports the outcome.
func (r *Recorder) Upload(ctx context.Context, baseURL, token string, dispatchID int64, status string,
	relayFallback bool, artifactPath string) *protocol.DebugLogReport {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	r.file.Sync()
	raw, err := os.ReadFile(r.path)
	truncated := r.truncated
	r.mu.Unlock()
	if err != nil {
		return &protocol.DebugLogReport{Status: "FAILED", Error: err.Error(), Truncated: truncated}
	}
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	if _, err := gzipWriter.Write(raw); err != nil {
		return &protocol.DebugLogReport{Status: "FAILED", Error: err.Error(), Truncated: truncated}
	}
	if err := gzipWriter.Close(); err != nil {
		return &protocol.DebugLogReport{Status: "FAILED", Error: err.Error(), Truncated: truncated}
	}
	payload := compressed.Bytes()
	digest := sha256.Sum256(payload)
	sha := hex.EncodeToString(digest[:])
	report := &protocol.DebugLogReport{Status: "FAILED", SizeBytes: int64(len(payload)), SHA256: sha,
		Truncated: truncated}
	endpoint := strings.TrimRight(baseURL, "/") + fmt.Sprintf("/api/daemon/dispatches/%d/debug-log-upload", dispatchID)
	directErr := uploadDirect(ctx, endpoint, token, payload, sha, truncated, status)
	if directErr == nil {
		report.Status = "UPLOADED"
		report.Channel = "DIRECT"
		report.Error = ""
		return report
	}
	report.Error = directErr.Error()
	if relayFallback && artifactPath != "" {
		if err := uploadRelay(ctx, artifactPath, token, fmt.Sprintf("debug/%d.log.gz", dispatchID), payload, sha); err == nil {
			report.Status = "UPLOADED"
			report.Channel = "RELAY"
			report.Error = ""
		}
	}
	return report
}

func uploadDirect(ctx context.Context, endpoint, token string, payload []byte, sha string, truncated bool,
	status string) error {
	target, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	query := target.Query()
	query.Set("token", token)
	target.RawQuery = query.Encode()
	body, err := json.Marshal(map[string]any{
		"sizeBytes": len(payload), "sha256": sha, "truncated": truncated, "dispatchStatus": status,
	})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: time.Minute}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("request debug log upload: HTTP %d %s", response.StatusCode, strings.TrimSpace(string(raw)))
	}
	var receipt struct {
		UploadURL       string `json:"uploadUrl"`
		AlreadyUploaded bool   `json:"alreadyUploaded"`
	}
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return fmt.Errorf("decode debug log upload response: %w", err)
	}
	if receipt.AlreadyUploaded {
		return nil
	}
	if receipt.UploadURL == "" {
		return fmt.Errorf("debug log upload response has no uploadUrl")
	}
	put, err := http.NewRequestWithContext(ctx, http.MethodPut, receipt.UploadURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	put.Header.Set("Content-Type", "application/gzip")
	putResponse, err := (&http.Client{Timeout: 2 * time.Minute}).Do(put)
	if err != nil {
		return err
	}
	defer putResponse.Body.Close()
	if putResponse.StatusCode/100 != 2 {
		return fmt.Errorf("upload debug log object: HTTP %d", putResponse.StatusCode)
	}
	return nil
}

func uploadRelay(ctx context.Context, endpoint, token, name string, payload []byte, sha string) error {
	target, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	query := target.Query()
	query.Set("token", token)
	target.RawQuery = query.Encode()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("files", filepath.Base(name))
	if err != nil {
		return err
	}
	if _, err := part.Write(payload); err != nil {
		return err
	}
	metadata, _ := json.Marshal([]map[string]any{{"name": name, "sizeBytes": len(payload), "sha256": sha}})
	_ = writer.WriteField("filesMetadata", string(metadata))
	if err := writer.Close(); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), body)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := (&http.Client{Timeout: 2 * time.Minute}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return fmt.Errorf("relay debug log: HTTP %d %s", response.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}
