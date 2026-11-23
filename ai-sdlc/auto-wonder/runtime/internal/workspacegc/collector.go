package workspacegc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	terminalStatePath     = "state/runtime-terminal.json"
	conversationStatePath = ".runtime-state.json"
	maxGCItemsPerRun      = 256
)

type terminalState struct {
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type conversationState struct {
	UpdatedAt time.Time `json:"updatedAt"`
}

// Report describes the capsules removed by one GC pass.
type Report struct {
	Removed  []string
	Pressure bool
}

// DiskUsage is the filesystem capacity used for pressure-mode admission.
type DiskUsage struct {
	Total uint64
	Free  uint64
}

// Collector performs conservative TTL cleanup of terminal dispatch capsules and
// inactive conversation workspaces.
type Collector struct {
	Root                 string
	TaskTTL              time.Duration
	ConversationTTL      time.Duration
	Now                  func() time.Time
	IsDispatchActive     func(int64) bool
	IsConversationActive func(int64) bool
	HighWatermark        int
	LowWatermark         int
	MinFreeBytes         int64
	TargetFreeBytes      int64
	DiskUsage            func(string) (DiskUsage, error)
}

func (c Collector) RunOnce(ctx context.Context) (Report, error) {
	report := Report{}
	if c.Root == "" {
		return report, nil
	}
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	entries, err := os.ReadDir(c.Root)
	if os.IsNotExist(err) {
		return report, nil
	}
	if err != nil {
		return report, err
	}
	pressure, simulatedFree, totalBytes := c.pressureMode()
	report.Pressure = pressure
	removed := 0
	for _, entry := range entries {
		if removed >= maxGCItemsPerRun || ctx.Err() != nil {
			break
		}
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "dispatch-") {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(entry.Name(), "dispatch-"), 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		if c.IsDispatchActive != nil && c.IsDispatchActive(id) {
			continue
		}
		if c.pendingResult(id) {
			continue
		}
		state, err := readTerminal(filepath.Join(c.Root, entry.Name()))
		if err != nil || !terminalStatus(state.Status) {
			continue
		}
		expired := now.Sub(state.UpdatedAt) > c.TaskTTL
		pressureEligible := pressure && (state.Status == "SUCCESS" || state.Status == "COMPLETED")
		if !expired && !pressureEligible {
			continue
		}
		if pressure && c.pressureGoalMet(simulatedFree, totalBytes) {
			break
		}
		dir := filepath.Join(c.Root, entry.Name())
		size := directorySize(dir)
		if err := removeCapsule(c.Root, dir, now); err != nil {
			return report, err
		}
		report.Removed = append(report.Removed, entry.Name())
		simulatedFree += size
		removed++
	}

	conversationsDir := filepath.Join(c.Root, "conversations")
	conversations, err := os.ReadDir(conversationsDir)
	if os.IsNotExist(err) {
		return report, nil
	}
	if err != nil {
		return report, err
	}
	for _, entry := range conversations {
		if removed >= maxGCItemsPerRun || ctx.Err() != nil {
			break
		}
		if !entry.IsDir() {
			continue
		}
		id, err := strconv.ParseInt(entry.Name(), 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		if c.IsConversationActive != nil && c.IsConversationActive(id) {
			continue
		}
		state, err := readConversation(filepath.Join(conversationsDir, entry.Name()))
		if err != nil || now.Sub(state.UpdatedAt) <= c.ConversationTTL {
			continue
		}
		dir := filepath.Join(conversationsDir, entry.Name())
		if err := removeCapsule(c.Root, dir, now); err != nil {
			return report, err
		}
		report.Removed = append(report.Removed, filepath.Join("conversations", entry.Name()))
		removed++
	}
	return report, nil
}

func (c Collector) pressureMode() (bool, uint64, uint64) {
	if c.DiskUsage == nil {
		return false, 0, 0
	}
	usage, err := c.DiskUsage(c.Root)
	if err != nil || usage.Total == 0 {
		return false, 0, 0
	}
	usedPercent := int((usage.Total - usage.Free) * 100 / usage.Total)
	pressure := c.HighWatermark > 0 && usedPercent >= c.HighWatermark
	if c.MinFreeBytes > 0 && usage.Free < uint64(c.MinFreeBytes) {
		pressure = true
	}
	return pressure, usage.Free, usage.Total
}

func (c Collector) pressureGoalMet(free, total uint64) bool {
	if free < uint64(maxInt64(c.TargetFreeBytes, c.MinFreeBytes)) {
		return false
	}
	if c.LowWatermark <= 0 || total == 0 {
		return true
	}
	usedPercent := int((total - free) * 100 / total)
	return usedPercent <= c.LowWatermark
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func directorySize(root string) uint64 {
	var size uint64
	_ = filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if info, statErr := entry.Info(); statErr == nil {
			size += uint64(info.Size())
		}
		return nil
	})
	return size
}

func (c Collector) pendingResult(dispatchID int64) bool {
	_, err := os.Stat(filepath.Join(c.Root, ".runtime-outbox", "results",
		fmt.Sprintf("dispatch-%d.json", dispatchID)))
	return err == nil
}

func WriteTerminal(capsuleDir, status string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return writeJSON(filepath.Join(capsuleDir, terminalStatePath), terminalState{
		Status: status, UpdatedAt: at.UTC(),
	})
}

func TouchConversation(conversationDir string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return writeJSON(filepath.Join(conversationDir, conversationStatePath), conversationState{UpdatedAt: at.UTC()})
}

func readTerminal(capsuleDir string) (terminalState, error) {
	var state terminalState
	raw, err := os.ReadFile(filepath.Join(capsuleDir, terminalStatePath))
	if err != nil {
		return state, err
	}
	err = json.Unmarshal(raw, &state)
	return state, err
}

func readConversation(conversationDir string) (conversationState, error) {
	var state conversationState
	raw, err := os.ReadFile(filepath.Join(conversationDir, conversationStatePath))
	if err != nil {
		return state, err
	}
	err = json.Unmarshal(raw, &state)
	return state, err
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".state-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	cleanup := true
	defer func() {
		_ = temp.Close()
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temp.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func terminalStatus(status string) bool {
	switch status {
	case "SUCCESS", "FAILED", "PAUSED", "CANCELED", "COMPLETED":
		return true
	default:
		return false
	}
}

func removeCapsule(root, target string, now time.Time) error {
	trash := filepath.Join(root, ".gc-trash")
	if err := os.MkdirAll(trash, 0o700); err != nil {
		return err
	}
	name := fmt.Sprintf("%s-%d", filepath.Base(target), now.UnixNano())
	if err := os.Rename(target, filepath.Join(trash, name)); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(trash, name))
}
