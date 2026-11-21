package requests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAskCreatesThenReadsTheAnsweredRequest(t *testing.T) {
	getCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer executor-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodPost:
			var ask Ask
			if err := json.NewDecoder(r.Body).Decode(&ask); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if ask.Kind != "INPUT" || ask.StepID != "7" || ask.StepAttempt != 2 {
				http.Error(w, "bad ask", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 11, "status": "PENDING"})
		case http.MethodGet:
			getCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 11, "status": "RESOLVED",
				"answer": map[string]any{"action": "accept", "content": map[string]any{"environment": "dev"}},
			})
		}
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, Token: "executor-token", DispatchID: 5}
	answer, err := client.Ask(context.Background(), Ask{
		Kind: "INPUT", Key: "environment-1", StepID: "7", StepAttempt: 2, Message: "choose",
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Action != "accept" || answer.Content["environment"] != "dev" || getCalls == 0 {
		t.Fatalf("unexpected answer: %+v getCalls=%d", answer, getCalls)
	}
}
