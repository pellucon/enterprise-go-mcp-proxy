// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package logging

import (
	"encoding/json"
	"fmt"
)

// LevelConfig is the "logLevel" configsync section's wire shape — see
// CLAUDE.md's "Unify configuration into a single config-map mechanism".
type LevelConfig struct {
	Level string `json:"level"`
}

// ParseConfig validates raw as a LevelConfig — the parse func passed to
// every configsync.Watcher[LevelConfig], matching every other section's own
// package-level ParseConfig convention (access_control.ParseConfig,
// ratelimit.ParseConfig, events.ParseDictionary, ...). Empty raw parses as
// a zero LevelConfig (Level == ""), which ApplyConfig treats as a no-op.
func ParseConfig(raw []byte) (*LevelConfig, error) {
	var cfg LevelConfig
	if len(raw) == 0 {
		return &cfg, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("invalid logLevel config: %w", err)
	}
	switch normalizeLevel(cfg.Level) {
	case "", "error", "info", "debug":
	default:
		return nil, fmt.Errorf("invalid logLevel config: unknown level %q", cfg.Level)
	}
	return &cfg, nil
}

// ApplyConfig is the func(*LevelConfig) adapter shared by both
// configsync.NewWatcher's onChange and registerSectionAdminTools' apply for
// the "log-level" section — the same "one function reused for both"
// convention events.Client.SetDictionary already follows for
// event_dictionary. A nil cfg or empty cfg.Level is a no-op: the process
// keeps whatever level it already has.
func ApplyConfig(cfg *LevelConfig) {
	if cfg == nil || cfg.Level == "" {
		return
	}
	Init(cfg.Level)
}
