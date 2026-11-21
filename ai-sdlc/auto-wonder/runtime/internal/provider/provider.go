// Package provider adapts local CLI coding agents to one runtime interface.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Request is one agent turn.
type Request struct {
	AgentPath       string
	Prompt          string
	WorkDir         string
	ArtifactsDir    string
	Model           string
	ReasoningEffort string
	ContextWindow   string
	SessionID       string
	SystemPrompt    string
	MCPConfigPath   string
	// ClaudeSettingsPath is an additional Claude Code settings file used for
	// native PreToolUse/PostToolUse tool hooks.
	ClaudeSettingsPath string
	// UseACP selects a provider's native Agent Client Protocol server when it
	// has one (OpenCode). Codex always uses its own stdio JSON-RPC protocol.
	UseACP bool
	// Interactive means a human can answer native permission/input requests.
	// Unattended ACP runs auto-select an allow option instead of blocking.
	Interactive bool
	Env         map[string]string
	Unattended  bool
	// ApprovalPolicy maps to Codex thread/start; empty keeps the task default.
	ApprovalPolicy string
	PiProvider     string
	PermissionFlag string
	PromptFlag     string
	// RequestInput is the in-process bridge used by engines that expose a native
	// approval/input RPC (currently Codex app-server).
	RequestInput func(context.Context, InputRequest) (InputAnswer, error)
}

// InputRequest is a human gate raised by the Agent.
type InputRequest struct {
	Kind           string
	Key            string
	Method         string
	Message        string
	Schema         map[string]any
	Context        map[string]any
	TimeoutSeconds int
}

// InputAnswer is the normalized human reply.
type InputAnswer struct {
	Action  string
	Content map[string]any
}

// Event is a normalized progress signal from an engine.
type Event struct {
	Kind string
	// Text is the short, log-safe form used for dispatch progress.
	Text string
	// FullText keeps the untruncated payload for interactive conversation
	// replies. Dispatch progress must not write multi-kilobyte deltas.
	FullText  string
	SessionID string

	// Structured ACP tool/plan data. Providers without ACP leave these empty.
	ToolCallID    string
	ToolName      string
	ToolKind      string
	ToolTitle     string
	ToolStatus    string
	ToolInput     json.RawMessage
	ToolOutput    json.RawMessage
	ToolLocations json.RawMessage
	Plan          json.RawMessage
}

// Result is the terminal outcome of one turn.
type Result struct {
	Summary string
	// FullText is the untruncated final answer, used by interactive
	// conversations. Dispatch telemetry keeps using Summary.
	FullText  string
	SessionID string
	Usage     *Usage
}

// Usage is the provider-reported per-turn usage.
type Usage struct {
	Provider         string
	Model            string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	Credits          *float64
}

// Provider executes one prompt with a local CLI agent.
type Provider interface {
	Name() string
	Run(ctx context.Context, req Request, emit func(Event)) (Result, error)
}

// New returns the provider selected by name.
func New(name string) (Provider, error) {
	switch strings.ToLower(name) {
	case "codex":
		return &Codex{}, nil
	case "opencode":
		return &OpenCode{}, nil
	case "pi":
		return &Pi{}, nil
	case "claude":
		return &Claude{}, nil
	default:
		return nil, fmt.Errorf("unsupported provider %q", name)
	}
}

func envSlice(base []string, extra map[string]string) []string {
	result := append([]string{}, base...)
	seen := map[string]bool{}
	for _, item := range result {
		if key, _, ok := strings.Cut(item, "="); ok {
			seen[key] = true
		}
	}
	for key, value := range extra {
		if !seen[key] {
			result = append(result, key+"="+value)
		}
	}
	return result
}

func currentEnv() []string { return os.Environ() }
