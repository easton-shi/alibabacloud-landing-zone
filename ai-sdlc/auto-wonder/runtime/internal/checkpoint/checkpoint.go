// Package checkpoint implements the durable TASK_PAUSED archive and resume metadata.
package checkpoint

import (
	"archive/tar"
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
	"strconv"
	"strings"
	"time"

	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/repos"
)

const (
	SchemaVersion            = "autowonder.runtimeCheckpoint.v1"
	LightweightSchemaVersion = "autowonder.runtimeCheckpoint.lightweight.v1"
)

// RepoState is the recoverable commit of one repository.
type RepoState struct {
	Name       string `json:"name"`
	BaseCommit string `json:"baseCommit"`
	HeadCommit string `json:"headCommit"`
	Branch     string `json:"branch,omitempty"`
}

// Document is the root checkpoint.json.
type Document struct {
	SchemaVersion string      `json:"schemaVersion"`
	DispatchID    string      `json:"dispatchId,omitempty"`
	CheckpointSeq int64       `json:"checkpointSeq,omitempty"`
	StateSha256   string      `json:"stateSha256,omitempty"`
	Repos         []RepoState `json:"repos,omitempty"`
}

// Receipt is the server response after a checkpoint upload.
type Receipt struct {
	CheckpointSeq int64  `json:"checkpointSeq"`
	SHA256        string `json:"sha256"`
	SizeBytes     int64  `json:"sizeBytes"`
}

// Archive builds the gzip/tar checkpoint and returns its SHA-256.
func Archive(document Document) ([]byte, string, error) {
	if document.SchemaVersion == "" {
		document.SchemaVersion = SchemaVersion
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, "", fmt.Errorf("encode checkpoint: %w", err)
	}
	buffer := &bytes.Buffer{}
	gzipWriter := gzip.NewWriter(buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	header := &tar.Header{Name: "checkpoint.json", Mode: 0o600, Size: int64(len(raw)), ModTime: time.Now().UTC()}
	if err := tarWriter.WriteHeader(header); err != nil {
		return nil, "", fmt.Errorf("write checkpoint header: %w", err)
	}
	if _, err := tarWriter.Write(raw); err != nil {
		return nil, "", fmt.Errorf("write checkpoint: %w", err)
	}
	if err := tarWriter.Close(); err != nil {
		return nil, "", fmt.Errorf("close checkpoint tar: %w", err)
	}
	if err := gzipWriter.Close(); err != nil {
		return nil, "", fmt.Errorf("close checkpoint gzip: %w", err)
	}
	digest := sha256.Sum256(buffer.Bytes())
	return buffer.Bytes(), "sha256:" + hex.EncodeToString(digest[:]), nil
}

// BuildLightweight builds the lightweight v1 checkpoint envelope. It retains
// the same tar/gzip container and checkpoint.json entry as historical v1 so
// readers can distinguish the format by schemaVersion before interpreting it.
func BuildLightweight(document Document) ([]byte, string, error) {
	document.SchemaVersion = LightweightSchemaVersion
	return Archive(document)
}

// FromCheckouts captures the current committed state of every checkout.
func FromCheckouts(ctx context.Context, checkouts []repos.Checkout) (Document, error) {
	document := Document{SchemaVersion: SchemaVersion, Repos: make([]RepoState, 0, len(checkouts))}
	for _, checkout := range checkouts {
		head, err := repos.Head(ctx, checkout.Dir)
		if err != nil {
			return Document{}, err
		}
		branch, _ := repos.Branch(ctx, checkout.Dir)
		document.Repos = append(document.Repos, RepoState{
			Name: checkout.Spec.Name, BaseCommit: checkout.BaseCommit, HeadCommit: head, Branch: branch,
		})
	}
	return document, nil
}

// Upload posts the checkpoint to the daemon endpoint.
func Upload(ctx context.Context, endpoint, token string, dispatchID, seq int64, provider, sessionID, runtimeID,
	activeStepID, purpose, resultID string, archive []byte, archiveSHA256 string) (Receipt, error) {
	target, err := withToken(endpoint, token)
	if err != nil {
		return Receipt{}, err
	}
	body := &bytes.Buffer{}
	form := multipart.NewWriter(body)
	fields := map[string]string{
		"checkpointSeq":     strconv.FormatInt(seq, 10),
		"provider":          provider,
		"providerSessionId": sessionID,
		"runtimeId":         runtimeID,
		"activeStepId":      activeStepID,
		"purpose":           purpose,
	}
	if resultID != "" {
		fields["resultId"] = resultID
	}
	for key, value := range fields {
		if value == "" {
			continue
		}
		if err := form.WriteField(key, value); err != nil {
			return Receipt{}, fmt.Errorf("write checkpoint field %s: %w", key, err)
		}
	}
	part, err := form.CreateFormFile("checkpoint", fmt.Sprintf("dispatch-%d-%d.tar.gz", dispatchID, seq))
	if err != nil {
		return Receipt{}, fmt.Errorf("create checkpoint part: %w", err)
	}
	if _, err := part.Write(archive); err != nil {
		return Receipt{}, fmt.Errorf("write checkpoint archive: %w", err)
	}
	if err := form.Close(); err != nil {
		return Receipt{}, fmt.Errorf("close checkpoint form: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, body)
	if err != nil {
		return Receipt{}, fmt.Errorf("build checkpoint request: %w", err)
	}
	request.Header.Set("Content-Type", form.FormDataContentType())
	request.Header.Set("X-Checkpoint-SHA256", archiveSHA256)
	response, err := (&http.Client{Timeout: 2 * time.Minute}).Do(request)
	if err != nil {
		return Receipt{}, fmt.Errorf("upload checkpoint: %w", err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if response.StatusCode/100 != 2 {
		return Receipt{}, fmt.Errorf("upload checkpoint: HTTP %d %s", response.StatusCode, strings.TrimSpace(string(raw)))
	}
	var receipt Receipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return Receipt{}, fmt.Errorf("decode checkpoint receipt: %w", err)
	}
	return receipt, nil
}

// Download fetches a resume checkpoint and returns each repository's commit.
func Download(ctx context.Context, endpoint, expectedSHA256 string) (Document, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Document{}, fmt.Errorf("build checkpoint download: %w", err)
	}
	response, err := (&http.Client{Timeout: 2 * time.Minute}).Do(request)
	if err != nil {
		return Document{}, fmt.Errorf("download checkpoint: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Document{}, fmt.Errorf("download checkpoint: HTTP %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 64*1024*1024))
	if err != nil {
		return Document{}, fmt.Errorf("read checkpoint: %w", err)
	}
	if expectedSHA256 != "" {
		digest := sha256.Sum256(raw)
		actual := "sha256:" + hex.EncodeToString(digest[:])
		if !strings.EqualFold(strings.TrimPrefix(expectedSHA256, "sha256:"), strings.TrimPrefix(actual, "sha256:")) {
			return Document{}, fmt.Errorf("checkpoint sha256 mismatch: got %s want %s", actual, expectedSHA256)
		}
	}
	return Decode(raw)
}

// Decode extracts and parses checkpoint.json from a gzip/tar archive.
func Decode(archive []byte) (Document, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return Document{}, fmt.Errorf("open checkpoint gzip: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return Document{}, fmt.Errorf("checkpoint.json is missing")
		}
		if err != nil {
			return Document{}, fmt.Errorf("read checkpoint tar: %w", err)
		}
		if header.Name != "checkpoint.json" {
			continue
		}
		if header.Size > 1024*1024 {
			return Document{}, fmt.Errorf("checkpoint.json is too large")
		}
		raw, err := io.ReadAll(io.LimitReader(tarReader, 1024*1024))
		if err != nil {
			return Document{}, fmt.Errorf("read checkpoint.json: %w", err)
		}
		var document Document
		if err := json.Unmarshal(raw, &document); err != nil {
			return Document{}, fmt.Errorf("parse checkpoint.json: %w", err)
		}
		return document, nil
	}
}

// Save writes the archive to the dispatch workspace.
func Save(path string, archive []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create checkpoint dir: %w", err)
	}
	return os.WriteFile(path, archive, 0o600)
}

func withToken(endpoint, token string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid checkpoint endpoint: %w", err)
	}
	query := parsed.Query()
	if token != "" && query.Get("token") == "" {
		query.Set("token", token)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
