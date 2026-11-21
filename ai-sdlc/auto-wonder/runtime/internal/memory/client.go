package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 8 << 20

// Client talks to the dispatch-scoped daemon memory endpoints. The server
// authenticates these calls with the dispatch MCP token, not the executor token.
type Client struct {
	BaseURL    string
	Token      string
	DispatchID int64
	HTTP       *http.Client
}

// New builds a dispatch memory client.
func New(baseURL, token string, dispatchID int64) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token,
		DispatchID: dispatchID, HTTP: &http.Client{Timeout: time.Minute}}
}

// Changes reads one page of memory-store changes after a revision.
func (c *Client) Changes(ctx context.Context, endpoint string, storeID, afterRevision int64,
	limit int) ([]byte, int, error) {
	query := url.Values{}
	query.Set("storeId", strconv.FormatInt(storeID, 10))
	query.Set("afterRevision", strconv.FormatInt(afterRevision, 10))
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	return c.do(ctx, http.MethodGet, endpoint, query, nil)
}

// Mutate applies one create/update/delete operation to a store.
func (c *Client) Mutate(ctx context.Context, endpoint string, storeID int64, body []byte) ([]byte, int, error) {
	query := url.Values{"storeId": []string{strconv.FormatInt(storeID, 10)}}
	return c.do(ctx, http.MethodPost, endpoint, query, body)
}

// Recall validates that a path may be recalled from the store.
func (c *Client) Recall(ctx context.Context, endpoint string, storeID int64, path string) ([]byte, int, error) {
	body, _ := json.Marshal(map[string]any{"storeId": storeID, "path": path})
	return c.do(ctx, http.MethodPost, endpoint, nil, body)
}

// ClaimMaintenance acquires the store-level maintenance lease.
func (c *Client) ClaimMaintenance(ctx context.Context, endpoint string, storeID int64) ([]byte, int, error) {
	body, _ := json.Marshal(map[string]any{"storeId": storeID})
	return c.do(ctx, http.MethodPost, endpoint, nil, body)
}

// RenewMaintenance extends an existing lease.
func (c *Client) RenewMaintenance(ctx context.Context, endpoint string, storeID int64,
	leaseID string) ([]byte, int, error) {
	body, _ := json.Marshal(map[string]any{"storeId": storeID, "leaseId": leaseID})
	return c.do(ctx, http.MethodPost, endpoint, nil, body)
}

// ReleaseMaintenance releases a lease.
func (c *Client) ReleaseMaintenance(ctx context.Context, endpoint string, storeID int64,
	leaseID string) ([]byte, int, error) {
	body, _ := json.Marshal(map[string]any{"storeId": storeID, "leaseId": leaseID})
	return c.do(ctx, http.MethodPost, endpoint, nil, body)
}

func (c *Client) do(ctx context.Context, method, endpoint string, query url.Values,
	body []byte) ([]byte, int, error) {
	if endpoint == "" {
		return nil, http.StatusNotFound, fmt.Errorf("memory endpoint is unavailable")
	}
	target := endpoint
	if strings.HasPrefix(target, "/") {
		target = c.BaseURL + target
	}
	if len(query) > 0 {
		separator := "?"
		if strings.Contains(target, "?") {
			separator = "&"
		}
		target += separator + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, 0, fmt.Errorf("build memory request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: time.Minute}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("memory request: %w", err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if response.StatusCode/100 != 2 {
		return raw, response.StatusCode, fmt.Errorf("memory request: HTTP %d %s",
			response.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, response.StatusCode, nil
}

// MaintenanceEndpoint derives /maintenance/<action> from the snapshot path.
func MaintenanceEndpoint(snapshotPath, action string) string {
	index := strings.LastIndex(snapshotPath, "/")
	if index < 0 {
		return ""
	}
	return snapshotPath[:index] + "/maintenance/" + action
}
