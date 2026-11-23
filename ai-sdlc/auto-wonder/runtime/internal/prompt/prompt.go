// Package prompt assembles the agent prompt from a task package.
package prompt

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/capsule"
	"github.com/aliyun/alibabacloud-landing-zone/ai-sdlc/auto-wonder/runtime/internal/repos"
)

const maxSectionBytes = 64 * 1024

// BuildWork describes the materialized execution environment for one dispatch.
type BuildWork struct {
	Checkouts []repos.Checkout
	// LazyRepos are readable but not checked out; the agent clones them only
	// when the task actually needs them.
	LazyRepos               []repos.Spec
	LocalAPIEndpoint        string
	DispatchID              int64
	StepID                  string
	StepAttempt             int
	ArtifactProtocolVersion int
}

// Build renders the execution context in the same order as the official
// runtime: identity -> current SDLC step -> workitem -> clarification ->
// capabilities -> repositories -> memory -> roster -> workspace contract.
func Build(c *capsule.Capsule, work BuildWork) (string, error) {
	pkg := c.PackageDir
	var parts []string

	identity := readJSON(filepath.Join(pkg, "identity.json"))
	role := stringValue(identity, "roleName", "the assigned role")
	roleCode := stringValue(identity, "roleCode", "developer")
	parts = append(parts, fmt.Sprintf(
		"You are %s (role: %s), a digital worker on the AutoWonder platform.\n%s",
		role, roleCode, firstString(identity, "agentMd", "responsibilities", "businessBackground")))

	sdlc := readJSON(filepath.Join(pkg, "sdlc.json"))
	stepBlock := renderCurrentStep(sdlc)
	if stepBlock != "" {
		parts = append(parts, stepBlock)
	}

	for _, section := range []struct{ title, name string }{
		{"Workitem", "workitem.md"},
		{"Clarification", "clarification.md"},
		{"Context", "context.md"},
		{"Comments", "comments.md"},
		{"Interaction context", "interaction-context.md"},
	} {
		if body := readText(filepath.Join(pkg, section.name)); body != "" {
			parts = append(parts, "## "+section.title+"\n"+body)
		}
	}

	if capabilities := renderCapabilities(pkg); capabilities != "" {
		parts = append(parts, "## Skills and MCP capabilities\n"+capabilities)
	}
	if repositories := renderRepositories(work.Checkouts); repositories != "" {
		parts = append(parts, "## Repositories\n"+repositories)
	} else if len(work.LazyRepos) == 0 {
		parts = append(parts, "## Repositories\nNo repository was bound to this task. "+
			"If the work item explicitly names a repository, inspect the context before making changes.")
	}
	if lazy := renderAvailableRepositories(work.LazyRepos, c.WorkspaceDir); lazy != "" {
		parts = append(parts, "## Repositories available on demand\n"+lazy)
	}
	if relations := readText(filepath.Join(pkg, "repo-map.json")); relations != "" {
		parts = append(parts, "## Repository relations (JSON)\n"+relations)
	}

	if memory := renderMemory(filepath.Join(pkg, "memory")); memory != "" {
		parts = append(parts, "## Memory\n"+memory)
	}
	if roster := readText(filepath.Join(pkg, "roster.json")); roster != "" {
		parts = append(parts, "## Team roster\n"+roster)
	}

	parts = append(parts, fmt.Sprintf(
		"## Workspace\nTask package: %s (read-only reference).\n"+
			"Workspace root: %s. Repository checkouts are under %s/repos/.\n"+
			"Write deliverables under %s/deliverables/ and evidence under %s/evidence/.\n"+
			"If this step hands off to another role, write %s/handoff-proposal.json with "+
			"{\"nextOwner\":\"...\",\"nextRole\":\"...\",\"summary\":\"...\"}.\n"+
			"Anything you fail to complete must be written to %s/FAILURE so the scheduler marks the dispatch failed.\n"+
			"The runtime writes %s/deliverables/runtime-source-revision.json after your turn; do not edit that file.\n"+
			"Do not place credentials, tokens or environment-variable values in deliverables.",
		pkg, c.WorkspaceDir, c.WorkspaceDir, c.ArtifactsDir, c.ArtifactsDir,
		c.ArtifactsDir, c.ArtifactsDir, c.ArtifactsDir))

	if work.LocalAPIEndpoint != "" {
		attempt := work.StepAttempt
		if attempt < 1 {
			attempt = 1
		}
		parts = append(parts, fmt.Sprintf(
			"## Completion protocol\n"+
				"Before ending ordinary work successfully, POST a completion_requested event to %s/dispatches/%d/step-events "+
				"and wait for an accepted HTTP 202 response.\n"+
				"Example body: {\"stepId\":\"%s\",\"stepAttempt\":%d,\"type\":\"completion_requested\","+
				"\"message\":\"<what you delivered>\",\"evidenceRefs\":[\"evidence/<file>\",\"deliverables/<file>\"]}.\n"+
				"evidenceRefs must be relative to %s and point to regular files under typed output directories. "+
				"Do not use full paths or an artifacts/output/ prefix. The Runtime publishes only referenced files.",
			work.LocalAPIEndpoint, work.DispatchID, work.StepID, attempt, c.ArtifactsDir))
	}

	return strings.Join(nonEmpty(parts), "\n\n"), nil
}

type capabilityDocument struct {
	Skills []struct {
		Name        string `json:"name"`
		Path        string `json:"path"`
		Version     string `json:"version"`
		Required    bool   `json:"required"`
		Description string `json:"description"`
	} `json:"skills"`
	MCPServers []struct {
		Name      string `json:"name"`
		Transport string `json:"transport"`
		URL       string `json:"url"`
	} `json:"mcpServers"`
	Plugins []struct {
		Name    string `json:"name"`
		Path    string `json:"path"`
		Version string `json:"version"`
	} `json:"plugins"`
}

func renderCapabilities(packageDir string) string {
	document := readCapabilities(filepath.Join(packageDir, "skills.json"))
	if document == nil {
		return ""
	}
	var parts []string
	if len(document.Skills) > 0 {
		var skills strings.Builder
		skills.WriteString("Load a skill's SKILL.md before following it. Skills bundled with this dispatch:\n")
		for _, skill := range document.Skills {
			path := skill.Path
			if path == "" {
				path = filepath.ToSlash(filepath.Join("capabilities", "skills", skill.Name))
			}
			fmt.Fprintf(&skills, "- %s (v%s) at %s/%s/SKILL.md", skill.Name, skill.Version, packageDir, path)
			if skill.Required {
				skills.WriteString(" [required]")
			}
			if skill.Description != "" {
				skills.WriteString(": " + truncate(skill.Description, 300))
			}
			skills.WriteString("\n")
		}
		parts = append(parts, skills.String())
	}
	if len(document.Plugins) > 0 {
		var plugins strings.Builder
		plugins.WriteString("Bundled plugins:\n")
		for _, plugin := range document.Plugins {
			fmt.Fprintf(&plugins, "- %s (v%s) at %s\n", plugin.Name, plugin.Version, plugin.Path)
		}
		parts = append(parts, plugins.String())
	}
	if len(document.MCPServers) > 0 {
		var servers strings.Builder
		servers.WriteString("MCP servers configured for this turn:\n")
		for _, server := range document.MCPServers {
			fmt.Fprintf(&servers, "- %s (%s)", server.Name, firstNonEmpty(server.Transport, "stdio"))
			if server.URL != "" {
				servers.WriteString(" " + server.URL)
			}
			servers.WriteString("\n")
		}
		parts = append(parts, servers.String())
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func readCapabilities(path string) *capabilityDocument {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var document capabilityDocument
	if json.Unmarshal(raw, &document) != nil {
		return nil
	}
	return &document
}

func renderRepositories(checkouts []repos.Checkout) string {
	if len(checkouts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("These repositories are already checked out. Work inside their directories rather than cloning duplicates.\n")
	for _, checkout := range checkouts {
		fmt.Fprintf(&b, "- %s: %s", checkout.Spec.Name, checkout.Dir)
		if branch := branchOf(checkout); branch != "" {
			fmt.Fprintf(&b, " (branch %s)", branch)
		}
		fmt.Fprintf(&b, " [commit %s, commit=%t, push=%t, network=%t]",
			shortCommit(checkout.BaseCommit), checkout.Spec.AllowCommit, checkout.Spec.AllowPush, checkout.Spec.AllowNetwork)
		if len(checkout.Spec.AllowedBranchPatterns) > 0 {
			fmt.Fprintf(&b, " allowed branches: %s", strings.Join(checkout.Spec.AllowedBranchPatterns, ", "))
		}
		b.WriteString("\n")
	}
	b.WriteString("Do not rewrite Git history. Do not commit or push unless the repository policy above permits it. ")
	b.WriteString("Leave all source changes in the checkout; the scheduler consumes the final HEAD and delivery branch from runtime-source-revision.json.")
	return b.String()
}

// renderAvailableRepositories lists lazy read-only repositories. They are not
// cloned during preparation because the Chief conversation sees every tenant
// repository; cloning all of them would dwarf the actual task.
func renderAvailableRepositories(specs []repos.Spec, workspaceDir string) string {
	if len(specs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("These read-only repositories are not checked out. Clone one under " +
		filepath.ToSlash(filepath.Join(workspaceDir, "repos")) +
		"/<path> only when the task needs it, using the host's Git credentials.\n")
	for _, spec := range specs {
		path := spec.Path
		if path == "" {
			path = spec.Name
		}
		fmt.Fprintf(&b, "- %s: %s (path %s", spec.Name, spec.URL, path)
		if spec.Ref != "" {
			fmt.Fprintf(&b, ", ref %s", spec.Ref)
		}
		b.WriteString(")\n")
	}
	return b.String()
}

func branchOf(checkout repos.Checkout) string {
	if checkout.Spec.DeliveryBranch != "" {
		return checkout.Spec.DeliveryBranch
	}
	return checkout.Spec.Ref
}

func shortCommit(value string) string {
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func renderCurrentStep(sdlc map[string]any) string {
	if sdlc == nil {
		return ""
	}
	steps, _ := sdlc["steps"].([]any)
	var current map[string]any
	if stepID := valueID(sdlc["currentStepId"]); stepID != "" {
		for _, raw := range steps {
			step, _ := raw.(map[string]any)
			if valueID(step["id"]) == stepID {
				current = step
				break
			}
		}
	}
	if current == nil && len(steps) > 0 {
		current, _ = steps[0].(map[string]any)
	}
	if current == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Your task for THIS step\n")
	fmt.Fprintf(&b, "Step: %s (%s)\n", stringValue(current, "name", "implement"), stringValue(current, "kind", ""))
	b.WriteString(firstString(current, "instructionMd", "instruction"))
	if checklist := renderChecklist(current); checklist != "" {
		fmt.Fprintf(&b, "\nChecklist: %s", checklist)
	}
	return b.String()
}

func renderChecklist(step map[string]any) string {
	if raw := firstString(step, "checklistJson"); raw != "" {
		return raw
	}
	switch value := step["checklist"].(type) {
	case []any, map[string]any:
		if encoded, err := json.Marshal(value); err == nil {
			return string(encoded)
		}
	case string:
		return value
	}
	return ""
}

func valueID(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return fmt.Sprintf("%.0f", typed)
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

func renderMemory(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	var parts []string
	for _, name := range names {
		if body := readText(filepath.Join(dir, name)); body != "" {
			parts = append(parts, "### "+strings.TrimSuffix(name, ".md")+"\n"+body)
		}
	}
	return strings.Join(parts, "\n\n")
}

func readText(path string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(content) > maxSectionBytes {
		content = content[:maxSectionBytes]
	}
	return strings.TrimSpace(string(content))
}

func readJSON(path string) map[string]any {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var value map[string]any
	if json.Unmarshal(content, &value) != nil {
		return nil
	}
	return value
}

func stringValue(value map[string]any, key, fallback string) string {
	if value == nil {
		return fallback
	}
	if text, ok := value[key].(string); ok && text != "" {
		return text
	}
	return fallback
}

func firstString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text := stringValue(value, key, ""); text != "" {
			return text
		}
	}
	return ""
}

func nonEmpty(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			result = append(result, value)
		}
	}
	return result
}

// BuildConversation renders one interactive conversation turn. Unlike a
// dispatch there is no SDLC step or work item: the frame's system prompt is the
// identity, the capability package supplies skills/MCP, and the user message is
// the turn payload.
func BuildConversation(systemPrompt, content, packageDir, workspaceDir string, checkouts []repos.Checkout,
	lazyRepos []repos.Spec) string {
	var parts []string
	if strings.TrimSpace(systemPrompt) != "" {
		parts = append(parts, strings.TrimSpace(systemPrompt))
	}
	if capabilities := renderCapabilities(packageDir); capabilities != "" {
		parts = append(parts, "## Skills and MCP capabilities\n"+capabilities)
	}
	if repositories := renderRepositories(checkouts); repositories != "" {
		parts = append(parts, "## Repositories\n"+repositories)
	} else if len(lazyRepos) == 0 {
		parts = append(parts, "## Repositories\nNo repository was bound to this conversation.")
	}
	if lazy := renderAvailableRepositories(lazyRepos, workspaceDir); lazy != "" {
		parts = append(parts, "## Repositories available on demand\n"+lazy)
	}
	if relations := readText(filepath.Join(packageDir, "repo-map.json")); relations != "" {
		parts = append(parts, "## Repository relations (JSON)\n"+relations)
	}
	parts = append(parts, fmt.Sprintf(
		"## Conversation workspace\nCapability package: %s (read-only reference).\n"+
			"Workspace root: %s. Repository checkouts are under %s/repos/.\n"+
			"Answer the user directly in Markdown. Keep working inside the workspace; "+
			"do not clone duplicate repositories. Do not place credentials, tokens or "+
			"environment-variable values in your reply.",
		packageDir, workspaceDir, workspaceDir))
	parts = append(parts, "## User message\n"+content)
	return strings.Join(nonEmpty(parts), "\n\n")
}
