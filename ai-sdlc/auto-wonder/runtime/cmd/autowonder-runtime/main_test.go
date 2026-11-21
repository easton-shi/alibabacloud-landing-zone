package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/config"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/conversation"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/protocol"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/provider"
)

type stubProvider struct{ name string }

func (s stubProvider) Name() string { return s.name }
func (s stubProvider) Run(context.Context, provider.Request,
	func(provider.Event)) (provider.Result, error) {
	return provider.Result{}, nil
}

type discardSender struct{}

func (discardSender) Send(any) error { return nil }

func TestHeartbeatInventoryArraysAreNeverNull(t *testing.T) {
	frame := buildHeartbeat(config.Config{}, protocol.InventorySnapshot{
		Capacity: 1, Running: []int64{}, Owned: []int64{},
	}, nil, nil, []string{"dispatch_inventory_v1"}, "2026-01-01T00:00:00Z")
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, field := range []string{
		`"runningConversationTurnIds":[]`,
		`"computingConversationTurnIds":[]`,
	} {
		if !strings.Contains(text, field) {
			t.Fatalf("heartbeat missing %s: %s", field, text)
		}
	}
}

func TestRuntimeFeaturesOnlyAdvertiseACPForCodex(t *testing.T) {
	cfg := config.Config{MaxConcurrent: 1}
	codex := conversation.New(cfg, discardSender{}, stubProvider{name: "codex"})
	if !contains(runtimeFeatures(codex, nil, "codex"), "CONVERSATION_ACP_INTERACTION_V1") {
		t.Fatal("codex must advertise ACP")
	}
	for _, name := range []string{"opencode", "pi"} {
		handler := conversation.New(cfg, discardSender{}, stubProvider{name: name})
		if contains(runtimeFeatures(handler, nil, name), "CONVERSATION_ACP_INTERACTION_V1") {
			t.Fatalf("%s must not advertise ACP", name)
		}
		if contains(runtimeFeatures(handler, nil, name), "TASK_PACKAGE_TOOL_HOOKS_V1") {
			t.Fatalf("%s must not advertise blocking tool hooks", name)
		}
	}
	claude := conversation.New(cfg, discardSender{}, stubProvider{name: "claude"})
	if !contains(runtimeFeatures(claude, nil, "claude"), "TASK_PACKAGE_TOOL_HOOKS_V1") {
		t.Fatal("claude must advertise blocking tool hooks")
	}
	if contains(runtimeFeatures(claude, nil, "claude"), "CONVERSATION_ACP_INTERACTION_V1") {
		t.Fatal("claude must not advertise ACP")
	}
	for _, feature := range []string{"CONVERSATION_TURN_EVENT", "CONVERSATION_TURN_CANCEL",
		"CONVERSATION_COMPUTE_INVENTORY_V1", "CONVERSATION_DELIVERY_ACK_V1",
		"CONVERSATION_ANSWER_DELIVERY_V1"} {
		if !contains(runtimeFeatures(codex, nil, "codex"), feature) {
			t.Fatalf("missing %s", feature)
		}
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
