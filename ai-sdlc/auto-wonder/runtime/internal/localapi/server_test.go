package localapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/memory"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/requests"
)

func TestMemoryRoutesProxyDispatchClient(t *testing.T) {
	var lastPath, lastAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path + "?" + r.URL.RawQuery
		lastAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	client := memory.New(upstream.URL, "awdispatch_token", 7)
	server, err := Start(7, "700001", 1, &requests.Client{}, MemoryEndpoints{
		Client: client, Changes: "/changes", Mutation: "/mutations", Recall: "/recalls",
		Claim: "/maintenance/claims", Renew: "/maintenance/renewals", Release: "/maintenance/releases",
	}, StepEventEndpoints{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	response, err := http.Get(server.Endpoint + "/memory/changes?storeId=11&afterRevision=2")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(lastPath, "storeId=11") ||
		lastAuthorization != "Bearer awdispatch_token" {
		t.Fatalf("changes not proxied: %d %q %q", response.StatusCode, lastPath, lastAuthorization)
	}

	response, err = http.Post(server.Endpoint+"/memory/mutations?storeId=11", "application/json",
		strings.NewReader(`{"operation":"CREATE","path":"m.md","contentMd":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(lastPath, "/mutations?storeId=11") {
		t.Fatalf("mutation not proxied: %d %q", response.StatusCode, lastPath)
	}

	response, err = http.Post(server.Endpoint+"/memory/maintenance/claims", "application/json",
		strings.NewReader(`{"storeId":11}`))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	_ = json.NewDecoder(response.Body).Decode(&payload)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || payload["ok"] != true {
		t.Fatalf("claim not proxied: %d %v", response.StatusCode, payload)
	}
}

func TestStepEventsProxyAndCaptureAcceptedCompletion(t *testing.T) {
	var upstreamPath, upstreamBody, upstreamAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		upstreamAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		upstreamBody = string(body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"accepted":true}`))
	}))
	defer upstream.Close()

	tracker := NewCompletionTracker()
	client := &requests.Client{BaseURL: upstream.URL, Token: "executor-token", DispatchID: 7}
	server, err := Start(7, "700001", 1, client, MemoryEndpoints{}, StepEventEndpoints{
		Client: client, Tracker: tracker, StepID: "700001", StepAttempt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	body := []byte(`{"stepId":"700001","stepAttempt":1,"type":"completion_requested","message":"done","evidenceRefs":["evidence/report.md"]}`)
	response, err := http.Post(server.Endpoint+"/dispatches/7/step-events", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if upstreamPath != "/api/daemon/dispatches/7/step-events" || upstreamBody != string(body) ||
		upstreamAuth != "Bearer executor-token" {
		t.Fatalf("upstream path/body/auth = %q %q %q", upstreamPath, upstreamBody, upstreamAuth)
	}
	refs := tracker.Refs()
	if len(refs) != 1 || refs[0] != "evidence/report.md" {
		t.Fatalf("tracked refs = %v", refs)
	}
}

func TestStepEventsRejectedCompletionIsNotRecorded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"bad refs"}`, http.StatusBadRequest)
	}))
	defer upstream.Close()
	tracker := NewCompletionTracker()
	client := &requests.Client{BaseURL: upstream.URL, Token: "executor-token", DispatchID: 7}
	server, err := Start(7, "700001", 1, client, MemoryEndpoints{}, StepEventEndpoints{
		Client: client, Tracker: tracker, StepID: "700001", StepAttempt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	body := `{"stepId":"700001","stepAttempt":1,"type":"completion_requested","evidenceRefs":["missing.md"]}`
	response, err := http.Post(server.Endpoint+"/dispatches/7/step-events", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || tracker.Completed() {
		t.Fatalf("status=%d completed=%t", response.StatusCode, tracker.Completed())
	}
}

func TestStepEventsAllowsLegacyServerFallback(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	tracker := NewCompletionTracker()
	client := &requests.Client{BaseURL: upstream.URL, Token: "executor-token", DispatchID: 7}
	server, err := Start(7, "700001", 1, client, MemoryEndpoints{}, StepEventEndpoints{
		Client: client, Tracker: tracker, StepID: "700001", StepAttempt: 1, AllowLocalAccept: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	body := `{"stepId":"700001","stepAttempt":1,"type":"completion_requested","evidenceRefs":["evidence/report.md"]}`
	response, err := http.Post(server.Endpoint+"/dispatches/7/step-events", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted || !tracker.Completed() {
		t.Fatalf("status=%d completed=%t", response.StatusCode, tracker.Completed())
	}
}
