// Package mcpconfig builds the provider-neutral MCP configuration for a dispatch.
package mcpconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type capabilityDocument struct {
	MCPServers []server `json:"mcpServers"`
}

type server struct {
	Name             string            `json:"name"`
	Transport        string            `json:"transport"`
	URL              string            `json:"url"`
	Command          string            `json:"command"`
	Args             []string          `json:"args"`
	Headers          map[string]string `json:"headers"`
	HeaderSecretRefs map[string]string `json:"headerSecretRefs"`
	Env              map[string]string `json:"env"`
	EnvSecretRefs    map[string]string `json:"envSecretRefs"`
	AuthType         string            `json:"authType"`
}

// Write renders mcp.config.json and returns its path. It returns an empty path
// when the dispatch has no MCP servers.
func Write(packageDir, baseURL, dispatchToken string, secrets map[string]string, path string) (string, error) {
	servers := map[string]any{}
	if dispatchToken != "" && baseURL != "" {
		servers["autowonder"] = map[string]any{
			"type": "http",
			"url":  strings.TrimRight(baseURL, "/") + "/api/mcp",
			"headers": map[string]string{
				"Authorization": "Bearer " + dispatchToken,
			},
		}
	}
	document, err := readCapabilities(filepath.Join(packageDir, "skills.json"))
	if err != nil {
		return "", err
	}
	if document != nil {
		for _, descriptor := range document.MCPServers {
			name := strings.TrimSpace(descriptor.Name)
			if name == "" {
				continue
			}
			if name == "autowonder" && dispatchToken != "" {
				continue
			}
			entry, err := renderServer(descriptor, secrets)
			if err != nil {
				return "", err
			}
			servers[name] = entry
		}
	}
	if len(servers) == 0 {
		return "", nil
	}
	encoded, err := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode MCP configuration: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create MCP directory: %w", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("write MCP configuration: %w", err)
	}
	return path, nil
}

func readCapabilities(path string) (*capabilityDocument, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read skills.json: %w", err)
	}
	var document capabilityDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("parse skills.json: %w", err)
	}
	return &document, nil
}

func renderServer(descriptor server, secrets map[string]string) (map[string]any, error) {
	headers, err := resolveSecrets(descriptor.Headers, descriptor.HeaderSecretRefs, secrets)
	if err != nil {
		return nil, fmt.Errorf("MCP server %q headers: %w", descriptor.Name, err)
	}
	environment, err := resolveSecrets(descriptor.Env, descriptor.EnvSecretRefs, secrets)
	if err != nil {
		return nil, fmt.Errorf("MCP server %q env: %w", descriptor.Name, err)
	}
	entry := map[string]any{"enabled": true}
	transport := strings.ToLower(strings.TrimSpace(descriptor.Transport))
	switch transport {
	case "http", "sse", "streamable-http":
		entry["type"] = transport
		entry["url"] = descriptor.URL
	case "stdio", "local", "":
		if descriptor.Command == "" {
			return nil, fmt.Errorf("MCP server %q has neither a URL nor a command", descriptor.Name)
		}
		entry["type"] = "stdio"
		entry["command"] = descriptor.Command
		if len(descriptor.Args) > 0 {
			entry["args"] = descriptor.Args
		}
	default:
		return nil, fmt.Errorf("MCP server %q has unsupported transport %q", descriptor.Name, descriptor.Transport)
	}
	if len(headers) > 0 {
		entry["headers"] = headers
	}
	if len(environment) > 0 {
		entry["env"] = environment
	}
	return entry, nil
}

func resolveSecrets(values, references, secrets map[string]string) (map[string]string, error) {
	if len(values) == 0 && len(references) == 0 {
		return nil, nil
	}
	resolved := make(map[string]string, len(values)+len(references))
	for key, value := range values {
		resolved[key] = value
	}
	for key, reference := range references {
		value, ok := secrets[reference]
		if !ok || value == "" {
			return nil, fmt.Errorf("secret reference %q is unavailable", reference)
		}
		resolved[key] = value
	}
	return resolved, nil
}
