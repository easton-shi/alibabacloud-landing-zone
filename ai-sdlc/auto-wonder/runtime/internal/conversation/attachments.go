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

type attachmentPolicy struct {
	allowedHosts   map[string]struct{}
	allowedDomains []string
}

func newAttachmentPolicy(baseURL string, configured []string) attachmentPolicy {
	policy := attachmentPolicy{
		allowedHosts:   map[string]struct{}{},
		allowedDomains: []string{"aliyuncs.com"},
	}
	if parsed, err := url.Parse(baseURL); err == nil && parsed.Hostname() != "" {
		policy.allowedHosts[strings.ToLower(parsed.Hostname())] = struct{}{}
	}
	for _, raw := range configured {
		host := strings.ToLower(strings.TrimSpace(raw))
		switch {
		case host == "":
			continue
		case strings.HasPrefix(host, "*.") && len(host) > 2:
			policy.allowedDomains = append(policy.allowedDomains, strings.TrimPrefix(host, "*."))
		default:
			policy.allowedHosts[host] = struct{}{}
		}
	}
	return policy
}

func (p attachmentPolicy) validateURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse attachment URL: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("attachment URL scheme %q is not allowed", parsed.Scheme)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("attachment URL credentials are not allowed")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return nil, fmt.Errorf("attachment URL has no host")
	}
	if _, ok := p.allowedHosts[host]; ok {
		return parsed, nil
	}
	for _, domain := range p.allowedDomains {
		if strings.HasSuffix(host, "."+domain) {
			return parsed, nil
		}
	}
	return nil, fmt.Errorf("attachment host %q is not allowed", host)
}

func uniqueAttachmentPath(dir, name string) string {
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path
	}
	extension := filepath.Ext(name)
	stem := strings.TrimSuffix(name, extension)
	for index := 2; ; index++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, index, extension))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

const (
	maxAttachmentBytes = 100 << 20
	maxAttachments     = 50
)

// downloadAttachments materializes the attachment manifest the server embeds
// in the conversation content. Failures are reported to the agent as notes,
// not as a turn failure: the user must still get an answer that explains which
// file is unavailable.
func downloadAttachments(ctx context.Context, content, workspaceDir string, turnID int64,
	policy attachmentPolicy) string {
	items := parseAttachments(content)
	if len(items) == 0 {
		return ""
	}
	dir := filepath.Join(workspaceDir, "attachments", strconv.FormatInt(turnID, 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Sprintf("Runtime could not create the attachment directory %s: %v", dir, err)
	}
	client := &http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(request *http.Request, _ []*http.Request) error {
		if _, err := policy.validateURL(request.URL.String()); err != nil {
			return err
		}
		return nil
	}}
	var saved, failed []string
	for index, item := range items {
		if index >= maxAttachments {
			failed = append(failed, "more than 50 attachments; remaining files were not downloaded")
			break
		}
		if _, err := policy.validateURL(item.URL); err != nil {
			failed = append(failed, item.Name+": invalid attachment URL")
			continue
		}
		name := safeAttachmentName(item.Name)
		if name == "" {
			name = fmt.Sprintf("attachment-%d", index+1)
		}
		path := uniqueAttachmentPath(dir, name)
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
	manifest := attachmentManifest(content)
	matches := attachmentLine.FindAllStringSubmatch(manifest, maxAttachments+1)
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

func attachmentManifest(content string) string {
	const header = "Current message attachments"
	userMessage := strings.Index(content, "User message:")
	if userMessage < 0 {
		return ""
	}
	start := strings.Index(content[:userMessage], header)
	if start < 0 {
		return ""
	}
	return content[start:userMessage]
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
