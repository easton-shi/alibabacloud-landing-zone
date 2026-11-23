package main

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/config"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/conversation"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/protocol"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/provider"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/update"
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

func TestRuntimeFeaturesAdvertiseTerminalSteer(t *testing.T) {
	cfg := config.Config{MaxConcurrent: 1}
	for _, name := range []string{"codex", "opencode", "pi", "claude"} {
		handler := conversation.New(cfg, discardSender{}, stubProvider{name: name})
		if !contains(runtimeFeatures(handler, nil, name), "terminal_steer_v1") {
			t.Fatalf("%s must advertise terminal_steer_v1", name)
		}
	}
}

func TestRuntimeFeaturesDoNotAdvertiseIncompleteCheckpointOrUpdate(t *testing.T) {
	previousVersion := version
	version = "0.3.48"
	t.Cleanup(func() { version = previousVersion })
	handler := conversation.New(config.Config{MaxConcurrent: 1}, discardSender{}, stubProvider{name: "codex"})
	features := runtimeFeatures(handler, &update.Updater{}, "codex")
	for _, feature := range []string{
		"CHECKPOINT_LIGHTWEIGHT_V1",
		"EXECUTOR_UPDATE_V1",
		"EXECUTOR_UPDATE_RESTART_V1",
	} {
		if contains(features, feature) {
			t.Fatalf("incomplete feature %s was advertised: %v", feature, features)
		}
	}
}

func TestRuntimeFeaturesAdvertiseSafeUpdateOnSupportedHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("in-place update is intentionally unsupported on Windows")
	}
	previousVersion := version
	version = "0.1.0"
	t.Cleanup(func() { version = previousVersion })
	updater := update.New(update.Config{BaseURL: "https://updates.example", Insecure: true}, "0.1.0", "", "")
	handler := conversation.New(config.Config{MaxConcurrent: 1}, discardSender{}, stubProvider{name: "codex"})
	features := runtimeFeatures(handler, updater, "codex")
	for _, feature := range []string{"EXECUTOR_UPDATE_V1", "EXECUTOR_UPDATE_RESTART_V1"} {
		if !contains(features, feature) {
			t.Fatalf("safe update feature %s missing: %v", feature, features)
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
