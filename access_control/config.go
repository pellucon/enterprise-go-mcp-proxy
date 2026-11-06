// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package access_control

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
)

// AccessibilityRule grants a role access to matching tools/prompts/resources/
// resource templates. This is also the exact JSON shape exchanged by the
// gateway's _admin.update_role_permissions/_admin.export_role_permissions
// tools, so a role file can be exported, edited, and fed back in unchanged.
type AccessibilityRule struct {
	Role                     string   `json:"role"`
	AllowedTools             []string `json:"allowed_tools"`
	AllowedPrompts           []string `json:"allowed_prompts,omitempty"`
	AllowedResources         []string `json:"allowed_resources,omitempty"`
	AllowedResourceTemplates []string `json:"allowed_resource_templates,omitempty"`
}

// AccessConfig is the root of the access-rules JSON document.
type AccessConfig struct {
	ClientAccessRules []AccessibilityRule `json:"clientAccessRules"`
}

// parseConfigFile reads configPath and parses it. See ParseConfig for the
// parsing logic on already-read bytes.
func parseConfigFile(configPath string) (*AccessConfig, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open config file: %w", err)
	}
	return ParseConfig(data)
}

// ParseConfig parses already-read access-rules JSON bytes. Split out of
// parseConfigFile so a caller extracting this section from a larger
// combined config file (e.g. main.go's combined-config loader) can parse it
// directly, without writing it back out to a temporary file first.
func ParseConfig(data []byte) (*AccessConfig, error) {
	var cfg AccessConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse policy JSON: %w", err)
	}
	return &cfg, nil
}

// LoadConfigFromFile reads and parses an access-rules JSON file, logging a
// summary of the rules it found.
func LoadConfigFromFile(configPath string) (*AccessConfig, error) {
	cfg, err := parseConfigFile(configPath)
	if err != nil {
		logging.Errorf("Initialization Error: %v", err)
		return nil, err
	}

	logging.Infof("Policy Engine initialized successfully with %d active rule definitions.", len(cfg.ClientAccessRules))
	for _, rule := range cfg.ClientAccessRules {
		logging.Infof(" - Role '%s' is authorized to invoke tools: %v, prompts: %v, resources: %v, resource templates: %v", rule.Role, rule.AllowedTools, rule.AllowedPrompts, rule.AllowedResources, rule.AllowedResourceTemplates)
	}
	return cfg, nil
}
