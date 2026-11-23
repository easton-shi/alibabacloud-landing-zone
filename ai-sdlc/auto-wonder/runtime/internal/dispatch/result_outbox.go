package dispatch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
)

type resultOutbox struct {
	dir string
	mu  sync.Mutex
}

func newResultOutbox(root string) *resultOutbox {
	if root == "" {
		return &resultOutbox{}
	}
	return &resultOutbox{dir: filepath.Join(root, ".runtime-outbox", "results")}
}

func (o *resultOutbox) Put(value any) error {
	if o == nil || o.dir == "" {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode dispatch outbox frame: %w", err)
	}
	var envelope struct {
		DispatchID int64 `json:"dispatchId"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode dispatch outbox identity: %w", err)
	}
	if envelope.DispatchID <= 0 {
		return fmt.Errorf("dispatch outbox requires a positive dispatchId")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := os.MkdirAll(o.dir, 0o700); err != nil {
		return fmt.Errorf("create dispatch outbox: %w", err)
	}
	temp, err := os.CreateTemp(o.dir, ".dispatch-*.tmp")
	if err != nil {
		return fmt.Errorf("create dispatch outbox file: %w", err)
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
		return fmt.Errorf("secure dispatch outbox file: %w", err)
	}
	if _, err := temp.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("write dispatch outbox file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync dispatch outbox file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close dispatch outbox file: %w", err)
	}
	target := o.path(envelope.DispatchID)
	if err := os.Rename(tempPath, target); err != nil {
		return fmt.Errorf("publish dispatch outbox file: %w", err)
	}
	cleanup = false
	return nil
}

func (o *resultOutbox) List() ([]json.RawMessage, error) {
	if o == nil || o.dir == "" {
		return nil, nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	entries, err := os.ReadDir(o.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read dispatch outbox: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	frames := make([]json.RawMessage, 0, len(names))
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(o.dir, name))
		if err != nil {
			return nil, fmt.Errorf("read dispatch outbox frame %s: %w", name, err)
		}
		frames = append(frames, json.RawMessage(raw))
	}
	return frames, nil
}

func (o *resultOutbox) Delete(dispatchID int64) error {
	if o == nil || o.dir == "" || dispatchID <= 0 {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := os.Remove(o.path(dispatchID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete dispatch outbox frame: %w", err)
	}
	return nil
}

func (o *resultOutbox) Has(dispatchID int64) bool {
	if o == nil || o.dir == "" || dispatchID <= 0 {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	_, err := os.Stat(o.path(dispatchID))
	return err == nil
}

func (o *resultOutbox) Get(dispatchID int64) (json.RawMessage, error) {
	if o == nil || o.dir == "" || dispatchID <= 0 {
		return nil, os.ErrNotExist
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	raw, err := os.ReadFile(o.path(dispatchID))
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

func (o *resultOutbox) path(dispatchID int64) string {
	return filepath.Join(o.dir, "dispatch-"+strconv.FormatInt(dispatchID, 10)+".json")
}
