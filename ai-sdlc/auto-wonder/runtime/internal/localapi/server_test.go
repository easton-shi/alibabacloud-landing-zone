package localapi

import (
	"encoding/json"
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
	})
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
