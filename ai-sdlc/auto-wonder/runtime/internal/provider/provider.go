// Package provider adapts local CLI coding agents to one runtime interface.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Request is one agent turn.
type Request struct {
	DispatchID      int64
	AgentPath       string
	Prompt          string
	WorkDir         string
	ArtifactsDir    string
	Model           string
	ReasoningEffort string
	ContextWindow   string
	SessionID       string
	ConversationID  int64
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
	// Written reports whether the normalized answer was accepted by the
	// provider's native RPC channel.
	Written func(error)
}

// ReportWritten invokes the provider's write-completion callback, if any.
func (a InputAnswer) ReportWritten(err error) {
	if a.Written != nil {
		a.Written(err)
	}
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

// Steerable is implemented by providers that can cooperatively steer an active
// turn. It is optional so older providers remain compatible.
type Steerable interface {
	// Steer hands new content to the active provider turn. It must return after
	// the handoff is accepted, not after the steering turn finishes.
	Steer(ctx context.Context, dispatchID int64, content string) error
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

var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var reservedEnvironmentNames = map[string]struct{}{
	"CLAUDE_CONFIG_DIR":       {},
	"CODEX_HOME":              {},
	"QODERCN_CONFIG_DIR":      {},
	"QODER_CONFIG_DIR":        {},
	"QODER_HOST_SERVICE_NAME": {},
	"QODER_INTEGRATION_ID":    {},
}

// ValidateTaskEnvironment enforces the platform environment contract before
// any Runtime-owned variables are added.
func ValidateTaskEnvironment(overrides map[string]string) error {
	normalized := make(map[string]struct{}, len(overrides))
	for key, value := range overrides {
		if !environmentNamePattern.MatchString(key) {
			return fmt.Errorf("invalid environment variable name %q", key)
		}
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "AUTOWONDER_") {
			return fmt.Errorf("environment variable %q is reserved by Runtime", key)
		}
		if _, reserved := reservedEnvironmentNames[upper]; reserved {
			return fmt.Errorf("environment variable %q is reserved by Runtime", key)
		}
		if strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("environment variable %q contains NUL", key)
		}
		if _, duplicate := normalized[upper]; duplicate {
			return fmt.Errorf("environment variables collide case-insensitively: %q", key)
		}
		normalized[upper] = struct{}{}
	}
	return nil
}

// MergeEnvironment overlays provider values case-insensitively.
func MergeEnvironment(base []string, overrides map[string]string) ([]string, error) {
	if len(overrides) == 0 {
		return append([]string(nil), base...), nil
	}
	baseNames := make(map[string]string, len(base))
	for _, item := range base {
		if key, _, ok := strings.Cut(item, "="); ok {
			upper := strings.ToUpper(key)
			if _, exists := baseNames[upper]; !exists {
				baseNames[upper] = key
			}
		}
	}
	names := make([]string, 0, len(overrides))
	normalized := make(map[string]string, len(overrides))
	outputNames := make(map[string]string, len(overrides))
	for key, value := range overrides {
		if !environmentNamePattern.MatchString(key) {
			return nil, fmt.Errorf("invalid environment variable name %q", key)
		}
		upper := strings.ToUpper(key)
		if strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("environment variable %q contains NUL", key)
		}
		if _, duplicate := normalized[upper]; duplicate {
			return nil, fmt.Errorf("environment variables collide case-insensitively: %q", key)
		}
		outputName := key
		if existing, ok := baseNames[upper]; ok {
			outputName = existing
		}
		names = append(names, outputName)
		normalized[upper] = value
		outputNames[upper] = outputName
	}
	sort.Strings(names)
	result := make([]string, 0, len(base)+len(names))
	for _, item := range base {
		key, _, ok := strings.Cut(item, "=")
		if ok {
			if _, overridden := normalized[strings.ToUpper(key)]; overridden {
				continue
			}
		}
		result = append(result, item)
	}
	for _, name := range names {
		upper := strings.ToUpper(name)
		result = append(result, outputNames[upper]+"="+normalized[upper])
	}
	return result, nil
}

func currentEnv() []string { return os.Environ() }
