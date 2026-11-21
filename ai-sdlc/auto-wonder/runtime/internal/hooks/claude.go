package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WriteClaudeToolSettings generates the extra settings file passed to Claude
// Code via --settings. Claude's native PreToolUse hook can block a call with
// exit code 2; PostToolUse observes the result. Both wrappers translate the
// native input into autowonder.toolHookContext.v1 before invoking the package.
func WriteClaudeToolSettings(path, contextPath, executable string, set *Set) (string, error) {
	if set == nil || (!set.Has(BeforeTool) && !set.Has(AfterTool)) {
		return "", nil
	}
	if strings.TrimSpace(executable) == "" {
		return "", fmt.Errorf("runtime executable is required for Claude tool hooks")
	}
	command := func(trigger string) string {
		return fmt.Sprintf("%s hook tool --context %s --trigger %s --provider claude",
			shellQuote(executable), shellQuote(contextPath), trigger)
	}
	hooksConfig := map[string]any{}
	if set.Has(BeforeTool) {
		hooksConfig["PreToolUse"] = []any{map[string]any{
			"matcher": "*",
			"hooks":   []any{map[string]any{"type": "command", "command": command(BeforeTool)}},
		}}
	}
	if set.Has(AfterTool) {
		hooksConfig["PostToolUse"] = []any{map[string]any{
			"matcher": "*",
			"hooks":   []any{map[string]any{"type": "command", "command": command(AfterTool)}},
		}}
	}
	raw, err := json.MarshalIndent(map[string]any{"hooks": hooksConfig}, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return "", err
	}
	return path, nil
}
