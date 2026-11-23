// Command autowonder-runtime is an open-source AutoWonder executor runtime.
//
// It speaks the scheduler-executor protocol documented in
// ai-sdlc/auto-wonder/docs/scheduler-executor-protocol.md and can drive
// Codex, OpenCode, Pi or Claude Code as its engine.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/config"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/control"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/conversation"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/dispatch"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/hooks"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/protocol"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/provider"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/update"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/workspacegc"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/ws"
)

var version = "dev"

func main() {
	command, args := cliCommand(os.Args[1:])
	switch command {
	case "help", "-h", "--help":
		printUsage()
		return
	case "version", "--version", "-v":
		fmt.Printf("autowonder-runtime %s\n", version)
		return
	case "hook":
		if len(args) > 0 && args[0] == "tool" {
			os.Exit(runToolHookCommand(args[1:]))
		}
		os.Exit(runHookCommand(args))
	case "connect", "start", "run":
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		printUsage()
		os.Exit(2)
	}

	cfg, err := config.LoadFromArgs(args)
	if err != nil {
		slog.Error("configuration error", "error", err)
		os.Exit(2)
	}
	configureLogging(cfg.LogLevel)
	engine, err := provider.New(cfg.Provider)
	if err != nil {
		slog.Error("provider error", "error", err)
		os.Exit(2)
	}
	if cfg.Provider == "opencode" && cfg.OpenCodeACP && !provider.DetectOpenCodeACP(cfg.AgentPath) {
		slog.Warn("opencode acp is unavailable; falling back to run --format json")
		cfg.OpenCodeACP = false
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, engine, time.Now().UTC().Format(time.RFC3339)); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("runtime stopped", "error", err)
		os.Exit(1)
	}
}

func cliCommand(args []string) (string, []string) {
	if len(args) == 0 {
		return "run", nil
	}
	if strings.HasPrefix(args[0], "-") {
		return "run", args
	}
	return args[0], args[1:]
}

func printUsage() {
	fmt.Print(`autowonder-runtime - open-source AutoWonder executor runtime

Usage:
  autowonder-runtime connect --ws-url URL --token TOKEN --executor-id ID \
      --provider codex|opencode|pi|claude [--model MODEL] \
      [--reasoning-effort EFFORT] [--context-window TOKENS] [--workspace-root DIR] \
      [--max-tasks 1-10] [--memory-mode platform|provider-local|none] \
      [--agent-path PATH] [--name NAME] [--debug]
  autowonder-runtime hook run --context FILE --trigger beforeCommit|beforePush --repo DIR
  autowonder-runtime hook tool --context FILE --trigger beforeTool|afterTool --provider claude
  autowonder-runtime version

Environment variables remain supported for container/systemd installations.
`)
}

func run(ctx context.Context, cfg config.Config, engine provider.Provider, startedAt string) error {
	conversations := conversation.New(cfg, nil, engine)
	executable, _ := os.Executable()
	updater := update.New(update.Config{BaseURL: cfg.UpdateBaseURL, PublicKey: cfg.UpdatePublicKey,
		Insecure: cfg.UpdateInsecure}, version, executable, cfg.UpdateReceiptPath())
	if updater != nil {
		if rolledBack, recoveryErr := updater.RecoverPending(); recoveryErr != nil {
			slog.Error("recover pending update", "error", recoveryErr)
		} else if rolledBack {
			slog.Warn("rolled back runtime update; restarting previous version")
			return rerunAfterRollback(executable)
		}
	}
	backoff := time.Second
	for {
		connected, err := serve(ctx, cfg, engine, conversations, updater, startedAt)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			slog.Warn("executor connection ended", "error", err)
		}
		if connected {
			backoff = time.Second
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func rerunAfterRollback(executable string) error {
	cmd := exec.Command(executable, os.Args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "AUTOWONDER_UPGRADE_ROLLED_BACK=1")
	return cmd.Run()
}

// serve runs one WebSocket session. Disconnecting cancels in-flight dispatches;
// the next session reconnects and the server re-delivers anything that still
// needs execution.
func serve(ctx context.Context, cfg config.Config, engine provider.Provider,
	conversations *conversation.Handler, updater *update.Updater, startedAt string) (bool, error) {
	client, err := ws.Dial(ctx, cfg.Endpoint())
	if err != nil {
		return false, err
	}
	defer client.Close()
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	conversations.Attach(client)
	defer conversations.Detach()

	runner := dispatch.New(cfg, client, engine)
	runner.ReplayOutbox()
	go replayDispatchOutbox(sessionCtx, runner)
	if cfg.WorkspaceGCEnabled {
		go runWorkspaceGC(sessionCtx, cfg, workspacegc.Collector{
			Root: cfg.WorkspaceRoot, TaskTTL: cfg.WorkspaceGCTaskTTL,
			ConversationTTL:  cfg.WorkspaceGCConversationTTL,
			IsDispatchActive: runner.Owns, IsConversationActive: conversations.ConversationActive,
			HighWatermark: cfg.WorkspaceGCHighWatermark, LowWatermark: cfg.WorkspaceGCLowWatermark,
			MinFreeBytes: cfg.WorkspaceGCMinFreeBytes, TargetFreeBytes: cfg.WorkspaceGCTargetFreeBytes,
			DiskUsage: workspacegc.ReadDiskUsage,
		})
	}
	executorID, _ := strconv.ParseInt(cfg.ExecutorID, 10, 64)
	executable, _ := os.Executable()
	controlHandler := &control.Handler{
		ExecutorID: executorID,
		Version:    version,
		Args:       os.Args[1:],
		Executable: executable,
		Inventory:  runtimeInventory{dispatcher: runner, conversations: conversations},
		Send:       client.Send,
		Updater:    updater,
	}
	if updater != nil {
		controlHandler.PublishUpgradeReceipt(client.Send)
	}
	go heartbeat(sessionCtx, client, cfg, runner, conversations, updater, engine.Name(), startedAt)
	slog.Info("runtime connected", "executorId", cfg.ExecutorID, "provider", engine.Name(), "capacity", cfg.MaxConcurrent)

	for {
		payload, err := client.Read()
		if err != nil {
			if ctx.Err() != nil {
				return true, ctx.Err()
			}
			return true, err
		}
		if controlHandler.Handle(sessionCtx, payload) {
			continue
		}
		conversations.Handle(sessionCtx, payload)
		runner.Handle(sessionCtx, payload)
	}
}

func replayDispatchOutbox(ctx context.Context, runner *dispatch.Runner) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runner.ReplayOutbox()
		}
	}
}

func runWorkspaceGC(ctx context.Context, cfg config.Config, collector workspacegc.Collector) {
	if delay := cfg.WorkspaceGCInitialDelay; delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
	interval := cfg.WorkspaceGCInterval
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		report, err := collector.RunOnce(ctx)
		if err != nil {
			slog.Warn("workspace GC failed", "error", err)
		} else if len(report.Removed) > 0 {
			slog.Info("workspace GC completed", "removed", len(report.Removed))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func heartbeat(ctx context.Context, client *ws.Client, cfg config.Config, runner *dispatch.Runner,
	conversations *conversation.Handler, updater *update.Updater, providerName, startedAt string) {
	ticker := time.NewTicker(time.Duration(cfg.HeartbeatSeconds) * time.Second)
	defer ticker.Stop()
	features := runtimeFeatures(conversations, updater, providerName)
	send := func() {
		inventory := runner.Inventory()
		runningTurns, computingTurns := conversations.Inventory()
		frame := buildHeartbeat(cfg, inventory, runningTurns, computingTurns, features, startedAt)
		if err := client.Send(frame); err != nil {
			slog.Warn("heartbeat failed", "error", err)
		}
	}
	send()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			send()
		}
	}
}

func configureLogging(level string) {
	var parsed slog.Level
	switch level {
	case "debug":
		parsed = slog.LevelDebug
	case "warn":
		parsed = slog.LevelWarn
	case "error":
		parsed = slog.LevelError
	default:
		parsed = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parsed})))
}

func runToolHookCommand(args []string) int {
	flags := flag.NewFlagSet("hook tool", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	contextPath := flags.String("context", "", "path to tool-hook-context.json")
	trigger := flags.String("trigger", "", "beforeTool or afterTool")
	providerName := flags.String("provider", "claude", "provider that raised the tool event")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *contextPath == "" || (*trigger != hooks.BeforeTool && *trigger != hooks.AfterTool) {
		fmt.Fprintln(os.Stderr, "hook tool requires --context and --trigger beforeTool|afterTool")
		return 2
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
	if err != nil {
		fmt.Fprintf(os.Stderr, "read tool hook stdin: %v\n", err)
		return 1
	}
	var native struct {
		HookEventName string          `json:"hook_event_name"`
		ToolName      string          `json:"tool_name"`
		ToolInput     json.RawMessage `json:"tool_input"`
		ToolResponse  json.RawMessage `json:"tool_response"`
		Cwd           string          `json:"cwd"`
		SessionID     string          `json:"session_id"`
		ToolUseID     string          `json:"tool_use_id"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &native); err != nil {
			fmt.Fprintf(os.Stderr, "parse native tool hook input: %v\n", err)
			return 1
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	input := hooks.ToolHookInput{
		Trigger: *trigger, Provider: *providerName, Cwd: native.Cwd,
		ToolName: native.ToolName, ToolInput: native.ToolInput,
		CallID: native.ToolUseID, ToolOutput: native.ToolResponse,
	}
	results, runErr := hooks.RunToolHooks(ctx, *contextPath, *trigger, *providerName, input)
	for _, result := range results {
		if result.Skipped || result.Success {
			continue
		}
		message := result.Stderr
		if message == "" {
			message = result.Stdout
		}
		if message == "" {
			message = result.Error
		}
		fmt.Fprintf(os.Stderr, "autowonder %s hook %s failed: %s\n", *trigger, result.Name, message)
	}
	if runErr == nil {
		return 0
	}
	if *trigger == hooks.AfterTool {
		if runtimeContext, err := hooks.LoadGitContext(*contextPath); err == nil {
			_ = hooks.MarkFailure(runtimeContext.ArtifactsDir, runErr.Error())
		}
	}
	return 2
}

func runHookCommand(args []string) int {
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprintln(os.Stderr, "usage: autowonder-runtime hook run --context FILE --trigger TRIGGER --repo DIR")
		return 2
	}
	flags := flag.NewFlagSet("hook run", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	contextPath := flags.String("context", "", "path to hook-context.json")
	trigger := flags.String("trigger", "", "beforeCommit or beforePush")
	repo := flags.String("repo", "", "repository working tree")
	originalHooks := flags.String("original-hooks", "", "original hooks directory to chain")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if *contextPath == "" || *trigger == "" || *repo == "" {
		fmt.Fprintln(os.Stderr, "hook run requires --context, --trigger and --repo")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := hooks.RunGitHook(ctx, *contextPath, *trigger, *repo, *originalHooks,
		flags.Args()); err != nil {
		fmt.Fprintf(os.Stderr, "autowonder runtime hook failed: %v\n", err)
		return 1
	}
	return 0
}

type runtimeInventory struct {
	dispatcher    *dispatch.Runner
	conversations *conversation.Handler
}

func (i runtimeInventory) Busy() bool {
	return i.dispatcher.Busy() || i.conversations.Busy()
}

func (i runtimeInventory) SetDraining(value bool) {
	i.dispatcher.SetDraining(value)
	i.conversations.SetDraining(value)
}

func (i runtimeInventory) CancelAll() {
	i.dispatcher.CancelAll()
	i.conversations.CancelAll()
}

func (i runtimeInventory) WaitIdle(ctx context.Context) error {
	if err := i.dispatcher.WaitIdle(ctx); err != nil {
		return err
	}
	return i.conversations.WaitIdle(ctx)
}

// buildHeartbeat keeps the inventory arrays non-nil. The server's
// dispatch_inventory_v1 parser rejects `null` for any of them and closes the
// executor session.
func buildHeartbeat(cfg config.Config, inventory protocol.InventorySnapshot,
	runningTurns, computingTurns []int64, features []string, startedAt string) protocol.Heartbeat {
	if runningTurns == nil {
		runningTurns = []int64{}
	}
	if computingTurns == nil {
		computingTurns = []int64{}
	}
	return protocol.Heartbeat{
		Type:                         "HEARTBEAT",
		ProtocolFeatures:             features,
		MaxConcurrentDispatches:      inventory.Capacity,
		DispatchInventoryReady:       true,
		RunningDispatchIDs:           inventory.Running,
		OwnedDispatchIDs:             inventory.Owned,
		RunningConversationTurnIDs:   runningTurns,
		ComputingConversationTurnIDs: computingTurns,
		DispatchInventoryError:       inventory.Error,
		Version:                      version,
		Model:                        cfg.AgentModel,
		StartedAt:                    startedAt,
		RestartRequestID:             os.Getenv("AUTOWONDER_RESTART_REQUEST_ID"),
	}
}

func runtimeFeatures(conversations *conversation.Handler, updater *update.Updater, providerName string) []string {
	features := []string{
		"dispatch_inventory_v1",
		"AGENT_ENVIRONMENT_VARIABLES_V1",
		"TASK_MODEL_SELECTION_V1",
		"TASK_PACKAGE_SIGNATURE_V1",
		"TASK_PACKAGE_HOOKS_V1",
		"DEBUG_LOG_V1",
		"DISPATCH_REQUESTS_V1",
		"EXECUTOR_RESTART_V1",
		"EXECUTOR_RESTART_POLICY_V1",
		"EXECUTOR_LAUNCH_COMMAND_V1",
		"CONVERSATION_TURN_EVENT",
		"CONVERSATION_TURN_CANCEL",
		"CONVERSATION_COMPUTE_INVENTORY_V1",
		"CONVERSATION_ANSWER_DELIVERY_V1",
		"CONVERSATION_DELIVERY_ACK_V1",
		"ATTACHMENT_MANIFEST_V1",
		"terminal_steer_v1",
	}
	if conversations.SupportsACP() {
		features = append(features, "CONVERSATION_ACP_INTERACTION_V1")
	}
	if updater.SupportsSelfUpdate() && version != "" && version != "dev" {
		features = append(features, "EXECUTOR_UPDATE_V1", "EXECUTOR_UPDATE_RESTART_V1")
	}
	if providerName == "claude" {
		features = append(features, "TASK_PACKAGE_TOOL_HOOKS_V1")
	}
	return features
}
