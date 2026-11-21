// Package localapi exposes the per-dispatch loopback endpoint used by Agent tools.
package localapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/memory"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/requests"
)

// Server is a loopback HTTP endpoint for one dispatch.
type Server struct {
	http     *http.Server
	listener net.Listener
	Endpoint string
}

// MemoryEndpoints wires the dispatch-scoped daemon memory API into loopback.
// Endpoints are optional; when Client is nil the memory routes return 404.
type MemoryEndpoints struct {
	Client   *memory.Client
	Changes  string
	Mutation string
	Recall   string
	Claim    string
	Renew    string
	Release  string
}

// Start binds 127.0.0.1 on a random port and serves the Agent-facing loopback API.
func Start(dispatchID int64, stepID string, stepAttempt int, client *requests.Client,
	memoryEndpoints MemoryEndpoints) (*Server, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen local API: %w", err)
	}
	server := &Server{listener: listener, Endpoint: "http://" + listener.Addr().String()}
	mux := http.NewServeMux()
	mux.HandleFunc("/request-input", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !isLoopback(r.RemoteAddr) {
			http.Error(w, "loopback only", http.StatusForbidden)
			return
		}
		var ask requests.Ask
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&ask); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		if ask.StepID == "" {
			ask.StepID = stepID
		}
		if ask.StepAttempt < 1 {
			ask.StepAttempt = stepAttempt
		}
		answer, err := client.Ask(r.Context(), ask)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(answer)
	})
	registerMemoryRoutes(mux, memoryEndpoints)
	server.http = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.http.Serve(listener) }()
	return server, nil
}

// Close stops the loopback endpoint.
func (s *Server) Close() {
	if s == nil || s.http == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.http.Shutdown(ctx)
}

func registerMemoryRoutes(mux *http.ServeMux, endpoints MemoryEndpoints) {
	if endpoints.Client == nil {
		return
	}
	mux.HandleFunc("/memory/changes", func(w http.ResponseWriter, r *http.Request) {
		if !requireLoopback(w, r) || !requireMethod(w, r, http.MethodGet) {
			return
		}
		storeID, err := strconv.ParseInt(r.URL.Query().Get("storeId"), 10, 64)
		if err != nil || storeID <= 0 {
			http.Error(w, "storeId is required", http.StatusBadRequest)
			return
		}
		after, _ := strconv.ParseInt(r.URL.Query().Get("afterRevision"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		raw, status, err := endpoints.Client.Changes(r.Context(), endpoints.Changes, storeID, after, limit)
		writeUpstream(w, raw, status, err)
	})
	mux.HandleFunc("/memory/mutations", func(w http.ResponseWriter, r *http.Request) {
		if !requireLoopback(w, r) || !requireMethod(w, r, http.MethodPost) {
			return
		}
		storeID, err := strconv.ParseInt(r.URL.Query().Get("storeId"), 10, 64)
		if err != nil || storeID <= 0 {
			http.Error(w, "storeId is required", http.StatusBadRequest)
			return
		}
		body, err := readBody(w, r)
		if err != nil {
			return
		}
		raw, status, err := endpoints.Client.Mutate(r.Context(), endpoints.Mutation, storeID, body)
		writeUpstream(w, raw, status, err)
	})
	mux.HandleFunc("/memory/recalls", func(w http.ResponseWriter, r *http.Request) {
		if !requireLoopback(w, r) || !requireMethod(w, r, http.MethodPost) {
			return
		}
		var request struct {
			StoreID int64  `json:"storeId"`
			Path    string `json:"path"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&request); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		if request.StoreID <= 0 || request.Path == "" {
			http.Error(w, "storeId and path are required", http.StatusBadRequest)
			return
		}
		raw, status, err := endpoints.Client.Recall(r.Context(), endpoints.Recall, request.StoreID, request.Path)
		writeUpstream(w, raw, status, err)
	})
	mux.HandleFunc("/memory/maintenance/claims", func(w http.ResponseWriter, r *http.Request) {
		handleLease(w, r, func(ctx context.Context, endpoint string, storeID int64,
			_ string) ([]byte, int, error) {
			return endpoints.Client.ClaimMaintenance(ctx, endpoint, storeID)
		}, endpoints.Claim, false)
	})
	mux.HandleFunc("/memory/maintenance/renewals", func(w http.ResponseWriter, r *http.Request) {
		handleLease(w, r, endpoints.Client.RenewMaintenance, endpoints.Renew, true)
	})
	mux.HandleFunc("/memory/maintenance/releases", func(w http.ResponseWriter, r *http.Request) {
		handleLease(w, r, endpoints.Client.ReleaseMaintenance, endpoints.Release, true)
	})
}

func handleLease(w http.ResponseWriter, r *http.Request, call func(context.Context, string, int64, string) ([]byte, int, error),
	endpoint string, needsLease bool) {
	if !requireLoopback(w, r) || !requireMethod(w, r, http.MethodPost) {
		return
	}
	var request struct {
		StoreID int64  `json:"storeId"`
		LeaseID string `json:"leaseId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&request); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if request.StoreID <= 0 || (needsLease && request.LeaseID == "") {
		http.Error(w, "storeId and leaseId are required", http.StatusBadRequest)
		return
	}
	raw, status, err := call(r.Context(), endpoint, request.StoreID, request.LeaseID)
	writeUpstream(w, raw, status, err)
}

func requireLoopback(w http.ResponseWriter, r *http.Request) bool {
	if !isLoopback(r.RemoteAddr) {
		http.Error(w, "loopback only", http.StatusForbidden)
		return false
	}
	return true
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	var body json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024*1024)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return nil, err
	}
	return body, nil
}

func writeUpstream(w http.ResponseWriter, raw []byte, status int, err error) {
	if status == 0 {
		status = http.StatusBadGateway
	}
	if len(raw) > 0 {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	if len(raw) > 0 {
		_, _ = w.Write(raw)
		return
	}
	if err != nil {
		_, _ = w.Write([]byte(`{"error":` + strconv.Quote(err.Error()) + `}`))
	}
}

func isLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	return host == "127.0.0.1" || host == "::1" || strings.HasPrefix(host, "127.")
}
