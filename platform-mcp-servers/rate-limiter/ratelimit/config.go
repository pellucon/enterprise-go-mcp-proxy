// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Package ratelimit implements a per-tool, per-role call-rate limiter.
//
// A tool may have an optional global limit (calls/minute). While the tool's
// aggregate traffic is below that limit, every call is admitted
// unconditionally, regardless of the caller's role. Once the limit is
// reached, admission is decided by two strictly separate, non-crossing
// pools:
//
//   - The "configured" pool: shared, proportionally to their own configured
//     percentage, among roles that have an explicit RolePercent entry for the
//     tool. An idle configured role's unused share is borrowable by other
//     currently-active configured roles.
//   - The "unconfigured" pool: the leftover percentage (100% minus the sum of
//     configured percentages), shared equally among whichever distinct roles
//     without an explicit entry are currently active.
//
// The sum of a tool's configured percentages may be as high as 100 -- in
// that case there is no leftover, and a role absent from RolePercent is
// flatly denied once the tool is contended (a deliberate, accepted
// possibility, not guarded against: an operator who wants every role to keep
// a nonzero floor must leave headroom themselves by keeping the sum under
// 100).
//
// This package is self-contained: it takes a caller's roles as a plain
// []string rather than resolving them from any claims/JWT machinery, and has
// no dependency on any other package in this module.
package ratelimit

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// ToolConfig describes the rate-limiting policy for a single tool.
type ToolConfig struct {
	// GlobalPerMinute is the tool's overall call budget. A nil value means
	// the tool is entirely unlimited, and RolePercent must then be empty.
	GlobalPerMinute *int `json:"globalPerMinute,omitempty"`

	// RolePercent maps a role name to its percentage share of
	// GlobalPerMinute, consulted only once the tool's aggregate traffic
	// reaches GlobalPerMinute. The sum of all values must be at most 100
	// (see Config.Validate); a sum of exactly 100 leaves no floor at all for
	// roles absent from this map.
	RolePercent map[string]float64 `json:"rolePercent,omitempty"`
}

// Config is the full rate-limiting configuration, keyed by namespaced tool
// name (e.g. "garden-online-store.purchase_products"). A tool name absent
// from Tools is treated identically to a ToolConfig with a nil
// GlobalPerMinute: entirely unlimited.
type Config struct {
	// TimeoutSeconds is how long a caller (go-proxy) waits for a permission
	// decision before giving up. Consulted only by go-proxy itself -- the
	// rate-limiter server's own Limiter never reads this field. Nil means no
	// deadline is applied by go-proxy at all (the ordinary tool-call timeout,
	// if any, applies instead).
	TimeoutSeconds *int `json:"timeoutSeconds,omitempty"`

	Tools map[string]ToolConfig `json:"tools,omitempty"`
}

// ParseConfig unmarshals and validates a Config from its JSON wire form, the
// shape every configsync.Watcher/Writer[Config] and the combined proxy
// config file's own "rateLimiting" section use.
func ParseConfig(data []byte) (*Config, error) {
	var cfg Config
	if len(data) > 0 {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parsing rate limiting config: %w", err)
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate reports every configuration error found across every tool,
// joined together (via errors.Join) so a config author sees every problem in
// one pass rather than one at a time. A nil Config is valid (equivalent to
// an empty one).
func (c *Config) Validate() error {
	if c == nil {
		return nil
	}
	names := make([]string, 0, len(c.Tools))
	for name := range c.Tools {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic error ordering

	var errs []error
	for _, name := range names {
		if err := c.Tools[name].validate(); err != nil {
			errs = append(errs, fmt.Errorf("tool %q: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func (t ToolConfig) validate() error {
	if t.GlobalPerMinute == nil {
		if len(t.RolePercent) > 0 {
			return errors.New("rolePercent is set but globalPerMinute is nil (percentages of an unlimited/undefined budget are meaningless)")
		}
		return nil
	}

	var errs []error
	if *t.GlobalPerMinute <= 0 {
		errs = append(errs, fmt.Errorf("globalPerMinute must be > 0, got %d", *t.GlobalPerMinute))
	}

	roleNames := make([]string, 0, len(t.RolePercent))
	for role := range t.RolePercent {
		roleNames = append(roleNames, role)
	}
	sort.Strings(roleNames) // deterministic error ordering

	sum := 0.0
	for _, role := range roleNames {
		pct := t.RolePercent[role]
		if role == "" {
			errs = append(errs, errors.New("rolePercent contains an empty role name"))
		}
		if pct <= 0 {
			errs = append(errs, fmt.Errorf("role %q: percent must be > 0, got %v", role, pct))
		}
		sum += pct
	}
	if sum > 100 {
		errs = append(errs, fmt.Errorf("sum of role percentages must be at most 100, got %v", sum))
	}
	return errors.Join(errs...)
}
