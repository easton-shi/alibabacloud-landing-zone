// Package hooks loads and executes AutoWonder Runtime Hooks.
//
// A task package may contain hooks.json plus capabilities/hooks/<name>/hook.yaml
// and the scripts referenced by that descriptor. This package implements the
// public autowonder.hook.v1 contract for task-lifecycle triggers. Tool hooks
// (beforeTool/afterTool) are executed through a provider-native interception
// point; the runtime only advertises TASK_PACKAGE_TOOL_HOOKS_V1 for providers
// that can actually block a tool call.
package hooks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	SchemaVersion = "autowonder.hook.v1"

	BeforeRepoPrepare = "beforeRepoPrepare"
	BeforeStep        = "beforeStep"
	BeforeAgentStart  = "beforeAgentStart"
	BeforeTool        = "beforeTool"
	AfterTool         = "afterTool"
	AfterAgentExit    = "afterAgentExit"
	BeforeCommit      = "beforeCommit"
	BeforePush        = "beforePush"
	Cleanup           = "cleanup"

	FailureFailTask = "fail-task"
	FailureContinue = "continue"

	OncePerAttempt = "once-per-attempt"
	OncePerStep    = "once-per-step"
	OnceNone       = "none"
)

var (
	namePattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	interpreterRegex = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)
	knownTriggers    = map[string]bool{
		BeforeRepoPrepare: true, BeforeStep: true, BeforeAgentStart: true,
		BeforeTool: true, AfterTool: true,
		AfterAgentExit: true, BeforeCommit: true, BeforePush: true, Cleanup: true,
	}
	knownFailurePolicies = map[string]bool{FailureFailTask: true, FailureContinue: true}
	knownOnceScopes      = map[string]bool{OncePerAttempt: true, OncePerStep: true, OnceNone: true}
)

// Definition is one parsed hook.yaml.
type Definition struct {
	SchemaVersion  string            `yaml:"schemaVersion" json:"schemaVersion"`
	Name           string            `yaml:"name" json:"name"`
	Version        string            `yaml:"version" json:"version"`
	Trigger        string            `yaml:"trigger" json:"trigger"`
	Interpreter    string            `yaml:"interpreter" json:"interpreter"`
	Command        string            `yaml:"command" json:"command"`
	Args           []string          `yaml:"args" json:"args"`
	Cwd            string            `yaml:"cwd" json:"cwd"`
	Env            map[string]string `yaml:"env" json:"env"`
	TimeoutSeconds int               `yaml:"timeoutSeconds" json:"timeoutSeconds"`
	FailurePolicy  string            `yaml:"failurePolicy" json:"failurePolicy"`
	OnceScope      string            `yaml:"onceScope" json:"onceScope"`

	Dir string `yaml:"-" json:"-"`
}

type hooksDocument struct {
	SchemaVersion string       `json:"schemaVersion"`
	Hooks         []hookRecord `json:"hooks"`
}

type hookRecord struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	Enabled *bool  `json:"enabled"`
}

// Set is the validated hook collection of one task package.
type Set struct {
	PackageDir string
	Hooks      []Definition
}

// Load reads hooks.json and each enabled descriptor. A missing hooks.json means
// the package has no hooks. Any malformed descriptor fails the dispatch instead
// of silently running a partial set.
func Load(packageDir string) (*Set, error) {
	set := &Set{PackageDir: packageDir}
	raw, err := os.ReadFile(filepath.Join(packageDir, "hooks.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return set, nil
		}
		return nil, fmt.Errorf("read hooks.json: %w", err)
	}
	var document hooksDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("decode hooks.json: %w", err)
	}
	if document.SchemaVersion != "" && document.SchemaVersion != "autowonder.hooks.v1" {
		return nil, fmt.Errorf("unsupported hooks.json schema %q", document.SchemaVersion)
	}
	seen := map[string]bool{}
	for _, record := range document.Hooks {
		if record.Enabled != nil && !*record.Enabled {
			continue
		}
		if !namePattern.MatchString(record.Name) {
			return nil, fmt.Errorf("invalid hook name %q", record.Name)
		}
		if seen[record.Name] {
			return nil, fmt.Errorf("duplicate hook name %q", record.Name)
		}
		seen[record.Name] = true
		dir := filepath.Join(packageDir, "capabilities", "hooks", record.Name)
		definition, err := loadDefinition(dir)
		if err != nil {
			return nil, fmt.Errorf("hook %s: %w", record.Name, err)
		}
		if definition.Name != record.Name {
			return nil, fmt.Errorf("hook %s descriptor name is %q", record.Name, definition.Name)
		}
		if record.SHA256 != "" {
			digest, err := treeDigest(dir)
			if err != nil {
				return nil, fmt.Errorf("hook %s digest: %w", record.Name, err)
			}
			if digest != record.SHA256 {
				return nil, fmt.Errorf("hook %s digest mismatch: hooks.json=%s package=%s",
					record.Name, record.SHA256, digest)
			}
		}
		set.Hooks = append(set.Hooks, *definition)
	}
	return set, nil
}

func loadDefinition(dir string) (*Definition, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "hook.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read hook.yaml: %w", err)
	}
	var definition Definition
	if err := yaml.Unmarshal(raw, &definition); err != nil {
		return nil, fmt.Errorf("decode hook.yaml: %w", err)
	}
	definition.Dir = dir
	defaults(&definition)
	if err := validate(&definition); err != nil {
		return nil, err
	}
	return &definition, nil
}

func defaults(definition *Definition) {
	if definition.Cwd == "" {
		definition.Cwd = "workspace"
	}
	if definition.FailurePolicy == "" {
		definition.FailurePolicy = FailureFailTask
	}
	if definition.OnceScope == "" {
		definition.OnceScope = OnceNone
	}
	if definition.TimeoutSeconds <= 0 {
		definition.TimeoutSeconds = 60
	}
	if definition.TimeoutSeconds > 3600 {
		definition.TimeoutSeconds = 3600
	}
}

func validate(definition *Definition) error {
	if definition.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schemaVersion %q", definition.SchemaVersion)
	}
	if !namePattern.MatchString(definition.Name) {
		return fmt.Errorf("invalid name %q", definition.Name)
	}
	if !knownTriggers[definition.Trigger] {
		return fmt.Errorf("unsupported trigger %q", definition.Trigger)
	}
	if definition.Interpreter != "" && !interpreterRegex.MatchString(definition.Interpreter) {
		return fmt.Errorf("invalid interpreter %q", definition.Interpreter)
	}
	if strings.TrimSpace(definition.Command) == "" {
		return fmt.Errorf("command is required")
	}
	if definition.Cwd != "workspace" && definition.Cwd != "triggeringRepo" {
		return fmt.Errorf("unsupported cwd %q", definition.Cwd)
	}
	if !knownFailurePolicies[definition.FailurePolicy] {
		return fmt.Errorf("unsupported failurePolicy %q", definition.FailurePolicy)
	}
	if !knownOnceScopes[definition.OnceScope] {
		return fmt.Errorf("unsupported onceScope %q", definition.OnceScope)
	}
	if _, err := resolveCommand(definition.Dir, definition.Command); err != nil {
		return err
	}
	return nil
}

// Has reports whether at least one enabled hook uses the trigger.
func (s *Set) Has(trigger string) bool {
	if s == nil {
		return false
	}
	for _, definition := range s.Hooks {
		if definition.Trigger == trigger {
			return true
		}
	}
	return false
}

// Select returns hooks for one trigger in descriptor order.
func (s *Set) Select(trigger string) []Definition {
	if s == nil {
		return nil
	}
	var result []Definition
	for _, definition := range s.Hooks {
		if definition.Trigger == trigger {
			result = append(result, definition)
		}
	}
	return result
}

// resolveCommand keeps the packaged script path inside its hook directory.
func resolveCommand(dir, command string) (string, error) {
	if filepath.IsAbs(command) {
		return "", fmt.Errorf("command must be relative to the hook package")
	}
	clean := filepath.Clean(command)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("command escapes hook package")
	}
	full := filepath.Join(dir, clean)
	relative, err := filepath.Rel(dir, full)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("command escapes hook package")
	}
	info, err := os.Stat(full)
	if err != nil {
		return "", fmt.Errorf("command %q is unavailable: %w", command, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("command %q is not a regular file", command)
	}
	return full, nil
}

// treeDigest mirrors TaskPackager#extractHook: sorted "name\0sha256:<hex>\n".
func treeDigest(dir string) (string, error) {
	digests := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		digests[filepath.ToSlash(relative)] = "sha256:" + hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(digests) == 0 {
		return "", fmt.Errorf("hook package is empty")
	}
	names := make([]string, 0, len(digests))
	for name := range digests {
		names = append(names, name)
	}
	sort.Strings(names)
	digest := sha256.New()
	for _, name := range names {
		digest.Write([]byte(name))
		digest.Write([]byte{0})
		digest.Write([]byte(digests[name]))
		digest.Write([]byte{'\n'})
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}
