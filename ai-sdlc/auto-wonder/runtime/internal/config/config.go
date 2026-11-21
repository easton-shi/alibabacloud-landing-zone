// Package config loads runtime configuration from the environment.
package config

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Config is the complete runtime configuration.
type Config struct {
	ServerWSURL      string
	ExecutorID       string
	ExecutorToken    string
	Provider         string
	AgentPath        string
	AgentModel       string
	ReasoningEffort  string
	ContextWindow    string
	WorkspaceRoot    string
	MaxConcurrent    int
	HeartbeatSeconds int
	LogLevel         string
	Unattended       bool
	UploadArtifacts  bool
	MemoryMode       string
	DaemonName       string
	// RequirePackageSignature rejects unsigned task packages when true.
	// Default false keeps Community servers that predate signing usable.
	RequirePackageSignature bool

	// OpenCode compatibility knobs. The closed-source 0.3.48 daemon passes
	// --dangerously-skip-permissions and --prompt; public OpenCode 1.x/2.x use
	// --auto and a positional prompt instead. Defaults target public builds.
	OpenCodePermissionFlag string
	OpenCodePromptFlag     string

	// Pi provider name passed through --provider (pi's own provider id).
	PiProvider string
	// OpenCodeACP uses `opencode acp` for streaming and native permission cards.
	OpenCodeACP bool

	// Self-update release source. Empty disables EXECUTOR_UPDATE_V1.
	UpdateBaseURL   string
	UpdatePublicKey string
	UpdateInsecure  bool
}

var reasoningPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

func load() (Config, error) {
	cfg := Config{
		ServerWSURL:             strings.TrimRight(env("AUTOWONDER_SERVER_WS_URL", ""), "/"),
		ExecutorID:              env("AUTOWONDER_EXECUTOR_ID", ""),
		ExecutorToken:           env("AUTOWONDER_EXECUTOR_TOKEN", ""),
		Provider:                strings.ToLower(env("AUTOWONDER_PROVIDER", "opencode")),
		AgentPath:               env("AUTOWONDER_AGENT_PATH", ""),
		AgentModel:              env("AUTOWONDER_AGENT_MODEL", ""),
		ReasoningEffort:         env("AUTOWONDER_REASONING_EFFORT", ""),
		ContextWindow:           env("AUTOWONDER_CONTEXT_WINDOW", ""),
		WorkspaceRoot:           env("AUTOWONDER_WORKSPACE_ROOT", defaultWorkspaceRoot()),
		MaxConcurrent:           envInt("AUTOWONDER_MAX_CONCURRENT_DISPATCHES", 1, 1, 10),
		HeartbeatSeconds:        envInt("AUTOWONDER_HEARTBEAT_SECONDS", 30, 5, 60),
		LogLevel:                strings.ToLower(env("AUTOWONDER_LOG_LEVEL", "info")),
		Unattended:              envBool("AUTOWONDER_UNATTENDED", true),
		UploadArtifacts:         envBool("AUTOWONDER_UPLOAD_ARTIFACTS", true),
		OpenCodePermissionFlag:  env("AUTOWONDER_OPENCODE_PERMISSION_FLAG", "--auto"),
		OpenCodePromptFlag:      env("AUTOWONDER_OPENCODE_PROMPT_FLAG", ""),
		PiProvider:              env("AUTOWONDER_PI_PROVIDER", ""),
		OpenCodeACP:             envBool("AUTOWONDER_OPENCODE_ACP", true),
		MemoryMode:              env("AUTOWONDER_MEMORY_MODE", "platform"),
		DaemonName:              env("AUTOWONDER_DAEMON_NAME", ""),
		RequirePackageSignature: envBool("AUTOWONDER_REQUIRE_PACKAGE_SIGNATURE", false),
		UpdateBaseURL:           env("AUTOWONDER_UPDATE_BASE_URL", ""),
		UpdatePublicKey:         env("AUTOWONDER_UPDATE_PUBLIC_KEY", ""),
		UpdateInsecure:          envBool("AUTOWONDER_UPDATE_INSECURE", false),
	}
	return validate(cfg)
}

// LoadFromArgs parses the `connect` command flags, falling back to the same
// environment variables as the container/systemd entry point.
func LoadFromArgs(args []string) (Config, error) {
	cfg := Config{
		ServerWSURL:             env("AUTOWONDER_SERVER_WS_URL", ""),
		ExecutorID:              env("AUTOWONDER_EXECUTOR_ID", ""),
		ExecutorToken:           env("AUTOWONDER_EXECUTOR_TOKEN", ""),
		Provider:                strings.ToLower(env("AUTOWONDER_PROVIDER", "opencode")),
		AgentPath:               env("AUTOWONDER_AGENT_PATH", ""),
		AgentModel:              env("AUTOWONDER_AGENT_MODEL", ""),
		ReasoningEffort:         env("AUTOWONDER_REASONING_EFFORT", ""),
		ContextWindow:           env("AUTOWONDER_CONTEXT_WINDOW", ""),
		WorkspaceRoot:           env("AUTOWONDER_WORKSPACE_ROOT", defaultWorkspaceRoot()),
		MaxConcurrent:           envInt("AUTOWONDER_MAX_CONCURRENT_DISPATCHES", 1, 1, 10),
		HeartbeatSeconds:        envInt("AUTOWONDER_HEARTBEAT_SECONDS", 30, 5, 60),
		LogLevel:                strings.ToLower(env("AUTOWONDER_LOG_LEVEL", "info")),
		Unattended:              envBool("AUTOWONDER_UNATTENDED", true),
		UploadArtifacts:         envBool("AUTOWONDER_UPLOAD_ARTIFACTS", true),
		OpenCodePermissionFlag:  env("AUTOWONDER_OPENCODE_PERMISSION_FLAG", "--auto"),
		OpenCodePromptFlag:      env("AUTOWONDER_OPENCODE_PROMPT_FLAG", ""),
		PiProvider:              env("AUTOWONDER_PI_PROVIDER", ""),
		OpenCodeACP:             envBool("AUTOWONDER_OPENCODE_ACP", true),
		MemoryMode:              env("AUTOWONDER_MEMORY_MODE", "platform"),
		DaemonName:              env("AUTOWONDER_DAEMON_NAME", ""),
		RequirePackageSignature: envBool("AUTOWONDER_REQUIRE_PACKAGE_SIGNATURE", false),
		UpdateBaseURL:           env("AUTOWONDER_UPDATE_BASE_URL", ""),
		UpdatePublicKey:         env("AUTOWONDER_UPDATE_PUBLIC_KEY", ""),
		UpdateInsecure:          envBool("AUTOWONDER_UPDATE_INSECURE", false),
	}
	flags := flag.NewFlagSet("autowonder-runtime connect", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&cfg.ServerWSURL, "ws-url", cfg.ServerWSURL, "executor WebSocket URL")
	flags.StringVar(&cfg.ExecutorToken, "token", cfg.ExecutorToken, "executor token")
	flags.StringVar(&cfg.ExecutorID, "executor-id", cfg.ExecutorID, "executor id")
	flags.StringVar(&cfg.Provider, "provider", cfg.Provider, "codex|opencode|pi|claude")
	flags.StringVar(&cfg.AgentModel, "model", cfg.AgentModel, "optional provider model id")
	flags.StringVar(&cfg.ReasoningEffort, "reasoning-effort", cfg.ReasoningEffort, "optional reasoning effort")
	flags.StringVar(&cfg.ContextWindow, "context-window", cfg.ContextWindow, "optional context window")
	flags.StringVar(&cfg.AgentPath, "agent-path", cfg.AgentPath, "optional agent CLI path")
	flags.StringVar(&cfg.WorkspaceRoot, "workspace-root", cfg.WorkspaceRoot, "executor workspace root")
	flags.IntVar(&cfg.MaxConcurrent, "max-tasks", cfg.MaxConcurrent, "maximum concurrent dispatches (1-10)")
	flags.StringVar(&cfg.MemoryMode, "memory-mode", cfg.MemoryMode, "platform|provider-local|none")
	flags.StringVar(&cfg.DaemonName, "name", cfg.DaemonName, "display name")
	debug := strings.EqualFold(cfg.LogLevel, "debug")
	flags.BoolVar(&debug, "debug", debug, "enable debug logging")
	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	if flags.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if debug {
		cfg.LogLevel = "debug"
	}
	return validate(cfg)
}

func validate(cfg Config) (Config, error) {
	cfg.Provider = strings.ToLower(strings.TrimSpace(cfg.Provider))
	cfg.LogLevel = strings.ToLower(strings.TrimSpace(cfg.LogLevel))
	if cfg.ServerWSURL == "" {
		return Config{}, fmt.Errorf("AUTOWONDER_SERVER_WS_URL is required")
	}
	if cfg.ExecutorID == "" {
		return Config{}, fmt.Errorf("AUTOWONDER_EXECUTOR_ID is required")
	}
	if cfg.ExecutorToken == "" {
		return Config{}, fmt.Errorf("AUTOWONDER_EXECUTOR_TOKEN is required")
	}
	if cfg.AgentPath == "" {
		cfg.AgentPath = defaultAgentPath(cfg.Provider)
	}
	if cfg.MaxConcurrent < 1 || cfg.MaxConcurrent > 10 {
		return Config{}, fmt.Errorf("max concurrent dispatches must be between 1 and 10")
	}
	if cfg.ReasoningEffort != "" && !reasoningPattern.MatchString(cfg.ReasoningEffort) {
		return Config{}, fmt.Errorf("reasoning effort contains invalid characters")
	}
	if cfg.ContextWindow != "" {
		if parsed, err := strconv.Atoi(cfg.ContextWindow); err != nil || parsed <= 0 {
			return Config{}, fmt.Errorf("context window must be a positive integer")
		}
	}
	switch cfg.Provider {
	case "codex", "opencode", "pi", "claude":
	default:
		return Config{}, fmt.Errorf("unsupported AUTOWONDER_PROVIDER %q (codex|opencode|pi|claude)", cfg.Provider)
	}
	switch cfg.MemoryMode {
	case "platform", "provider-local", "none":
	default:
		return Config{}, fmt.Errorf("unsupported memory mode %q (platform|provider-local|none)", cfg.MemoryMode)
	}
	return cfg, nil
}

// Load validates and returns the runtime configuration.
func Load() (Config, error) { return load() }

// Endpoint returns the authenticated WebSocket URL.
func (c Config) Endpoint() string {
	query := url.Values{}
	query.Set("executorId", c.ExecutorID)
	query.Set("token", c.ExecutorToken)
	query.Set("maxConcurrentDispatches", fmt.Sprintf("%d", c.MaxConcurrent))
	base := strings.TrimSuffix(strings.TrimRight(c.ServerWSURL, "/"), "/ws/executor")
	return base + "/ws/executor?" + query.Encode()
}

func defaultWorkspaceRoot() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "autowonder-runtime")
	}
	return filepath.Join(home, ".autowonder", "runtime")
}

func defaultAgentPath(provider string) string {
	switch provider {
	case "codex":
		return env("AUTOWONDER_CODEX_PATH", "codex")
	case "opencode":
		return env("AUTOWONDER_OPENCODE_PATH", "opencode")
	case "pi":
		return env("AUTOWONDER_PI_PATH", "pi")
	default:
		return env("AUTOWONDER_CLAUDE_PATH", "claude")
	}
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback, min, max int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if parsed < min {
		return min
	}
	if parsed > max {
		return max
	}
	return parsed
}

func envBool(name string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return parsed
}

// BaseURL converts the executor WebSocket URL into the HTTP origin used for
// REST downloads and MCP. It mirrors the dispatch runner's httpBase helper.
func (c Config) BaseURL() string {
	base := strings.TrimSuffix(strings.TrimRight(c.ServerWSURL, "/"), "/ws/executor")
	base = strings.Replace(base, "wss://", "https://", 1)
	base = strings.Replace(base, "ws://", "http://", 1)
	return base
}

// UpdateReceiptPath is the durable hand-off between the old and new process
// during a self-update.
func (c Config) UpdateReceiptPath() string {
	return filepath.Join(c.WorkspaceRoot, ".autowonder", "upgrade-receipt.json")
}
