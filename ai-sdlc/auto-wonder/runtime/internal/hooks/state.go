package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const stateFileName = "hooks-state.json"

type stateDocument struct {
	Hooks map[string]*stateEntry `json:"hooks"`
}

type stateEntry struct {
	Attempt bool            `json:"attempt,omitempty"`
	Steps   map[string]bool `json:"steps,omitempty"`
}

func statePath(stateDir string) string {
	return filepath.Join(stateDir, stateFileName)
}

func stateSaysSkip(stateDir string, definition Definition, stepID string) (bool, error) {
	var skip bool
	err := withState(stateDir, func(document *stateDocument) error {
		entry := document.Hooks[definition.Name]
		if entry == nil {
			return nil
		}
		switch definition.OnceScope {
		case OncePerAttempt:
			skip = entry.Attempt
		case OncePerStep:
			skip = entry.Steps[stepKey(stepID)]
		}
		return nil
	})
	return skip, err
}

func markState(stateDir string, definition Definition, stepID string) error {
	return withState(stateDir, func(document *stateDocument) error {
		entry := document.Hooks[definition.Name]
		if entry == nil {
			entry = &stateEntry{}
			document.Hooks[definition.Name] = entry
		}
		switch definition.OnceScope {
		case OncePerAttempt:
			entry.Attempt = true
		case OncePerStep:
			if entry.Steps == nil {
				entry.Steps = map[string]bool{}
			}
			entry.Steps[stepKey(stepID)] = true
		}
		return nil
	})
}

func stepKey(stepID string) string {
	if stepID == "" {
		return "0"
	}
	return stepID
}

// withState serializes read-modify-write on the state file with an exclusive
// lock file so a Git-hook child process and the main runtime cannot lose updates.
func withState(stateDir string, mutate func(*stateDocument) error) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	unlock, err := lock(filepath.Join(stateDir, stateFileName+".lock"))
	if err != nil {
		return err
	}
	defer unlock()

	path := statePath(stateDir)
	document := &stateDocument{Hooks: map[string]*stateEntry{}}
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, document); err != nil {
			return fmt.Errorf("decode %s: %w", stateFileName, err)
		}
		if document.Hooks == nil {
			document.Hooks = map[string]*stateEntry{}
		}
	}
	if err := mutate(document); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func lock(path string) (func(), error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			file.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > 30*time.Second {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for hook state lock %s", path)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
