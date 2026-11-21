package conversation

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var attachmentLine = regexp.MustCompile(`(?m)^\s*\d+\.\s+(.+?)\s+\(([^,]+),\s*(\d+)\s+bytes\)\s*\n\s*URL:\s*(\S+)\s*$`)

type attachment struct {
	Name string
	Type string
	Size int64
	URL  string
}

const (
	maxAttachmentBytes = 100 << 20
	maxAttachments     = 50
)

// downloadAttachments materializes the attachment manifest the server embeds
// in the conversation content. Failures are reported to the agent as notes,
// not as a turn failure: the user must still get an answer that explains which
// file is unavailable.
func downloadAttachments(ctx context.Context, content, workspaceDir string, turnID int64) string {
	items := parseAttachments(content)
	if len(items) == 0 {
		return ""
	}
	dir := filepath.Join(workspaceDir, "attachments", strconv.FormatInt(turnID, 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Sprintf("Runtime could not create the attachment directory %s: %v", dir, err)
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	var saved, failed []string
	for index, item := range items {
		if index >= maxAttachments {
			failed = append(failed, "more than 50 attachments; remaining files were not downloaded")
			break
		}
		parsed, err := url.Parse(item.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			failed = append(failed, item.Name+": invalid attachment URL")
			continue
		}
		name := safeAttachmentName(item.Name)
		if name == "" {
			name = fmt.Sprintf("attachment-%d", index+1)
		}
		path := filepath.Join(dir, name)
		if err := fetchAttachment(ctx, client, item.URL, path, item.Size); err != nil {
			_ = os.Remove(path)
			failed = append(failed, item.Name+": "+err.Error())
			continue
		}
		saved = append(saved, path)
	}
	var b strings.Builder
	b.WriteString("Runtime attachment note: the files listed above were processed before this turn.\n")
	if len(saved) > 0 {
		b.WriteString("Downloaded to " + dir + ":\n")
		for _, path := range saved {
			b.WriteString("- " + path + "\n")
		}
		b.WriteString("Use these local copies instead of fetching the URLs again.\n")
	}
	if len(failed) > 0 {
		b.WriteString("Could not fetch:\n")
		for _, item := range failed {
			b.WriteString("- " + item + "\n")
		}
		b.WriteString("Tell the user exactly which file was unavailable.\n")
	}
	return b.String()
}

func parseAttachments(content string) []attachment {
	matches := attachmentLine.FindAllStringSubmatch(content, maxAttachments+1)
	items := make([]attachment, 0, len(matches))
	for _, match := range matches {
		size, _ := strconv.ParseInt(match[3], 10, 64)
		items = append(items, attachment{
			Name: strings.TrimSpace(match[1]),
			Type: strings.TrimSpace(match[2]),
			Size: size,
			URL:  match[4],
		})
	}
	return items
}

func safeAttachmentName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")
	name = filepath.Base(name)
	name = strings.Trim(name, ". ")
	if len(name) > 180 {
		ext := filepath.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		name = name[:180-len(ext)] + ext
	}
	if name == "" || name == "." || name == ".." {
		return ""
	}
	return name
}

func fetchAttachment(ctx context.Context, client *http.Client, endpoint, path string, expected int64) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	written, err := io.Copy(file, io.LimitReader(response.Body, maxAttachmentBytes+1))
	closeErr := file.Close()
	if err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close file: %w", closeErr)
	}
	if written > maxAttachmentBytes {
		return fmt.Errorf("exceeds %d bytes", maxAttachmentBytes)
	}
	if expected > 0 && written != expected {
		return fmt.Errorf("size mismatch: got %d want %d", written, expected)
	}
	return nil
}
