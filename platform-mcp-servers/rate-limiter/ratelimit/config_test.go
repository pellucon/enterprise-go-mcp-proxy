// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package ratelimit

import (
	"strings"
	"testing"
)

func intPtr(v int) *int { return &v }

func TestConfig_Validate_NilConfig(t *testing.T) {
	var cfg *Config
	if err := cfg.Validate(); err != nil {
		t.Fatalf("nil config should be valid, got: %v", err)
	}
}

func TestConfig_Validate_EmptyConfig(t *testing.T) {
	cfg := &Config{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty config should be valid, got: %v", err)
	}
}

func TestConfig_Validate_ValidConfigs(t *testing.T) {
	tests := map[string]*Config{
		"unlimited tool, no role percent": {
			Tools: map[string]ToolConfig{"t": {}},
		},
		"global limit only, no role percent": {
			Tools: map[string]ToolConfig{"t": {GlobalPerMinute: intPtr(100)}},
		},
		"global limit with one role under 100": {
			Tools: map[string]ToolConfig{"t": {
				GlobalPerMinute: intPtr(100),
				RolePercent:     map[string]float64{"a": 50},
			}},
		},
		"global limit with several roles summing just under 100": {
			Tools: map[string]ToolConfig{"t": {
				GlobalPerMinute: intPtr(100),
				RolePercent:     map[string]float64{"a": 70, "b": 20, "c": 9.99},
			}},
		},
		"global limit with roles summing to exactly 100": {
			Tools: map[string]ToolConfig{"t": {
				GlobalPerMinute: intPtr(100),
				RolePercent:     map[string]float64{"a": 60, "b": 40},
			}},
		},
		"a single role configured at exactly 100": {
			Tools: map[string]ToolConfig{"t": {
				GlobalPerMinute: intPtr(100),
				RolePercent:     map[string]float64{"a": 100},
			}},
		},
		"multiple independently-valid tools": {
			Tools: map[string]ToolConfig{
				"t1": {GlobalPerMinute: intPtr(10)},
				"t2": {GlobalPerMinute: intPtr(20), RolePercent: map[string]float64{"x": 10}},
			},
		},
		"with a timeout configured": {
			TimeoutSeconds: intPtr(5),
			Tools:          map[string]ToolConfig{"t": {GlobalPerMinute: intPtr(10)}},
		},
	}
	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			if err := cfg.Validate(); err != nil {
				t.Fatalf("expected valid config, got error: %v", err)
			}
		})
	}
}

func TestConfig_Validate_RejectsNonPositiveGlobal(t *testing.T) {
	for _, g := range []int{0, -1, -100} {
		cfg := &Config{Tools: map[string]ToolConfig{"t": {GlobalPerMinute: intPtr(g)}}}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("globalPerMinute=%d: expected validation error, got nil", g)
		}
	}
}

func TestConfig_Validate_RejectsRolePercentWithoutGlobalLimit(t *testing.T) {
	cfg := &Config{Tools: map[string]ToolConfig{"t": {
		RolePercent: map[string]float64{"a": 10},
	}}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for rolePercent without a global limit")
	}
	if !strings.Contains(err.Error(), "globalPerMinute is nil") {
		t.Fatalf("expected error to mention the missing global limit, got: %v", err)
	}
}

func TestConfig_Validate_RejectsNonPositivePercent(t *testing.T) {
	for _, pct := range []float64{0, -5} {
		cfg := &Config{Tools: map[string]ToolConfig{"t": {
			GlobalPerMinute: intPtr(100),
			RolePercent:     map[string]float64{"a": pct},
		}}}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("percent=%v: expected validation error, got nil", pct)
		}
	}
}

func TestConfig_Validate_RejectsEmptyRoleName(t *testing.T) {
	cfg := &Config{Tools: map[string]ToolConfig{"t": {
		GlobalPerMinute: intPtr(100),
		RolePercent:     map[string]float64{"": 10},
	}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for an empty role name")
	}
}

func TestConfig_Validate_RejectsSumAbove100(t *testing.T) {
	tests := map[string]map[string]float64{
		"sum just over 100":  {"a": 60, "b": 40.01},
		"sum well above 100": {"a": 70, "b": 50},
	}
	for name, pcts := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := &Config{Tools: map[string]ToolConfig{"t": {
				GlobalPerMinute: intPtr(100),
				RolePercent:     pcts,
			}}}
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected validation error for sum > 100")
			}
			if !strings.Contains(err.Error(), "at most 100") {
				t.Fatalf("expected error to mention the <=100 requirement, got: %v", err)
			}
		})
	}
}

func TestConfig_Validate_SumExactly100IsNowValid(t *testing.T) {
	// Confirms the relaxed rule from v2: sum may reach exactly 100 (leaving
	// zero floor for unconfigured roles -- accepted, see limiter_test.go's
	// TestCheck_UnconfiguredPool_StarvesWhenSumIsExactly100 for the runtime
	// consequence).
	cfg := &Config{Tools: map[string]ToolConfig{"t": {
		GlobalPerMinute: intPtr(100),
		RolePercent:     map[string]float64{"a": 60, "b": 40},
	}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("sum of exactly 100 should now be valid, got: %v", err)
	}
}

func TestConfig_Validate_SumJustUnder100IsValid(t *testing.T) {
	cfg := &Config{Tools: map[string]ToolConfig{"t": {
		GlobalPerMinute: intPtr(100),
		RolePercent:     map[string]float64{"a": 99.9},
	}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("sum just under 100 should be valid, got: %v", err)
	}
}

func TestConfig_Validate_AggregatesMultipleErrorsAcrossTools(t *testing.T) {
	cfg := &Config{Tools: map[string]ToolConfig{
		"bad-global": {GlobalPerMinute: intPtr(-1)},
		"bad-sum": {
			GlobalPerMinute: intPtr(100),
			RolePercent:     map[string]float64{"a": 60, "b": 60},
		},
		"good": {GlobalPerMinute: intPtr(10)},
	}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected an aggregated validation error")
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("expected an errors.Join-style error, got %T", err)
	}
	errs := joined.Unwrap()
	if len(errs) != 2 {
		t.Fatalf("expected exactly 2 per-tool errors (the 'good' tool contributes none), got %d: %v", len(errs), errs)
	}
	if !strings.Contains(err.Error(), "bad-global") || !strings.Contains(err.Error(), "bad-sum") {
		t.Fatalf("expected both offending tool names in the error, got: %v", err)
	}
	if strings.Contains(err.Error(), `"good"`) {
		t.Fatalf("did not expect the valid tool to be mentioned, got: %v", err)
	}
}

func TestParseConfig_RoundTrip(t *testing.T) {
	data := []byte(`{
		"timeoutSeconds": 5,
		"tools": {
			"garden-online-store.purchase_products": {
				"globalPerMinute": 100,
				"rolePercent": {"business-manager": 70, "customer": 20}
			}
		}
	}`)
	cfg, err := ParseConfig(data)
	if err != nil {
		t.Fatalf("ParseConfig: unexpected error: %v", err)
	}
	if cfg.TimeoutSeconds == nil || *cfg.TimeoutSeconds != 5 {
		t.Fatalf("expected timeoutSeconds=5, got %+v", cfg.TimeoutSeconds)
	}
	tc, ok := cfg.Tools["garden-online-store.purchase_products"]
	if !ok || tc.GlobalPerMinute == nil || *tc.GlobalPerMinute != 100 {
		t.Fatalf("expected the tool entry to round-trip, got %+v (ok=%v)", tc, ok)
	}
	if tc.RolePercent["business-manager"] != 70 || tc.RolePercent["customer"] != 20 {
		t.Fatalf("expected rolePercent to round-trip, got %+v", tc.RolePercent)
	}
}

func TestParseConfig_EmptyInputIsValidNoOp(t *testing.T) {
	cfg, err := ParseConfig(nil)
	if err != nil {
		t.Fatalf("ParseConfig(nil): unexpected error: %v", err)
	}
	if len(cfg.Tools) != 0 || cfg.TimeoutSeconds != nil {
		t.Fatalf("expected a zero-value Config, got %+v", cfg)
	}
}

func TestParseConfig_RejectsInvalidConfig(t *testing.T) {
	data := []byte(`{"tools": {"t": {"globalPerMinute": -1}}}`)
	if _, err := ParseConfig(data); err == nil {
		t.Fatal("expected ParseConfig to reject an invalid config")
	}
}
