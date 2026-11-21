// Package upload pushes deliverables through the daemon artifact relay.
package upload

import (
	"bytes"
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
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
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

	target, err := url.Parse(endpoint)
	if err != nil {
		return 0, err
	}
	query := target.Query()
	query.Set("token", token)
	target.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), &body)
	if err != nil {
		return 0, err
	}
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
