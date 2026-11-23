// Package requests bridges an Agent's question to the dispatch request gate.
package requests

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Ask is one INPUT or APPROVAL request.
type Ask struct {
	Kind           string         `json:"kind"`
	Key            string         `json:"key"`
	StepID         string         `json:"stepId"`
	StepAttempt    int            `json:"stepAttempt"`
	Message        string         `json:"message"`
	Schema         map[string]any `json:"schema,omitempty"`
	Context        map[string]any `json:"context,omitempty"`
	TimeoutSeconds int            `json:"timeoutSeconds,omitempty"`
}

// Answer is what the human gate returned.
type Answer struct {
	Action  string         `json:"action"`
	Content map[string]any `json:"content,omitempty"`
	Status  string         `json:"status,omitempty"`
}

// Client talks to /api/daemon/dispatches/{id}/requests.
type Client struct {
	BaseURL    string
	Token      string
	DispatchID int64
}

type requestRecord struct {
	ID     int64          `json:"id"`
	Status string         `json:"status"`
	Answer *Answer        `json:"answer"`
	Body   map[string]any `json:"body"`
	Kind   string         `json:"kind"`
	Key    string         `json:"key"`
}

// Ask creates the request if needed and waits until a human answers, cancels or it expires.
func (c *Client) Ask(ctx context.Context, ask Ask) (Answer, error) {
	if c.BaseURL == "" || c.Token == "" || c.DispatchID == 0 {
		return Answer{}, fmt.Errorf("dispatch request client is not configured")
	}
	if ask.Key == "" {
		digest := sha256.Sum256([]byte(ask.Kind + "\n" + ask.Message))
		ask.Key = "agent-" + hex.EncodeToString(digest[:8])
	}
	if ask.StepAttempt < 1 {
		ask.StepAttempt = 1
	}
	record, err := c.create(ctx, ask)
	if err != nil {
		return Answer{}, err
	}
	if record.Status != "" && record.Status != "PENDING" && record.Answer == nil {
		return Answer{Action: "decline", Status: record.Status}, nil
	}
	if record.Answer != nil {
		return normalize(*record.Answer, record.Status), nil
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = c.cancel(record.ID)
			return Answer{}, ctx.Err()
		case <-ticker.C:
			current, err := c.get(ctx, record.ID)
			if err != nil {
				return Answer{}, err
			}
			if current.Status == "PENDING" {
				continue
			}
			if current.Answer == nil {
				return Answer{Action: "decline", Status: current.Status}, nil
			}
			return normalize(*current.Answer, current.Status), nil
		}
	}
}

func (c *Client) create(ctx context.Context, ask Ask) (requestRecord, error) {
	body, err := json.Marshal(ask)
	if err != nil {
		return requestRecord{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(""), bytes.NewReader(body))
	if err != nil {
		return requestRecord{}, err
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return requestRecord{}, fmt.Errorf("create dispatch request: %w", err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 256*1024))
	if response.StatusCode/100 != 2 {
		return requestRecord{}, fmt.Errorf("create dispatch request: HTTP %d %s", response.StatusCode,
			strings.TrimSpace(string(raw)))
	}
	var record requestRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return requestRecord{}, fmt.Errorf("decode dispatch request: %w", err)
	}
	return record, nil
}

func (c *Client) get(ctx context.Context, id int64) (requestRecord, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint(fmt.Sprintf("/%d", id)), nil)
	if err != nil {
		return requestRecord{}, err
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return requestRecord{}, fmt.Errorf("read dispatch request: %w", err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 256*1024))
	if response.StatusCode/100 != 2 {
		return requestRecord{}, fmt.Errorf("read dispatch request: HTTP %d %s", response.StatusCode,
			strings.TrimSpace(string(raw)))
	}
	var record requestRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return requestRecord{}, fmt.Errorf("decode dispatch request: %w", err)
	}
	return record, nil
}

func (c *Client) cancel(id int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(fmt.Sprintf("/%d/cancel", id)), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return nil
}

func (c *Client) endpoint(suffix string) string {
	base := strings.TrimRight(c.BaseURL, "/")
	return fmt.Sprintf("%s/api/daemon/dispatches/%d/requests%s", base, c.DispatchID, suffix)
}

// StepEvent proxies one step event to the daemon endpoint and returns the raw
// upstream response so the local API can preserve its status and body.
func (c *Client) StepEvent(ctx context.Context, body []byte) ([]byte, int, error) {
	if c.BaseURL == "" || c.Token == "" || c.DispatchID == 0 {
		return nil, 0, fmt.Errorf("step event client is not configured")
	}
	endpoint := fmt.Sprintf("%s/api/daemon/dispatches/%d/step-events",
		strings.TrimRight(c.BaseURL, "/"), c.DispatchID)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("post step event: %w", err)
	}
	defer response.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, 256*1024))
	if readErr != nil {
		return nil, response.StatusCode, fmt.Errorf("read step event response: %w", readErr)
	}
	if response.StatusCode/100 != 2 {
		return raw, response.StatusCode, fmt.Errorf("step event HTTP %d", response.StatusCode)
	}
	return raw, response.StatusCode, nil
}

func normalize(answer Answer, status string) Answer {
	answer.Status = status
	if answer.Action == "" {
		answer.Action = "decline"
	}
	return answer
}

// Fingerprint returns the 64-hex context fingerprint required for APPROVAL requests.
func Fingerprint(value any) string {
	raw, _ := json.Marshal(value)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}
