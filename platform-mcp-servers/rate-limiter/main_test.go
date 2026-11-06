// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"testing"
	"time"
)

func TestStatsRecorder_EmptyReportsZeroTotals(t *testing.T) {
	s := &statsRecorder{}
	out := s.stats(60)
	if out.Total.Count != 0 || out.Total.AverageWaitSeconds != 0 || len(out.Tools) != 0 {
		t.Fatalf("expected an all-zero report for an empty recorder, got %+v", out)
	}
}

func TestStatsRecorder_AggregatesByToolAndRole(t *testing.T) {
	s := &statsRecorder{}
	s.record("toolA", "guest", 1*time.Second)
	s.record("toolA", "guest", 3*time.Second)
	s.record("toolA", "admin", 2*time.Second)
	s.record("toolB", "guest", 4*time.Second)

	out := s.stats(60)

	if out.Total.Count != 4 {
		t.Fatalf("expected total count 4, got %d", out.Total.Count)
	}
	wantTotalAvg := (1.0 + 3.0 + 2.0 + 4.0) / 4.0
	if out.Total.AverageWaitSeconds != wantTotalAvg {
		t.Fatalf("expected total average %v, got %v", wantTotalAvg, out.Total.AverageWaitSeconds)
	}
	if len(out.Tools) != 2 {
		t.Fatalf("expected 2 distinct tools, got %d: %+v", len(out.Tools), out.Tools)
	}

	var toolA *ToolStats
	for i := range out.Tools {
		if out.Tools[i].Tool == "toolA" {
			toolA = &out.Tools[i]
		}
	}
	if toolA == nil {
		t.Fatalf("expected a toolA entry, got %+v", out.Tools)
	}
	if toolA.Total.Count != 3 {
		t.Fatalf("expected toolA total count 3, got %d", toolA.Total.Count)
	}
	wantToolAAvg := (1.0 + 3.0 + 2.0) / 3.0
	if toolA.Total.AverageWaitSeconds != wantToolAAvg {
		t.Fatalf("expected toolA average %v, got %v", wantToolAAvg, toolA.Total.AverageWaitSeconds)
	}
	if len(toolA.ByRole) != 2 {
		t.Fatalf("expected 2 distinct roles for toolA, got %d: %+v", len(toolA.ByRole), toolA.ByRole)
	}
	for _, r := range toolA.ByRole {
		switch r.Role {
		case "guest":
			if r.Count != 2 {
				t.Errorf("expected guest count 2 for toolA, got %d", r.Count)
			}
			if want := (1.0 + 3.0) / 2.0; r.AverageWaitSeconds != want {
				t.Errorf("expected guest average %v for toolA, got %v", want, r.AverageWaitSeconds)
			}
		case "admin":
			if r.Count != 1 {
				t.Errorf("expected admin count 1 for toolA, got %d", r.Count)
			}
			if r.AverageWaitSeconds != 2.0 {
				t.Errorf("expected admin average 2.0 for toolA, got %v", r.AverageWaitSeconds)
			}
		default:
			t.Errorf("unexpected role %q in toolA's breakdown", r.Role)
		}
	}
}

func TestStatsRecorder_MinutesClampedAndDefaulted(t *testing.T) {
	s := &statsRecorder{}
	s.record("t", "r", time.Second)

	if out := s.stats(0); out.Minutes != 60 {
		t.Fatalf("expected minutes=0 to default to 60, got %d", out.Minutes)
	}
	if out := s.stats(500); out.Minutes != 180 {
		t.Fatalf("expected minutes=500 to clamp to 180, got %d", out.Minutes)
	}
	if out := s.stats(-5); out.Minutes != 60 {
		t.Fatalf("expected a negative minutes to default to 60, got %d", out.Minutes)
	}
}

func TestStatsRecorder_ExcludesRecordsOutsideRequestedWindow(t *testing.T) {
	s := &statsRecorder{}
	now := time.Now()
	s.records = []blockRecord{
		{at: now.Add(-90 * time.Minute), tool: "old", roleKey: "r", waitTime: time.Second},
		{at: now.Add(-1 * time.Minute), tool: "recent", roleKey: "r", waitTime: time.Second},
	}

	out := s.stats(60) // only the last 60 minutes
	if out.Total.Count != 1 {
		t.Fatalf("expected only the recent record to count within a 60-minute window, got total=%d: %+v", out.Total.Count, out)
	}
	if len(out.Tools) != 1 || out.Tools[0].Tool != "recent" {
		t.Fatalf("expected only 'recent' tool to appear, got %+v", out.Tools)
	}
}

func TestStatsRecorder_PrunesEntriesOlderThanMaxWindow(t *testing.T) {
	s := &statsRecorder{}
	now := time.Now()
	// Directly seed state to avoid a real 3h+ sleep: one very old entry (past
	// the 3h retention window) and one recent one.
	s.records = []blockRecord{
		{at: now.Add(-4 * time.Hour), tool: "ancient", roleKey: "r", waitTime: time.Second},
		{at: now.Add(-1 * time.Minute), tool: "recent", roleKey: "r", waitTime: time.Second},
	}
	s.prune(now)

	if len(s.records) != 1 || s.records[0].tool != "recent" {
		t.Fatalf("expected pruning to drop the entry older than the 3h retention window, got %+v", s.records)
	}

	// Even asking for the full 180-minute window shouldn't resurrect it.
	out := s.stats(180)
	if out.Total.Count != 1 {
		t.Fatalf("expected the pruned entry to stay gone even at the max window, got total=%d", out.Total.Count)
	}
}

func TestRoleKey_OrderIndependentGrouping(t *testing.T) {
	if roleKey([]string{"b", "a"}) != roleKey([]string{"a", "b"}) {
		t.Fatalf("expected role sets differing only by order to produce the same key")
	}
	if roleKey([]string{"a"}) == roleKey([]string{"a", "b"}) {
		t.Fatalf("expected different role sets to produce different keys")
	}
}
