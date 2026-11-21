package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/update"
)

// UpgradeFrame is the server's EXECUTOR_UPGRADE payload.
type UpgradeFrame struct {
	Type             string `json:"type"`
	ExecutorID       int64  `json:"executorId"`
	RequestID        string `json:"requestId"`
	TargetVersion    string `json:"targetVersion"`
	ResultReceiptAck bool   `json:"resultReceiptAck"`
	IssuedAt         string `json:"issuedAt"`
}

// UpgradeResult reports one phase. Only terminal success/failure carries a
// receiptId; the server deliberately ignores non-terminal receipt frames.
type UpgradeResult struct {
	Type           string `json:"type"`
	ExecutorID     int64  `json:"executorId"`
	RequestID      string `json:"requestId"`
	Phase          string `json:"phase"`
	ReceiptID      string `json:"receiptId,omitempty"`
	Terminal       bool   `json:"terminal,omitempty"`
	CurrentVersion string `json:"currentVersion,omitempty"`
	Error          string `json:"error,omitempty"`
}

const defaultUpgradeDrainTimeout = 10 * time.Minute

func (h *Handler) handleUpgrade(ctx context.Context, frame UpgradeFrame) {
	if h.accept("upgrade:"+frame.RequestID) == false {
		return
	}
	go h.upgrade(ctx, frame.RequestID, frame.TargetVersion, nil)
}

// upgrade downloads, verifies and installs a release. Empty target means the
// release source's /latest manifest. launch is only used by the update-restart
// path, which must apply new launch parameters to the upgraded process.
func (h *Handler) upgrade(ctx context.Context, requestID, target string, launch *LaunchConfig) {
	if h.Updater == nil {
		h.reportUpgrade(requestID, "failed", "", false, h.Version,
			"self-update source is not configured (AUTOWONDER_UPDATE_BASE_URL)")
		return
	}
	if target == "" {
		version, err := h.Updater.LatestVersion(ctx)
		if err != nil {
			h.reportUpgrade(requestID, "failed", "", false, h.Version, err.Error())
			return
		}
		target = version
	}
	if h.Version != "" && h.Version != "dev" && update.VersionAtLeast(h.Version, target) {
		h.reportUpgrade(requestID, "success", h.receiptID(), true, h.Version, "")
		return
	}
	h.reportUpgrade(requestID, "accepted", "", false, "", "")
	if h.Inventory != nil {
		h.Inventory.SetDraining(true)
	}
	restarted := false
	defer func() {
		if !restarted && h.Inventory != nil {
			h.Inventory.SetDraining(false)
		}
	}()
	h.reportUpgrade(requestID, "draining", "", false, "", "")
	timeout := h.DrainTimeout
	if timeout <= 0 {
		timeout = defaultUpgradeDrainTimeout
	}
	if h.Inventory != nil {
		drainCtx, cancel := context.WithTimeout(ctx, timeout)
		err := h.Inventory.WaitIdle(drainCtx)
		cancel()
		if err != nil {
			h.reportUpgrade(requestID, "failed", "", false, h.Version,
				"executor did not become idle: "+err.Error())
			return
		}
	}

	h.reportUpgrade(requestID, "downloading", "", false, "", "")
	artifact, err := h.Updater.Download(ctx, target)
	if err != nil {
		h.reportUpgrade(requestID, "failed", "", false, h.Version, err.Error())
		return
	}
	h.reportUpgrade(requestID, "applying", "", false, "", "")
	receiptID := h.receiptID()
	receipt := update.Receipt{
		RequestID: requestID, ReceiptID: receiptID, TargetVersion: target, Phase: "restarting",
	}
	if err := h.Updater.WriteReceipt(receipt); err != nil {
		h.reportUpgrade(requestID, "failed", "", false, h.Version, "write upgrade receipt: "+err.Error())
		return
	}
	if err := h.Updater.Replace(artifact); err != nil {
		_ = h.Updater.ClearReceipt()
		h.reportUpgrade(requestID, "failed", "", false, h.Version, err.Error())
		return
	}

	argv := append([]string(nil), h.Args...)
	if launch != nil {
		cloned := *launch
		cloned.RuntimeVersion = ""
		built, buildErr := h.buildArgs(&cloned)
		if buildErr != nil {
			_ = h.Updater.Rollback()
			_ = h.Updater.ClearReceipt()
			h.reportUpgrade(requestID, "failed", "", false, h.Version, buildErr.Error())
			return
		}
		argv = built
	}
	restarted = true
	h.mu.Lock()
	h.restarted = true
	h.mu.Unlock()
	executable := h.Executable
	if executable == "" {
		executable, _ = os.Executable()
	}
	exec := h.Exec
	if exec == nil {
		exec = execSelf
	}
	env := withEnv(withEnv(os.Environ(), "AUTOWONDER_UPGRADE_REQUEST_ID", requestID),
		"AUTOWONDER_UPGRADE_TARGET_VERSION", target)
	if err := exec(executable, argv, env); err != nil {
		restarted = false
		h.mu.Lock()
		h.restarted = false
		h.mu.Unlock()
		_ = h.Updater.Rollback()
		_ = h.Updater.ClearReceipt()
		h.reportUpgrade(requestID, "failed", "", false, h.Version, "restart after update failed: "+err.Error())
	}
}

func (h *Handler) reportUpgrade(requestID, phase, receiptID string, terminal bool, currentVersion, errText string) {
	if h.Send == nil {
		return
	}
	_ = h.Send(UpgradeResult{
		Type: "EXECUTOR_UPGRADE_RESULT", ExecutorID: h.ExecutorID, RequestID: requestID,
		Phase: phase, ReceiptID: receiptID, Terminal: terminal,
		CurrentVersion: currentVersion, Error: errText,
	})
}

// PublishUpgradeReceipt reports the outcome of a process replacement. It is
// called after every WebSocket attach; the receipt is kept until the server
// acknowledges it or a later heartbeat confirms the version.
func (h *Handler) PublishUpgradeReceipt(send func(any) error) {
	if h.Updater == nil || send == nil {
		return
	}
	receipt, err := h.Updater.ReadReceipt()
	if err != nil || receipt == nil {
		if err != nil {
			slog.Warn("read upgrade receipt failed", "error", err)
		}
		return
	}
	if receipt.Phase != "restarting" || time.Since(receipt.CreatedAt) > 24*time.Hour {
		_ = h.Updater.ClearReceipt()
		return
	}
	phase := "failed"
	terminal := true
	message := ""
	if update.VersionAtLeast(h.Version, receipt.TargetVersion) {
		phase = "success"
	} else {
		message = fmt.Sprintf("runtime restarted with version %s, expected %s", h.Version, receipt.TargetVersion)
	}
	result := UpgradeResult{
		Type: "EXECUTOR_UPGRADE_RESULT", ExecutorID: h.ExecutorID, RequestID: receipt.RequestID,
		Phase: phase, ReceiptID: receipt.ReceiptID, Terminal: terminal,
		CurrentVersion: h.Version, Error: message,
	}
	if err := send(result); err != nil {
		slog.Warn("publish upgrade receipt failed", "error", err)
	}
}

func (h *Handler) acknowledgeUpgrade(raw []byte) {
	if h.Updater == nil {
		return
	}
	var frame struct {
		Type      string `json:"type"`
		RequestID string `json:"requestId"`
		ReceiptID string `json:"receiptId"`
	}
	if json.Unmarshal(raw, &frame) != nil {
		return
	}
	receipt, err := h.Updater.ReadReceipt()
	if err != nil || receipt == nil {
		return
	}
	if receipt.RequestID == frame.RequestID &&
		(receipt.ReceiptID == frame.ReceiptID || frame.ReceiptID == "") {
		if err := h.Updater.ClearReceipt(); err != nil {
			slog.Warn("clear upgrade receipt failed", "error", err)
		}
	}
}

func (h *Handler) receiptID() string {
	var buffer [16]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return fmt.Sprintf("receipt-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer[:])
}
