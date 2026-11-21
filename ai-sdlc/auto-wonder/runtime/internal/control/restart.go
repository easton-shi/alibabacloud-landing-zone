// Package control handles executor-scoped control frames such as remote restart.
package control

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/update"
)

const (
	restartModeIdle  = "WHEN_IDLE"
	restartModeForce = "FORCE"

	statusWaitingIdle = "WAITING_IDLE"
	statusRestarting  = "RESTARTING"
	statusFailed      = "FAILED"
)

// Inventory is the slice of the dispatch runner used by restart handling.
type Inventory interface {
	Busy() bool
	SetDraining(bool)
	CancelAll()
	WaitIdle(context.Context) error
}

// RestartFrame is the server's EXECUTOR_RESTART payload.
type RestartFrame struct {
	Type        string        `json:"type"`
	ExecutorID  int64         `json:"executorId"`
	RequestID   string        `json:"requestId"`
	Update      bool          `json:"update"`
	RestartMode string        `json:"restartMode"`
	Launch      *LaunchConfig `json:"launch"`
	IssuedAt    string        `json:"issuedAt"`
}

// LaunchConfig is the structured, already-saved launch configuration.
type LaunchConfig struct {
	RuntimeVersion          string `json:"runtimeVersion"`
	WorkspaceRoot           string `json:"workspaceRoot"`
	DaemonName              string `json:"daemonName"`
	Debug                   bool   `json:"debug"`
	MemoryMode              string `json:"memoryMode"`
	MaxConcurrentDispatches int    `json:"maxConcurrentDispatches"`
	Model                   string `json:"model"`
	ReasoningEffort         string `json:"reasoningEffort"`
	ContextWindow           string `json:"contextWindow"`
	RestartMode             string `json:"restartMode"`
}

// RestartResult reports progress; completion is confirmed by the next heartbeat.
type RestartResult struct {
	Type       string `json:"type"`
	ExecutorID int64  `json:"executorId"`
	RequestID  string `json:"requestId"`
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
}

// Handler processes one executor's control frames.
type Handler struct {
	ExecutorID int64
	Version    string
	Args       []string
	Executable string
	Inventory  Inventory
	Send       func(any) error

	// Exec replaces the current process. Tests inject a fake.
	Exec func(executable string, argv, env []string) error
	// IdlePoll is how often a WHEN_IDLE restart re-checks the inventory.
	IdlePoll time.Duration
	// ForceGrace bounds how long FORCE waits after cancelling running work.
	ForceGrace time.Duration
	// Updater is non-nil only when a release source is configured.
	Updater *update.Updater
	// DrainTimeout bounds an upgrade's wait for running work to finish.
	DrainTimeout time.Duration

	mu        sync.Mutex
	seen      map[string]bool
	restarted bool
}

// Handle returns true when the frame belongs to the control plane.
func (h *Handler) Handle(ctx context.Context, raw []byte) bool {
	var envelope struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return false
	}
	switch envelope.Type {
	case "EXECUTOR_RESTART":
		var frame RestartFrame
		if json.Unmarshal(raw, &frame) != nil || frame.RequestID == "" {
			return true
		}
		if frame.ExecutorID != 0 && frame.ExecutorID != h.ExecutorID {
			return true
		}
		if !h.accept(frame.RequestID) {
			return true
		}
		go h.restart(ctx, frame)
		return true
	case "EXECUTOR_UPGRADE":
		var frame UpgradeFrame
		if json.Unmarshal(raw, &frame) != nil || frame.RequestID == "" {
			return true
		}
		if frame.ExecutorID != 0 && frame.ExecutorID != h.ExecutorID {
			return true
		}
		h.handleUpgrade(ctx, frame)
		return true
	case "EXECUTOR_UPGRADE_RESULT_ACK", "EXECUTOR_UPGRADE_RESULT_SUPPORT":
		h.acknowledgeUpgrade(raw)
		return true
	default:
		return false
	}
}

func (h *Handler) accept(requestID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.seen == nil {
		h.seen = map[string]bool{}
	}
	if h.seen[requestID] || h.restarted {
		return false
	}
	h.seen[requestID] = true
	return true
}

func (h *Handler) restart(ctx context.Context, frame RestartFrame) {
	mode := frame.RestartMode
	if frame.Launch != nil && frame.Launch.RestartMode != "" {
		mode = frame.Launch.RestartMode
	}
	if frame.Update {
		h.upgrade(ctx, frame.RequestID, "", frame.Launch)
		return
	}
	argv, err := h.buildArgs(frame.Launch)
	if err != nil {
		h.report(frame.RequestID, statusFailed, err.Error())
		return
	}

	switch mode {
	case restartModeIdle:
		if !h.busy() {
			h.exec(frame, argv)
			return
		}
		h.Inventory.SetDraining(true)
		defer func() {
			if !h.done() {
				h.Inventory.SetDraining(false)
			}
		}()
		poll := h.IdlePoll
		if poll <= 0 {
			poll = 30 * time.Second
		}
		ticker := time.NewTicker(poll)
		defer ticker.Stop()
		for {
			h.report(frame.RequestID, statusWaitingIdle, "waiting for running work to finish")
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !h.busy() {
					h.exec(frame, argv)
					return
				}
			}
		}
	case restartModeForce:
		if h.Inventory != nil {
			h.Inventory.CancelAll()
			grace := h.ForceGrace
			if grace <= 0 {
				grace = 5 * time.Second
			}
			waitCtx, cancel := context.WithTimeout(ctx, grace)
			_ = h.Inventory.WaitIdle(waitCtx)
			cancel()
		}
		h.exec(frame, argv)
	case "":
		if h.busy() {
			h.report(frame.RequestID, statusFailed, "executor is busy; retry when idle or use a restart mode")
			return
		}
		h.exec(frame, argv)
	default:
		h.report(frame.RequestID, statusFailed, "unsupported restartMode "+mode)
	}
}

func (h *Handler) buildArgs(launch *LaunchConfig) ([]string, error) {
	args := append([]string(nil), h.Args...)
	if launch == nil {
		return args, nil
	}
	if launch.RuntimeVersion != "" && h.Version != "" && launch.RuntimeVersion != h.Version {
		return nil, fmt.Errorf("cross-version launch is not supported (runtime %s, target %s)",
			h.Version, launch.RuntimeVersion)
	}
	if launch.WorkspaceRoot != "" {
		args = setFlag(args, "--workspace-root", launch.WorkspaceRoot)
	}
	if launch.DaemonName != "" {
		args = setFlag(args, "--name", launch.DaemonName)
	}
	args = setBoolFlag(args, "--debug", launch.Debug)
	if launch.MemoryMode != "" {
		args = setFlag(args, "--memory-mode", launch.MemoryMode)
	}
	if launch.MaxConcurrentDispatches > 0 {
		args = setFlag(args, "--max-tasks", strconv.Itoa(launch.MaxConcurrentDispatches))
	}
	if launch.Model != "" {
		args = setFlag(args, "--model", launch.Model)
	}
	if launch.ReasoningEffort != "" {
		args = setFlag(args, "--reasoning-effort", launch.ReasoningEffort)
	}
	if launch.ContextWindow != "" {
		args = setFlag(args, "--context-window", launch.ContextWindow)
	}
	return args, nil
}

func (h *Handler) exec(frame RestartFrame, argv []string) {
	h.mu.Lock()
	if h.restarted {
		h.mu.Unlock()
		return
	}
	h.restarted = true
	h.mu.Unlock()

	h.report(frame.RequestID, statusRestarting, "restarting runtime")
	executable := h.Executable
	if executable == "" {
		executable, _ = os.Executable()
	}
	exec := h.Exec
	if exec == nil {
		exec = execSelf
	}
	env := withEnv(os.Environ(), "AUTOWONDER_RESTART_REQUEST_ID", frame.RequestID)
	if err := exec(executable, argv, env); err != nil {
		h.report(frame.RequestID, statusFailed, "restart failed: "+err.Error())
		h.mu.Lock()
		h.restarted = false
		h.mu.Unlock()
	}
}

func withEnv(env []string, key, value string) []string {
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if strings.HasPrefix(entry, key+"=") {
			continue
		}
		result = append(result, entry)
	}
	return append(result, key+"="+value)
}

func (h *Handler) busy() bool {
	return h.Inventory != nil && h.Inventory.Busy()
}

func (h *Handler) done() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.restarted
}

func (h *Handler) report(requestID, status, message string) {
	if h.Send == nil {
		return
	}
	_ = h.Send(RestartResult{Type: "EXECUTOR_RESTART_RESULT", ExecutorID: h.ExecutorID,
		RequestID: requestID, Status: status, Message: message})
}

// setFlag replaces --name VALUE / --name=VALUE or appends it.
func setFlag(args []string, name, value string) []string {
	for index := 0; index < len(args); index++ {
		if args[index] == name {
			if index+1 < len(args) {
				args[index+1] = value
				return args
			}
			return append(args, value)
		}
		if strings.HasPrefix(args[index], name+"=") {
			args[index] = name + "=" + value
			return args
		}
	}
	return append(args, name, value)
}

func setBoolFlag(args []string, name string, enabled bool) []string {
	index := -1
	for position, arg := range args {
		if arg == name {
			index = position
			break
		}
	}
	if enabled && index < 0 {
		return append(args, name)
	}
	if !enabled && index >= 0 {
		return append(args[:index], args[index+1:]...)
	}
	return args
}
