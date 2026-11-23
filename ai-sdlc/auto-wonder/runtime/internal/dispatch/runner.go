// Package dispatch executes TASK_DISPATCH frames.
package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/capsule"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/checkpoint"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/config"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/debuglog"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/hooks"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/localapi"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/mcpconfig"
	memorystore "github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/memory"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/prompt"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/protocol"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/provider"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/repos"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/requests"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/upload"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/workspacegc"
)

// Sender is the narrow write surface used by the runner.
type Sender interface {
	Send(value any) error
}

// Runner owns concurrency, ownership inventory and provider execution.
type Runner struct {
	cfg      config.Config
	client   Sender
	provider provider.Provider
	sem      chan struct{}
	outbox   *resultOutbox

	mu       sync.Mutex
	running  map[int64]context.CancelFunc
	cancels  map[int64]context.CancelFunc
	owned    map[int64]bool
	pauses   map[int64]bool
	stops    map[int64]bool
	draining bool

	guidanceMu        sync.Mutex
	guidanceAcks      map[int64]protocol.TaskSteerAck
	guidanceBatchAcks map[int64]protocol.TaskGuidanceAck
}

// New builds a runner for the configured provider.
func New(cfg config.Config, client Sender, engine provider.Provider) *Runner {
	return &Runner{
		cfg:               cfg,
		client:            client,
		provider:          engine,
		sem:               make(chan struct{}, cfg.MaxConcurrent),
		outbox:            newResultOutbox(cfg.WorkspaceRoot),
		running:           map[int64]context.CancelFunc{},
		cancels:           map[int64]context.CancelFunc{},
		owned:             map[int64]bool{},
		pauses:            map[int64]bool{},
		stops:             map[int64]bool{},
		guidanceAcks:      map[int64]protocol.TaskSteerAck{},
		guidanceBatchAcks: map[int64]protocol.TaskGuidanceAck{},
	}
}

// ReplayOutbox resends dispatch frames that were persisted but not acknowledged.
func (r *Runner) ReplayOutbox() {
	frames, err := r.outbox.List()
	if err != nil {
		slog.Warn("replay dispatch outbox failed", "error", err)
		return
	}
	for _, frame := range frames {
		r.send(frame)
	}
}

// HandleResultAck removes a persisted terminal frame after durable acceptance.
func (r *Runner) HandleResultAck(raw []byte) {
	var ack struct {
		DispatchID int64 `json:"dispatchId"`
		Accepted   bool  `json:"accepted"`
	}
	if json.Unmarshal(raw, &ack) != nil || ack.DispatchID <= 0 || !ack.Accepted {
		return
	}
	if err := r.outbox.Delete(ack.DispatchID); err != nil {
		slog.Warn("delete dispatch outbox result failed", "dispatchId", ack.DispatchID, "error", err)
	}
}

// Inventory returns the dispatch_inventory_v1 snapshot for heartbeats.
func (r *Runner) Inventory() protocol.InventorySnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	running := make([]int64, 0, len(r.running))
	owned := make([]int64, 0, len(r.owned))
	for id := range r.running {
		running = append(running, id)
	}
	for id := range r.owned {
		owned = append(owned, id)
	}
	return protocol.InventorySnapshot{Capacity: r.cfg.MaxConcurrent, Running: running, Owned: owned}
}

// Busy reports whether this executor still owns running work.
func (r *Runner) Busy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.running) > 0 || len(r.owned) > 0
}

// Owns reports whether this executor currently owns the dispatch ID.
func (r *Runner) Owns(dispatchID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.owned[dispatchID]
}

// SetDraining stops accepting new dispatches while a WHEN_IDLE restart waits.
func (r *Runner) SetDraining(draining bool) {
	r.mu.Lock()
	r.draining = draining
	r.mu.Unlock()
}

// CancelAll cancels every in-flight dispatch for a FORCE restart.
func (r *Runner) CancelAll() {
	r.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(r.cancels))
	for _, cancel := range r.cancels {
		cancels = append(cancels, cancel)
	}
	r.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// WaitIdle waits until no dispatch is running or owned.
func (r *Runner) WaitIdle(ctx context.Context) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !r.Busy() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Handle routes one inbound frame. Unknown frames are ignored so the runtime
// stays forward-compatible with new server features.
func (r *Runner) Handle(parent context.Context, raw []byte) {
	var envelope protocol.Frame
	if json.Unmarshal(raw, &envelope) != nil {
		return
	}
	switch envelope.Type {
	case "TASK_DISPATCH":
		var frame protocol.Dispatch
		if json.Unmarshal(raw, &frame) != nil || frame.DispatchID == 0 {
			return
		}
		if r.outbox.Has(frame.DispatchID) {
			r.send(protocol.Ack{Type: "TASK_ACK", DispatchID: frame.DispatchID})
			if pending, err := r.outbox.Get(frame.DispatchID); err == nil {
				r.send(pending)
			} else {
				slog.Warn("read pending dispatch result failed", "dispatchId", frame.DispatchID, "error", err)
			}
			return
		}
		r.mu.Lock()
		draining := r.draining
		r.mu.Unlock()
		if draining {
			slog.Info("dispatch ignored while draining for restart", "dispatchId", frame.DispatchID)
			return
		}
		go r.execute(parent, frame)
	case "TASK_PAUSE":
		var frame struct {
			DispatchID int64 `json:"dispatchId"`
			Cancel     bool  `json:"cancel"`
		}
		if json.Unmarshal(raw, &frame) == nil {
			r.mu.Lock()
			if frame.Cancel {
				r.stops[frame.DispatchID] = true
			} else {
				r.pauses[frame.DispatchID] = true
			}
			r.mu.Unlock()
			r.cancel(frame.DispatchID)
		}
	case "TASK_RESULT_ACK":
		r.HandleResultAck(raw)
	case "TASK_STEER":
		var frame protocol.TaskSteer
		if json.Unmarshal(raw, &frame) == nil {
			r.handleTaskSteer(parent, frame)
		}
	case "TASK_GUIDANCE":
		var frame protocol.TaskGuidance
		if json.Unmarshal(raw, &frame) == nil {
			r.handleTaskGuidance(parent, frame)
		}
	}
}

func (r *Runner) handleTaskSteer(ctx context.Context, frame protocol.TaskSteer) {
	if frame.DispatchID == 0 || frame.GuidanceID == 0 {
		return
	}

	r.guidanceMu.Lock()
	defer r.guidanceMu.Unlock()

	if ack, ok := r.guidanceAcks[frame.GuidanceID]; ok {
		r.send(ack)
		return
	}

	r.mu.Lock()
	_, active := r.running[frame.DispatchID]
	r.mu.Unlock()
	if !active {
		ack := protocol.TaskSteerAck{
			Type:       "TASK_STEER_ACK",
			GuidanceID: frame.GuidanceID,
			DispatchID: frame.DispatchID,
			Status:     "FAILED",
			Error:      "dispatch is not active",
		}
		r.guidanceAcks[frame.GuidanceID] = ack
		r.send(ack)
		return
	}
	steerable, ok := r.provider.(provider.Steerable)
	if !ok {
		ack := protocol.TaskSteerAck{
			Type:       "TASK_STEER_ACK",
			GuidanceID: frame.GuidanceID,
			DispatchID: frame.DispatchID,
			Status:     "FAILED",
			Error:      "provider does not support steering",
		}
		r.guidanceAcks[frame.GuidanceID] = ack
		r.send(ack)
		return
	}
	if err := steerable.Steer(ctx, frame.DispatchID, frame.Content); err != nil {
		ack := protocol.TaskSteerAck{
			Type:       "TASK_STEER_ACK",
			GuidanceID: frame.GuidanceID,
			DispatchID: frame.DispatchID,
			Status:     "FAILED",
			Error:      err.Error(),
		}
		r.guidanceAcks[frame.GuidanceID] = ack
		r.send(ack)
		return
	}
	ack := protocol.TaskSteerAck{
		Type:       "TASK_STEER_ACK",
		GuidanceID: frame.GuidanceID,
		DispatchID: frame.DispatchID,
		Status:     "APPLIED",
	}
	r.guidanceAcks[frame.GuidanceID] = ack
	r.send(ack)
}

func (r *Runner) handleTaskGuidance(ctx context.Context, frame protocol.TaskGuidance) {
	if frame.DispatchID == 0 || frame.GuidanceID == 0 {
		return
	}

	r.guidanceMu.Lock()
	defer r.guidanceMu.Unlock()

	if ack, ok := r.guidanceBatchAcks[frame.GuidanceID]; ok {
		r.send(ack)
		return
	}

	r.mu.Lock()
	_, active := r.running[frame.DispatchID]
	r.mu.Unlock()
	if !active {
		ack := protocol.TaskGuidanceAck{
			Type:       "TASK_GUIDANCE_ACK",
			GuidanceID: frame.GuidanceID,
			DispatchID: frame.DispatchID,
			Status:     "FAILED",
			Error:      "dispatch is not active",
		}
		r.guidanceBatchAcks[frame.GuidanceID] = ack
		r.send(ack)
		return
	}
	steerable, ok := r.provider.(provider.Steerable)
	if !ok {
		ack := protocol.TaskGuidanceAck{
			Type:       "TASK_GUIDANCE_ACK",
			GuidanceID: frame.GuidanceID,
			DispatchID: frame.DispatchID,
			Status:     "FAILED",
			Error:      "provider does not support steering",
		}
		r.guidanceBatchAcks[frame.GuidanceID] = ack
		r.send(ack)
		return
	}
	if err := steerable.Steer(ctx, frame.DispatchID, frame.Content); err != nil {
		ack := protocol.TaskGuidanceAck{
			Type:       "TASK_GUIDANCE_ACK",
			GuidanceID: frame.GuidanceID,
			DispatchID: frame.DispatchID,
			Status:     "FAILED",
			Error:      err.Error(),
		}
		r.guidanceBatchAcks[frame.GuidanceID] = ack
		r.send(ack)
		return
	}
	ack := protocol.TaskGuidanceAck{
		Type:       "TASK_GUIDANCE_ACK",
		GuidanceID: frame.GuidanceID,
		DispatchID: frame.DispatchID,
		Status:     "APPLIED",
	}
	r.guidanceBatchAcks[frame.GuidanceID] = ack
	r.send(ack)
}

func (r *Runner) cancel(dispatchID int64) {
	r.mu.Lock()
	cancel := r.cancels[dispatchID]
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (r *Runner) execute(parent context.Context, frame protocol.Dispatch) {
	r.mu.Lock()
	if r.owned[frame.DispatchID] {
		r.mu.Unlock()
		slog.Warn("duplicate dispatch ignored", "dispatchId", frame.DispatchID)
		return
	}
	r.owned[frame.DispatchID] = true
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(parent, 30*time.Minute)
	defer cancel()
	r.mu.Lock()
	r.cancels[frame.DispatchID] = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.cancels, frame.DispatchID)
		delete(r.running, frame.DispatchID)
		delete(r.owned, frame.DispatchID)
		delete(r.pauses, frame.DispatchID)
		delete(r.stops, frame.DispatchID)
		r.mu.Unlock()
	}()

	r.send(protocol.Ack{Type: "TASK_ACK", DispatchID: frame.DispatchID})

	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return
	}

	r.mu.Lock()
	r.running[frame.DispatchID] = cancel
	r.mu.Unlock()

	outcome := r.runOne(ctx, frame)
	r.mu.Lock()
	paused := r.pauses[frame.DispatchID]
	stopped := r.stops[frame.DispatchID]
	r.mu.Unlock()
	if stopped {
		markTerminal(outcome.box, "CANCELED")
		slog.Info("dispatch canceled by scheduler", "dispatchId", frame.DispatchID)
		return
	}
	if paused {
		r.pause(ctx, frame, outcome)
		return
	}
	if outcome.result.Success && frame.CheckpointUploadPath != "" {
		uploadCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		err := r.uploadCompletionCheckpoint(uploadCtx, frame, &outcome)
		cancel()
		if err != nil {
			outcome.result = failedResult(outcome.result, fmt.Errorf("completion checkpoint: %w", err))
		}
	}
	status := "FAILED"
	if outcome.result.Success {
		status = "SUCCESS"
	}
	markTerminal(outcome.box, status)
	r.sendTerminal(outcome.result)
}

// uploadCompletionCheckpoint pins the successful result identity to a durable
// checkpoint before TASK_RESULT is sent. The server rejects V2 success results
// whose completion checkpoint cannot be matched to the same result ID.
func (r *Runner) uploadCompletionCheckpoint(ctx context.Context, frame protocol.Dispatch, outcome *runOutcome) error {
	document, err := checkpoint.FromCheckouts(ctx, outcome.checkouts)
	if err != nil {
		return err
	}
	seq := frame.ResumeCheckpointSeq + 1
	if seq <= 0 {
		seq = 1
	}
	document.DispatchID = strconv.FormatInt(frame.DispatchID, 10)
	document.CheckpointSeq = seq
	archive, digest, err := checkpoint.Archive(document)
	if err != nil {
		return err
	}
	stepID := ""
	if frame.SDLCStepID != 0 {
		stepID = strconv.FormatInt(frame.SDLCStepID, 10)
	}
	endpoint := resolvePath(httpBase(r.cfg.ServerWSURL), frame.CheckpointUploadPath)
	receipt, err := checkpoint.Upload(ctx, endpoint, r.cfg.ExecutorToken, frame.DispatchID, seq,
		r.provider.Name(), outcome.sessionID, r.runtimeID(), stepID, "COMPLETION",
		outcome.result.ResultID, archive, digest)
	if err != nil {
		return err
	}
	if receipt.CheckpointSeq > 0 {
		seq = receipt.CheckpointSeq
	}
	if receipt.SHA256 != "" {
		digest = receipt.SHA256
	}
	outcome.result.CheckpointSeq = seq
	outcome.result.CheckpointSHA256 = digest
	return nil
}

func markTerminal(box *capsule.Capsule, status string) {
	if box == nil {
		return
	}
	if err := workspacegc.WriteTerminal(box.Root, status, time.Now()); err != nil {
		slog.Warn("write workspace terminal state failed", "root", box.Root, "error", err)
	}
}

type runOutcome struct {
	result      protocol.Result
	box         *capsule.Capsule
	checkouts   []repos.Checkout
	sessionID   string
	hooks       *hooks.Set
	hookContext hooks.Context
}

func (r *Runner) runOne(ctx context.Context, frame protocol.Dispatch) runOutcome {
	var recorder *debuglog.Recorder
	if frame.DebugLog != nil && frame.DebugLog.Enabled {
		created, err := debuglog.New(filepath.Join(r.cfg.WorkspaceRoot, fmt.Sprintf("dispatch-%d", frame.DispatchID)),
			frame.DebugLog.MaxBytes)
		if err != nil {
			slog.Warn("create debug log failed", "dispatchId", frame.DispatchID, "error", err)
		} else {
			recorder = created
			recorder.AddSecret(r.cfg.ExecutorToken)
			recorder.AddSecret(frame.DispatchMCPToken)
			for _, secret := range frame.MCPSecrets {
				recorder.AddSecret(secret)
			}
			for key, value := range frame.EnvironmentVariables {
				upper := strings.ToUpper(key)
				if strings.Contains(upper, "TOKEN") || strings.Contains(upper, "SECRET") ||
					strings.Contains(upper, "PASSWORD") || strings.Contains(upper, "API_KEY") {
					recorder.AddSecret(value)
				}
			}
			defer recorder.Close()
		}
	}
	outcome := r.runCore(ctx, frame, recorder)
	outcome = r.runCleanup(outcome, recorder)
	if recorder != nil {
		status := "FAILED"
		if outcome.result.Success {
			status = "SUCCEEDED"
		}
		recorder.Printf("result success=%v error=%s", outcome.result.Success, outcome.result.Error)
		uploadCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		outcome.result.DebugLog = recorder.Upload(uploadCtx, httpBase(r.cfg.ServerWSURL), r.cfg.ExecutorToken,
			frame.DispatchID, status, frame.DebugLog.RelayFallback, frame.ArtifactUploadPath)
	}
	return outcome
}

func (r *Runner) runCore(ctx context.Context, frame protocol.Dispatch, recorder *debuglog.Recorder) runOutcome {
	progress := newProgress(r.client, frame.DispatchID, recorder)
	result := protocol.Result{
		Type:                  "TASK_RESULT",
		DispatchID:            frame.DispatchID,
		ResultProtocolVersion: 2,
		ResultID:              newResultID(),
	}
	if err := provider.ValidateTaskEnvironment(frame.EnvironmentVariables); err != nil {
		return runOutcome{result: failedResult(result, fmt.Errorf("environment variables: %w", err))}
	}

	progress.emit("capsule.downloading")
	box, err := capsule.PrepareSigned(ctx, frame.DownloadURL, frame.MD5, r.cfg.WorkspaceRoot, frame.DispatchID,
		capsule.Signature{
			PackageID:     frame.PackageID,
			ArchiveSHA256: frame.Checksum,
			Issuer:        frame.Issuer,
			SignatureRef:  frame.SignatureRef,
			Signature:     frame.Signature,
			Algorithm:     frame.SignatureAlgorithm,
			PublicKey:     frame.SignaturePublicKey,
			ExpiresAt:     frame.ExpiresAt,
			RequireSigned: r.cfg.RequirePackageSignature,
		})
	if err != nil {
		return runOutcome{result: failedResult(result, fmt.Errorf("capsule: %w", err))}
	}
	progress.emit("capsule.ready")

	hookSet, err := hooks.Load(box.PackageDir)
	if err != nil {
		return runOutcome{result: failedResult(result, fmt.Errorf("hooks: %w", err)), box: box}
	}
	hookContext := hooks.Context{
		RootDir:      box.Root,
		PackageDir:   box.PackageDir,
		WorkspaceDir: box.WorkspaceDir,
		StateDir:     filepath.Join(box.Root, "state"),
		ArtifactsDir: box.ArtifactsDir,
		DispatchID:   frame.DispatchID,
		StepID:       strconv.FormatInt(frame.SDLCStepID, 10),
		Attempt:      max(frame.Attempt, 1),
		Provider:     r.provider.Name(),
		DispatchEnv:  frame.EnvironmentVariables,
	}
	if err := os.MkdirAll(hookContext.StateDir, 0o755); err != nil {
		return runOutcome{result: failedResult(result, fmt.Errorf("hook state dir: %w", err)), box: box}
	}
	if err := r.runHookPhase(ctx, hookSet, hookContext, hooks.BeforeRepoPrepare, progress, recorder); err != nil {
		return runOutcome{result: failedResult(result, err), box: box, hooks: hookSet, hookContext: hookContext}
	}

	specs, err := repos.Load(box.PackageDir)
	if err != nil {
		return runOutcome{result: failedResult(result, fmt.Errorf("repositories: %w", err)), box: box,
			hooks: hookSet, hookContext: hookContext}
	}
	eagerSpecs, lazySpecs := repos.SplitByMode(specs)
	if len(eagerSpecs) > 0 {
		progress.emit(fmt.Sprintf("repositories.preparing count=%d", len(eagerSpecs)))
	}
	checkouts, err := repos.Materialize(ctx, box.WorkspaceDir, eagerSpecs)
	if err != nil {
		return runOutcome{result: failedResult(result, fmt.Errorf("repositories: %w", err)), box: box,
			hooks: hookSet, hookContext: hookContext}
	}
	progress.emit(fmt.Sprintf("repositories.ready count=%d lazy=%d", len(checkouts), len(lazySpecs)))

	if err := r.installGitHooks(box, hookSet, hookContext, checkouts); err != nil {
		return runOutcome{result: failedResult(result, fmt.Errorf("git hooks: %w", err)), box: box,
			checkouts: checkouts, hooks: hookSet, hookContext: hookContext}
	}
	hookContext.TriggeringRepoDir = ""
	hookContext.TriggeringRepoName = ""
	if err := r.runHookPhase(ctx, hookSet, hookContext, hooks.BeforeStep, progress, recorder); err != nil {
		return runOutcome{result: failedResult(result, err), box: box, checkouts: checkouts,
			hooks: hookSet, hookContext: hookContext}
	}

	claudeSettingsPath := ""
	if r.provider.Name() == "claude" {
		path, settingsErr := r.claudeToolSettings(box, hookSet, hookContext, checkouts)
		if settingsErr != nil {
			return runOutcome{result: failedResult(result, fmt.Errorf("claude tool hooks: %w", settingsErr)),
				box: box, checkouts: checkouts, hooks: hookSet, hookContext: hookContext}
		}
		claudeSettingsPath = path
	}

	if r.cfg.MemoryMode == "platform" && frame.MemorySnapshotPath != "" && frame.DispatchMCPToken != "" {
		snapshot, err := memorystore.FetchSnapshot(ctx,
			resolvePath(httpBase(r.cfg.ServerWSURL), frame.MemorySnapshotPath), frame.DispatchMCPToken)
		if err != nil {
			slog.Warn("platform memory snapshot unavailable", "dispatchId", frame.DispatchID, "error", err)
		} else if err := memorystore.WriteSnapshot(box.PackageDir, snapshot); err != nil {
			slog.Warn("write platform memory snapshot failed", "dispatchId", frame.DispatchID, "error", err)
		} else if snapshot != "" {
			progress.emit("memory.snapshot.ready")
		}
	}

	if heads, err := r.resumeHeads(ctx, frame); err != nil {
		return runOutcome{result: failedResult(result, fmt.Errorf("checkpoint resume: %w", err)), box: box,
			checkouts: checkouts, hooks: hookSet, hookContext: hookContext}
	} else if len(heads) > 0 {
		if err := repos.Restore(ctx, checkouts, heads); err != nil {
			return runOutcome{result: failedResult(result, fmt.Errorf("checkpoint resume: %w", err)), box: box,
				checkouts: checkouts, hooks: hookSet, hookContext: hookContext}
		}
		progress.emit(fmt.Sprintf("checkpoint.restored repositories=%d", len(heads)))
	}

	agentEnv := copyEnvironment(frame.EnvironmentVariables)
	requestClient := &requests.Client{BaseURL: httpBase(r.cfg.ServerWSURL), Token: r.cfg.ExecutorToken,
		DispatchID: frame.DispatchID}
	memoryEndpoints := localapi.MemoryEndpoints{}
	if frame.DispatchMCPToken != "" && frame.MemorySnapshotPath != "" {
		memoryEndpoints = localapi.MemoryEndpoints{
			Client:   memorystore.New(httpBase(r.cfg.ServerWSURL), frame.DispatchMCPToken, frame.DispatchID),
			Changes:  frame.MemoryChangesPath,
			Mutation: frame.MemoryMutationPath,
			Recall:   frame.MemoryRecallPath,
			Claim:    memorystore.MaintenanceEndpoint(frame.MemorySnapshotPath, "claims"),
			Renew:    memorystore.MaintenanceEndpoint(frame.MemorySnapshotPath, "renewals"),
			Release:  memorystore.MaintenanceEndpoint(frame.MemorySnapshotPath, "releases"),
		}
	}
	completionTracker := localapi.NewCompletionTracker()
	local, localErr := localapi.Start(frame.DispatchID, strconv.FormatInt(frame.SDLCStepID, 10),
		max(frame.Attempt, 1), requestClient, memoryEndpoints, localapi.StepEventEndpoints{
			Client: requestClient, Tracker: completionTracker,
			StepID: strconv.FormatInt(frame.SDLCStepID, 10), StepAttempt: max(frame.Attempt, 1),
			AllowLocalAccept: frame.ArtifactProtocolVersion < 2,
		})
	if localErr != nil {
		if frame.ArtifactProtocolVersion >= 2 {
			return runOutcome{result: failedResult(result, fmt.Errorf("local step-event API: %w", localErr)),
				box: box, checkouts: checkouts, hooks: hookSet, hookContext: hookContext}
		}
		slog.Warn("local request API unavailable", "dispatchId", frame.DispatchID, "error", localErr)
	} else {
		defer local.Close()
		if r.cfg.ReasoningEffort != "" {
			agentEnv["AUTOWONDER_REASONING_EFFORT"] = r.cfg.ReasoningEffort
		}
		if r.cfg.ContextWindow != "" {
			agentEnv["AUTOWONDER_CONTEXT_WINDOW"] = r.cfg.ContextWindow
		}
		agentEnv["AUTOWONDER_LOCAL_API_ENDPOINT"] = local.Endpoint
		agentEnv["AUTOWONDER_DISPATCH_ID"] = strconv.FormatInt(frame.DispatchID, 10)
		agentEnv["AUTOWONDER_STEP_ID"] = strconv.FormatInt(frame.SDLCStepID, 10)
		agentEnv["AUTOWONDER_STEP_ATTEMPT"] = strconv.Itoa(max(frame.Attempt, 1))
		if memoryEndpoints.Client != nil {
			agentEnv["AUTOWONDER_LOCAL_MEMORY_ENDPOINT"] = local.Endpoint + "/memory"
			agentEnv["AUTOWONDER_MEMORY_MODE"] = "platform"
		}
	}

	agentPrompt, err := prompt.Build(box, prompt.BuildWork{
		Checkouts: checkouts, LazyRepos: lazySpecs, LocalAPIEndpoint: agentEnv["AUTOWONDER_LOCAL_API_ENDPOINT"],
		DispatchID: frame.DispatchID, StepID: strconv.FormatInt(frame.SDLCStepID, 10),
		StepAttempt: max(frame.Attempt, 1), ArtifactProtocolVersion: frame.ArtifactProtocolVersion,
	})
	if err != nil {
		return runOutcome{result: failedResult(result, fmt.Errorf("prompt: %w", err)), box: box,
			checkouts: checkouts, hooks: hookSet, hookContext: hookContext}
	}
	if frame.ResumeSessionID != "" {
		agentPrompt += "\n\n## Resume context\nContinue session " + frame.ResumeSessionID + " from the previous attempt."
	}

	mcpPath, err := mcpconfig.Write(box.PackageDir, httpBase(r.cfg.ServerWSURL),
		frame.DispatchMCPToken, frame.MCPSecrets, filepath.Join(box.Root, "mcp.config.json"))
	if err != nil {
		return runOutcome{result: failedResult(result, fmt.Errorf("mcp config: %w", err)), box: box,
			checkouts: checkouts, hooks: hookSet, hookContext: hookContext}
	}

	requestInput := func(ctx context.Context, input provider.InputRequest) (provider.InputAnswer, error) {
		context := input.Context
		if input.Kind == "APPROVAL" {
			context = copyContext(context)
			if context["fingerprint"] == "" {
				context["fingerprint"] = requests.Fingerprint(input)
			}
		}
		answer, err := requestClient.Ask(ctx, requests.Ask{
			Kind: input.Kind, Key: input.Key, StepID: strconv.FormatInt(frame.SDLCStepID, 10),
			StepAttempt: max(frame.Attempt, 1), Message: input.Message, Schema: input.Schema,
			Context: context, TimeoutSeconds: input.TimeoutSeconds,
		})
		if err != nil {
			return provider.InputAnswer{}, err
		}
		return provider.InputAnswer{Action: answer.Action, Content: answer.Content}, nil
	}

	if err := r.runHookPhase(ctx, hookSet, hookContext, hooks.BeforeAgentStart, progress, recorder); err != nil {
		return runOutcome{result: failedResult(result, err), box: box, checkouts: checkouts,
			hooks: hookSet, hookContext: hookContext}
	}
	progress.emit(fmt.Sprintf("agent.starting provider=%s repositories=%d", r.provider.Name(), len(checkouts)))
	engineResult, runErr := r.provider.Run(ctx, provider.Request{
		AgentPath:          r.cfg.AgentPath,
		Prompt:             agentPrompt,
		WorkDir:            box.WorkspaceDir,
		ArtifactsDir:       box.ArtifactsDir,
		Model:              modelFor(r.cfg, frame),
		ReasoningEffort:    r.cfg.ReasoningEffort,
		ContextWindow:      r.cfg.ContextWindow,
		SessionID:          frame.ResumeSessionID,
		MCPConfigPath:      mcpPath,
		ClaudeSettingsPath: claudeSettingsPath,
		Env:                agentEnv,
		Unattended:         r.cfg.Unattended,
		PiProvider:         r.cfg.PiProvider,
		PermissionFlag:     r.cfg.OpenCodePermissionFlag,
		PromptFlag:         r.cfg.OpenCodePromptFlag,
		RequestInput:       requestInput,
	}, func(event provider.Event) {
		recorder.Printf("agent.%s %s", event.Kind, event.Text)
		switch event.Kind {
		case "text":
			progress.emit("agent.turn " + event.Text)
		case "tool":
			progress.emit("agent.tool " + event.Text)
		case "tool_start":
			progress.emit("agent.tool " + firstText(event.ToolTitle, event.ToolName))
		case "tool_update":
			if event.ToolStatus == "completed" || event.ToolStatus == "failed" ||
				event.ToolStatus == "error" || event.ToolStatus == "cancelled" {
				progress.emit("agent.tool_result " + firstText(event.ToolTitle, event.ToolName) +
					" status=" + event.ToolStatus)
			}
		case "plan":
			progress.emit("agent.plan")
		case "error":
			progress.emit("agent.error " + event.Text)
		}
	})
	progress.emit(fmt.Sprintf("agent.finished session=%s", engineResult.SessionID))

	exitCtx, exitCancel := hookExitContext(ctx)
	exitErr := r.runHookPhase(exitCtx, hookSet, hookContext, hooks.AfterAgentExit, progress, recorder)
	exitCancel()
	if exitErr != nil && runErr == nil {
		runErr = exitErr
	}

	revisionPath := filepath.Join(box.ArtifactsDir, "deliverables", "runtime-source-revision.json")
	if err := repos.WriteRevision(ctx, checkouts, revisionPath); err != nil {
		if runErr == nil {
			runErr = fmt.Errorf("source revision: %w", err)
		} else {
			slog.Warn("write source revision failed", "dispatchId", frame.DispatchID, "error", err)
		}
	}
	if r.cfg.UploadArtifacts && frame.ArtifactUploadPath != "" {
		endpoint := resolvePath(httpBase(r.cfg.ServerWSURL), frame.ArtifactUploadPath)
		explicit := completionTracker.Completed()
		if !explicit && frame.ArtifactProtocolVersion >= 2 {
			if runErr == nil {
				runErr = fmt.Errorf("completion intent missing")
			}
		} else if explicit {
			refs := append([]string(nil), completionTracker.Refs()...)
			refs = append(refs, "deliverables/runtime-source-revision.json")
			count, err := upload.ArtifactsSelected(ctx, endpoint, r.cfg.ExecutorToken, box.ArtifactsDir, refs)
			if err != nil {
				if runErr == nil {
					runErr = fmt.Errorf("artifact upload: %w", err)
				}
			} else if count > 0 {
				progress.emit(fmt.Sprintf("artifacts.uploaded count=%d mode=explicit", count))
			}
		} else if count, err := upload.Artifacts(ctx, endpoint, r.cfg.ExecutorToken, box.ArtifactsDir); err != nil {
			slog.Warn("artifact upload failed", "dispatchId", frame.DispatchID, "error", err)
		} else if count > 0 {
			progress.emit(fmt.Sprintf("artifacts.uploaded count=%d", count))
		}
	}

	if runErr != nil {
		return runOutcome{result: failedResult(result, runErr), box: box, checkouts: checkouts,
			sessionID: engineResult.SessionID, hooks: hookSet, hookContext: hookContext}
	}
	if _, err := os.Stat(filepath.Join(box.ArtifactsDir, "FAILURE")); err == nil {
		return runOutcome{result: failedResult(result, fmt.Errorf("agent signalled FAILURE")), box: box,
			checkouts: checkouts, sessionID: engineResult.SessionID, hooks: hookSet, hookContext: hookContext}
	}

	sendHandoff(r.client, box, frame)
	result.Success = true
	result.ResultSummary = engineResult.Summary
	if result.ResultSummary == "" {
		result.ResultSummary = "agent completed"
	}
	return runOutcome{result: result, box: box, checkouts: checkouts, sessionID: engineResult.SessionID,
		hooks: hookSet, hookContext: hookContext}
}

func copyEnvironment(source map[string]string) map[string]string {
	result := make(map[string]string, len(source)+4)
	for key, value := range source {
		result[key] = value
	}
	return result
}

func copyContext(source map[string]any) map[string]any {
	result := make(map[string]any, len(source)+1)
	for key, value := range source {
		result[key] = value
	}
	return result
}

func (r *Runner) pause(ctx context.Context, frame protocol.Dispatch, outcome runOutcome) {
	// TASK_PAUSE cancels the dispatch context, so checkpoint capture and upload need a fresh bounded context.
	uploadCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if frame.CheckpointUploadPath == "" {
		r.pauseFailed(frame.DispatchID, "dispatch did not provide a checkpoint upload path")
		return
	}
	document, err := checkpoint.FromCheckouts(uploadCtx, outcome.checkouts)
	if err != nil {
		r.pauseFailed(frame.DispatchID, err.Error())
		return
	}
	if len(document.Repos) == 0 {
		r.pauseFailed(frame.DispatchID, "dispatch has no repository state to checkpoint")
		return
	}
	archive, digest, err := checkpoint.Archive(document)
	if err != nil {
		r.pauseFailed(frame.DispatchID, err.Error())
		return
	}
	if outcome.box != nil {
		if err := checkpoint.Save(filepath.Join(outcome.box.Root, "checkpoint.tar.gz"), archive); err != nil {
			slog.Warn("save checkpoint failed", "dispatchId", frame.DispatchID, "error", err)
		}
	}
	seq := frame.ResumeCheckpointSeq + 1
	endpoint := resolvePath(httpBase(r.cfg.ServerWSURL), frame.CheckpointUploadPath)
	stepID := ""
	if frame.SDLCStepID != 0 {
		stepID = strconv.FormatInt(frame.SDLCStepID, 10)
	}
	receipt, err := checkpoint.Upload(uploadCtx, endpoint, r.cfg.ExecutorToken, frame.DispatchID, seq,
		r.provider.Name(), outcome.sessionID, r.runtimeID(), stepID, "RECOVERY", "", archive, digest)
	if err != nil {
		r.pauseFailed(frame.DispatchID, err.Error())
		return
	}
	if receipt.CheckpointSeq > 0 {
		seq = receipt.CheckpointSeq
	}
	if receipt.SHA256 != "" {
		digest = receipt.SHA256
	}
	markTerminal(outcome.box, "PAUSED")
	r.sendTerminal(protocol.Paused{
		Type: "TASK_PAUSED", DispatchID: frame.DispatchID, CheckpointSeq: seq, CheckpointSHA256: digest,
	})
}

func (r *Runner) pauseFailed(dispatchID int64, message string) {
	r.send(protocol.PauseFailed{Type: "TASK_PAUSE_FAILED", DispatchID: dispatchID, Error: truncate(message, 500)})
}

func (r *Runner) resumeHeads(ctx context.Context, frame protocol.Dispatch) (map[string]string, error) {
	candidates := make([]protocol.ResumeCheckpointCandidate, 0, len(frame.ResumeCheckpointCandidates)+1)
	if frame.ResumeCheckpointURL != "" {
		candidates = append(candidates, protocol.ResumeCheckpointCandidate{
			DownloadURL: frame.ResumeCheckpointURL, SHA256: frame.ResumeCheckpointSHA256,
			CheckpointSeq: frame.ResumeCheckpointSeq,
		})
	}
	candidates = append(candidates, frame.ResumeCheckpointCandidates...)
	if len(candidates) == 0 {
		return nil, nil
	}
	var lastErr error
	for _, candidate := range candidates {
		endpoint := resolvePath(httpBase(r.cfg.ServerWSURL), candidate.DownloadURL)
		document, err := checkpoint.Download(ctx, endpoint, candidate.SHA256)
		if err != nil {
			lastErr = err
			continue
		}
		heads := make(map[string]string, len(document.Repos))
		for _, repo := range document.Repos {
			if repo.Name != "" && repo.HeadCommit != "" {
				heads[repo.Name] = repo.HeadCommit
			}
		}
		return heads, nil
	}
	return nil, lastErr
}

func (r *Runner) runtimeID() string {
	if r.cfg.DaemonName != "" {
		return r.cfg.DaemonName
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "autowonder-runtime"
	}
	return host
}

func (r *Runner) send(value any) {
	if err := r.client.Send(value); err != nil {
		slog.Error("send frame", "error", err)
	}
}

func (r *Runner) sendTerminal(value any) {
	if err := r.outbox.Put(value); err != nil {
		slog.Error("persist terminal dispatch frame", "error", err)
		return
	}
	r.send(value)
}

func failedResult(result protocol.Result, err error) protocol.Result {
	result.Success = false
	result.Error = err.Error()
	result.ResultSummary = truncate(err.Error(), 500)
	return result
}

type progressReporter struct {
	client     Sender
	dispatchID int64
	recorder   *debuglog.Recorder
	mu         sync.Mutex
	last       time.Time
}

func newProgress(client Sender, dispatchID int64, recorder *debuglog.Recorder) *progressReporter {
	return &progressReporter{client: client, dispatchID: dispatchID, recorder: recorder}
}

func (p *progressReporter) emit(summary string) {
	p.recorder.Printf("progress %s", summary)
	p.mu.Lock()
	if time.Since(p.last) < 2*time.Second {
		p.mu.Unlock()
		return
	}
	p.last = time.Now()
	p.mu.Unlock()
	_ = p.client.Send(protocol.Progress{
		Type:       "TASK_PROGRESS",
		DispatchID: p.dispatchID,
		Log:        truncate(summary, 300),
	})
}

func sendHandoff(client Sender, box *capsule.Capsule, frame protocol.Dispatch) {
	raw, err := os.ReadFile(filepath.Join(box.ArtifactsDir, "handoff-proposal.json"))
	if err != nil {
		return
	}
	var proposal struct {
		NextOwner string `json:"nextOwner"`
		NextRole  string `json:"nextRole"`
		ToType    string `json:"toType"`
		Summary   string `json:"summary"`
	}
	if json.Unmarshal(raw, &proposal) != nil || proposal.NextOwner == "" {
		return
	}
	toType := proposal.ToType
	if toType == "" {
		toType = "AGENT"
	}
	nextRole := proposal.NextRole
	if nextRole == "" {
		nextRole = proposal.NextOwner
	}
	_ = client.Send(protocol.Handoff{
		Type:       "TASK_HANDOFF",
		DispatchID: frame.DispatchID,
		WorkitemID: frame.WorkitemID,
		To:         proposal.NextOwner,
		ToType:     toType,
		NextRole:   nextRole,
		Reason:     proposal.Summary,
	})
}

func httpBase(wsURL string) string {
	base := strings.TrimSuffix(wsURL, "/")
	base = strings.TrimSuffix(base, "/ws/executor")
	base = strings.Replace(base, "wss://", "https://", 1)
	base = strings.Replace(base, "ws://", "http://", 1)
	return base
}

func resolvePath(base, path string) string {
	if parsed, err := url.Parse(path); err == nil && parsed.IsAbs() {
		return path
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}

func modelFor(cfg config.Config, frame protocol.Dispatch) string {
	if frame.ModelSelection != nil && frame.ModelSelection.Model != "" {
		if frame.ModelSelection.Provider != "" && !strings.Contains(frame.ModelSelection.Model, "/") {
			return frame.ModelSelection.Provider + "/" + frame.ModelSelection.Model
		}
		return frame.ModelSelection.Model
	}
	return cfg.AgentModel
}

func newResultID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	buffer[6] = (buffer[6] & 0x0f) | 0x40
	buffer[8] = (buffer[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(buffer)
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[0:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:32])
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func (r *Runner) runHookPhase(ctx context.Context, set *hooks.Set, hookContext hooks.Context,
	trigger string, progress *progressReporter, recorder *debuglog.Recorder) error {
	if !set.Has(trigger) {
		return nil
	}
	results, err := set.Run(ctx, trigger, hookContext)
	for _, result := range results {
		status := "ok"
		if result.Skipped {
			status = "skipped"
		} else if !result.Success {
			status = "failed"
		}
		message := fmt.Sprintf("hook.%s %s %s", trigger, result.Name, status)
		progress.emit(message)
		if recorder != nil {
			recorder.Printf("%s exit=%d error=%s", message, result.ExitCode, result.Error)
		}
		if !result.Success && !result.Skipped {
			slog.Warn("runtime hook failed", "trigger", trigger, "hook", result.Name,
				"exitCode", result.ExitCode, "logDir", result.LogDir, "error", result.Error)
		}
	}
	if err != nil {
		return fmt.Errorf("%s hook: %w", trigger, err)
	}
	return nil
}

func (r *Runner) installGitHooks(box *capsule.Capsule, set *hooks.Set, hookContext hooks.Context,
	checkouts []repos.Checkout) error {
	if !set.Has(hooks.BeforeCommit) && !set.Has(hooks.BeforePush) {
		return nil
	}
	gitRepos := make([]hooks.GitRepo, 0, len(checkouts))
	for _, checkout := range checkouts {
		gitRepos = append(gitRepos, hooks.GitRepo{Name: checkout.Spec.Name, Dir: checkout.Dir})
	}
	if len(gitRepos) == 0 {
		return nil
	}
	return hooks.InstallGitHooks(box.Root, set, hooks.GitContext{
		RootDir:      hookContext.RootDir,
		PackageDir:   hookContext.PackageDir,
		WorkspaceDir: hookContext.WorkspaceDir,
		StateDir:     hookContext.StateDir,
		ArtifactsDir: hookContext.ArtifactsDir,
		DispatchID:   hookContext.DispatchID,
		StepID:       hookContext.StepID,
		Attempt:      hookContext.Attempt,
		Provider:     hookContext.Provider,
		Repos:        gitRepos,
		DispatchEnv:  hookContext.DispatchEnv,
	})
}

func (r *Runner) claudeToolSettings(box *capsule.Capsule, set *hooks.Set, hookContext hooks.Context,
	checkouts []repos.Checkout) (string, error) {
	if !set.Has(hooks.BeforeTool) && !set.Has(hooks.AfterTool) {
		return "", nil
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(box.Root, "tool-hooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	gitRepos := make([]hooks.GitRepo, 0, len(checkouts))
	for _, checkout := range checkouts {
		gitRepos = append(gitRepos, hooks.GitRepo{Name: checkout.Spec.Name, Dir: checkout.Dir})
	}
	contextPath := filepath.Join(dir, "hook-context.json")
	if err := hooks.WriteGitContext(contextPath, hooks.GitContext{
		RootDir: hookContext.RootDir, PackageDir: hookContext.PackageDir,
		WorkspaceDir: hookContext.WorkspaceDir, StateDir: hookContext.StateDir,
		ArtifactsDir: hookContext.ArtifactsDir, DispatchID: hookContext.DispatchID,
		StepID: hookContext.StepID, Attempt: hookContext.Attempt, Provider: hookContext.Provider,
		Repos: gitRepos, DispatchEnv: hookContext.DispatchEnv,
	}); err != nil {
		return "", err
	}
	return hooks.WriteClaudeToolSettings(filepath.Join(dir, "claude-settings.json"), contextPath,
		executable, set)
}

func (r *Runner) runCleanup(outcome runOutcome, recorder *debuglog.Recorder) runOutcome {
	if outcome.hooks == nil || !outcome.hooks.Has(hooks.Cleanup) {
		return outcome
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	results, err := outcome.hooks.Run(cleanupCtx, hooks.Cleanup, outcome.hookContext)
	for _, result := range results {
		if recorder != nil {
			recorder.Printf("hook.cleanup %s success=%v skipped=%v error=%s",
				result.Name, result.Success, result.Skipped, result.Error)
		}
	}
	if err != nil && outcome.result.Success {
		outcome.result = failedResult(outcome.result, fmt.Errorf("cleanup hook: %w", err))
	}
	return outcome
}

func hookExitContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent.Err() != nil {
		return context.WithTimeout(context.Background(), time.Minute)
	}
	return context.WithTimeout(parent, 5*time.Minute)
}

func firstText(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "tool"
}
