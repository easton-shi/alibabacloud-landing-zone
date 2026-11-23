// Package upload pushes deliverables through the daemon artifact relay.
package upload

import (
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
	"time"
)

type fileMetadata struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
}

type collectedFile struct {
	Path string
	Name string
}

// Artifacts uploads every regular file under dir. Failures are best-effort:
// the dispatch result must not depend on the relay being available.
func Artifacts(ctx context.Context, endpoint, token string, dir string) (int, error) {
	if endpoint == "" || dir == "" {
		return 0, nil
	}
	files, metadata, err := collect(dir)
	if err != nil || len(files) == 0 {
		return 0, err
	}
	return uploadCollected(ctx, endpoint, token, files, metadata)
}

// ArtifactsSelected uploads only explicit output-relative references. It is
// used by the explicit_refs_v1 protocol so unreferenced output files are not
// read or published.
func ArtifactsSelected(ctx context.Context, endpoint, token, dir string, refs []string) (int, error) {
	if endpoint == "" || dir == "" || len(refs) == 0 {
		return 0, nil
	}
	files, metadata, err := collectSelected(dir, refs)
	if err != nil || len(files) == 0 {
		return 0, err
	}
	return uploadCollected(ctx, endpoint, token, files, metadata)
}

func uploadCollected(ctx context.Context, endpoint, token string, files []collectedFile,
	metadata []fileMetadata) (int, error) {
	body, err := os.CreateTemp("", "autowonder-artifact-upload-*.multipart")
	if err != nil {
		return 0, fmt.Errorf("create artifact upload body: %w", err)
	}
	bodyPath := body.Name()
	defer func() {
		_ = body.Close()
		_ = os.Remove(bodyPath)
	}()
	writer := multipart.NewWriter(body)
	for _, file := range files {
		part, err := writer.CreateFormFile("files", filepath.Base(file.Name))
		if err != nil {
			return 0, err
		}
		source, err := os.Open(file.Path)
		if err != nil {
			return 0, err
		}
		_, copyErr := io.Copy(part, source)
		source.Close()
		if copyErr != nil {
			return 0, copyErr
		}
	}
	encoded, _ := json.Marshal(metadata)
	_ = writer.WriteField("filesMetadata", string(encoded))
	if err := writer.Close(); err != nil {
		return 0, err
	}
	if err := body.Sync(); err != nil {
		return 0, err
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	info, err := body.Stat()
	if err != nil {
		return 0, err
	}

	target, err := url.Parse(endpoint)
	if err != nil {
		return 0, err
	}
	query := target.Query()
	query.Set("token", token)
	target.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), body)
	if err != nil {
		return 0, err
	}
	request.ContentLength = info.Size()
	request.Header.Set("Content-Type", writer.FormDataContentType())
	client := &http.Client{Timeout: 10 * time.Minute}
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return 0, fmt.Errorf("artifact upload HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	return len(files), nil
}

func collectSelected(dir string, refs []string) ([]collectedFile, []fileMetadata, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]struct{}{}
	files := make([]collectedFile, 0, len(refs))
	metadata := make([]fileMetadata, 0, len(refs))
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		if strings.HasPrefix(ref, "artifacts/output/") || filepath.IsAbs(ref) {
			return nil, nil, fmt.Errorf("artifact ref %q must be relative to the output directory", ref)
		}
		clean := filepath.Clean(filepath.FromSlash(ref))
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
			return nil, nil, fmt.Errorf("unsafe artifact ref %q", ref)
		}
		if _, duplicate := seen[clean]; duplicate {
			continue
		}
		seen[clean] = struct{}{}
		path := filepath.Join(root, clean)
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			return nil, nil, fmt.Errorf("artifact ref %q escapes the output directory", ref)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, nil, fmt.Errorf("stat artifact %q: %w", ref, err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, nil, fmt.Errorf("artifact ref %q is not a regular file", ref)
		}
		digest, err := fileSHA256(path)
		if err != nil {
			return nil, nil, fmt.Errorf("hash artifact %q: %w", ref, err)
		}
		logical := "artifacts/output/" + filepath.ToSlash(relative)
		files = append(files, collectedFile{Path: path, Name: logical})
		metadata = append(metadata, fileMetadata{Name: logical, SizeBytes: info.Size(), SHA256: digest})
	}
	return files, metadata, nil
}

func collect(dir string) ([]collectedFile, []fileMetadata, error) {
	var files []collectedFile
	var metadata []fileMetadata
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("artifact directory contains symlink: %s", path)
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		name := "artifacts/output/" + filepath.ToSlash(relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		digest, err := fileSHA256(path)
		if err != nil {
			return err
		}
		files = append(files, collectedFile{Path: path, Name: name})
		metadata = append(metadata, fileMetadata{Name: name, SizeBytes: info.Size(), SHA256: digest})
		return nil
	})
	return files, metadata, err
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
