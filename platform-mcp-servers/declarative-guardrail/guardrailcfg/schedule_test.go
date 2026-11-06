// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package guardrailcfg

import (
	"testing"
	"time"
)

func mustDate(s string) *string { return &s }

func TestEffectiveDate_StartOnly(t *testing.T) {
	ed, err := parseEffectiveDate(mustDate("2026-01-01"), nil, time.UTC)
	if err != nil {
		t.Fatalf("parseEffectiveDate failed: %v", err)
	}
	if ed.Active(time.Date(2025, 12, 31, 23, 59, 0, 0, time.UTC)) {
		t.Fatal("expected inactive before start")
	}
	if !ed.Active(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("expected active on start day")
	}
	if !ed.Active(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("expected active long after start (open-ended)")
	}
}

func TestEffectiveDate_EndOnly(t *testing.T) {
	ed, err := parseEffectiveDate(nil, mustDate("2026-01-31"), time.UTC)
	if err != nil {
		t.Fatalf("parseEffectiveDate failed: %v", err)
	}
	if !ed.Active(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("expected active long before end (open-ended)")
	}
	if !ed.Active(time.Date(2026, 1, 31, 23, 59, 0, 0, time.UTC)) {
		t.Fatal("expected active through the entirety of the end day")
	}
	if ed.Active(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("expected inactive after end day")
	}
}

func TestEffectiveDate_Range(t *testing.T) {
	ed, err := parseEffectiveDate(mustDate("2026-01-01"), mustDate("2026-01-31"), time.UTC)
	if err != nil {
		t.Fatalf("parseEffectiveDate failed: %v", err)
	}
	if ed.Active(time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("expected inactive before range")
	}
	if !ed.Active(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("expected active within range")
	}
	if ed.Active(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("expected inactive after range")
	}
}

func TestEffectiveDate_RequiresAtLeastOneBound(t *testing.T) {
	if _, err := parseEffectiveDate(nil, nil, time.UTC); err == nil {
		t.Fatal("expected error when neither start nor end is set")
	}
}

func TestEffectiveDate_EndBeforeStartIsError(t *testing.T) {
	if _, err := parseEffectiveDate(mustDate("2026-02-01"), mustDate("2026-01-01"), time.UTC); err == nil {
		t.Fatal("expected error when end is before start")
	}
}

func TestEffectiveDate_BadFormat(t *testing.T) {
	if _, err := parseEffectiveDate(mustDate("not-a-date"), nil, time.UTC); err == nil {
		t.Fatal("expected error for malformed date")
	}
}

func TestEffectiveTimeWindow_SingleDay(t *testing.T) {
	w, err := parseEffectiveTimeWindow([]string{"Monday"}, "09:00", "17:00", time.UTC)
	if err != nil {
		t.Fatalf("parseEffectiveTimeWindow failed: %v", err)
	}
	windows := effectiveTimeWindows{w}

	// Monday, within range.
	if !windows.Active(time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)) {
		t.Fatal("expected active: Monday within range")
	}
	// Monday, outside range.
	if windows.Active(time.Date(2026, 1, 5, 8, 0, 0, 0, time.UTC)) {
		t.Fatal("expected inactive: Monday before start time")
	}
	// Tuesday, within time-of-day range but wrong day.
	if windows.Active(time.Date(2026, 1, 6, 12, 0, 0, 0, time.UTC)) {
		t.Fatal("expected inactive: wrong weekday")
	}
}

func TestEffectiveTimeWindow_MultipleDays(t *testing.T) {
	sun, err := parseEffectiveTimeWindow([]string{"Sunday"}, "09:00", "17:00", time.UTC)
	if err != nil {
		t.Fatalf("parseEffectiveTimeWindow failed: %v", err)
	}
	mon, err := parseEffectiveTimeWindow([]string{"Monday"}, "10:00", "12:00", time.UTC)
	if err != nil {
		t.Fatalf("parseEffectiveTimeWindow failed: %v", err)
	}
	windows := effectiveTimeWindows{sun, mon}

	// 2026-01-04 is a Sunday.
	if !windows.Active(time.Date(2026, 1, 4, 10, 0, 0, 0, time.UTC)) {
		t.Fatal("expected active: Sunday within its own range")
	}
	// 2026-01-05 is a Monday.
	if !windows.Active(time.Date(2026, 1, 5, 11, 0, 0, 0, time.UTC)) {
		t.Fatal("expected active: Monday within its own range")
	}
	if windows.Active(time.Date(2026, 1, 5, 15, 0, 0, 0, time.UTC)) {
		t.Fatal("expected inactive: Monday outside its own range")
	}
}

func TestEffectiveTimeWindow_NoDaysMeansEveryDay(t *testing.T) {
	w, err := parseEffectiveTimeWindow(nil, "09:00", "17:00", time.UTC)
	if err != nil {
		t.Fatalf("parseEffectiveTimeWindow failed: %v", err)
	}
	windows := effectiveTimeWindows{w}

	for day := 0; day < 7; day++ {
		date := time.Date(2026, 1, 4+day, 12, 0, 0, 0, time.UTC) // 2026-01-04 is Sunday
		if !windows.Active(date) {
			t.Fatalf("expected active every day, failed on %v", date.Weekday())
		}
	}
}

func TestEffectiveTimeWindow_BadTimeFormat(t *testing.T) {
	if _, err := parseEffectiveTimeWindow(nil, "9am", "17:00", time.UTC); err == nil {
		t.Fatal("expected error for malformed startTime")
	}
	if _, err := parseEffectiveTimeWindow(nil, "09:00", "5pm", time.UTC); err == nil {
		t.Fatal("expected error for malformed endTime")
	}
}

func TestEffectiveTimeWindow_BadWeekday(t *testing.T) {
	if _, err := parseEffectiveTimeWindow([]string{"Someday"}, "09:00", "17:00", time.UTC); err == nil {
		t.Fatal("expected error for invalid weekday name")
	}
}

func TestEffectiveTimeWindow_EndBeforeOrEqualStartIsError(t *testing.T) {
	if _, err := parseEffectiveTimeWindow(nil, "17:00", "09:00", time.UTC); err == nil {
		t.Fatal("expected error when endTime is before startTime")
	}
	if _, err := parseEffectiveTimeWindow(nil, "09:00", "09:00", time.UTC); err == nil {
		t.Fatal("expected error when endTime equals startTime")
	}
}

// TestEffectiveTimeWindow_NonUTCTimezone proves "09:00"-"17:00" in a
// non-UTC zone is interpreted as local wall-clock time there, not UTC:
// noon UTC is 07:00 in America/New_York (EST, UTC-5) - outside the
// window - but 08:00 UTC is noon EST - inside it.
func TestEffectiveTimeWindow_NonUTCTimezone(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("failed to load America/New_York: %v", err)
	}
	w, err := parseEffectiveTimeWindow([]string{"Monday"}, "09:00", "17:00", loc)
	if err != nil {
		t.Fatalf("parseEffectiveTimeWindow failed: %v", err)
	}
	windows := effectiveTimeWindows{w}

	// 2026-01-05 is a Monday; January is EST (UTC-5), no DST.
	if windows.Active(time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)) {
		t.Fatal("expected inactive: 12:00 UTC is 07:00 EST, before the window")
	}
	if !windows.Active(time.Date(2026, 1, 5, 17, 0, 0, 0, time.UTC)) {
		t.Fatal("expected active: 17:00 UTC is 12:00 EST, within the window")
	}
}

// TestEffectiveTimeWindow_DSTAwareTimezone proves a zone that observes
// DST (unlike a fixed-offset zone such as "EST") shifts its UTC-equivalent
// window between winter and summer.
func TestEffectiveTimeWindow_DSTAwareTimezone(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("failed to load America/New_York: %v", err)
	}
	w, err := parseEffectiveTimeWindow(nil, "09:00", "17:00", loc)
	if err != nil {
		t.Fatalf("parseEffectiveTimeWindow failed: %v", err)
	}
	windows := effectiveTimeWindows{w}

	// 2026-01-05 (winter, EST=UTC-5): 14:00 UTC == 09:00 EST, start of window.
	if !windows.Active(time.Date(2026, 1, 5, 14, 0, 0, 0, time.UTC)) {
		t.Fatal("expected active: 14:00 UTC is 09:00 EST in winter")
	}
	// 2026-07-06 (summer, EDT=UTC-4): 14:00 UTC == 10:00 EDT, already
	// inside the window, but 13:00 UTC == 09:00 EDT is the new start.
	if !windows.Active(time.Date(2026, 7, 6, 13, 0, 0, 0, time.UTC)) {
		t.Fatal("expected active: 13:00 UTC is 09:00 EDT in summer")
	}
	if windows.Active(time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)) {
		t.Fatal("expected inactive: 12:00 UTC is 08:00 EDT in summer, before the window")
	}
}

// TestEffectiveDate_NonUTCTimezone proves an effectiveDate boundary is
// anchored to midnight in the declaration's own timezone, not UTC
// midnight.
func TestEffectiveDate_NonUTCTimezone(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("failed to load America/New_York: %v", err)
	}
	ed, err := parseEffectiveDate(mustDate("2026-01-01"), nil, loc)
	if err != nil {
		t.Fatalf("parseEffectiveDate failed: %v", err)
	}

	// Midnight Jan 1 in America/New_York (EST, UTC-5) is 05:00 UTC.
	if ed.Active(time.Date(2026, 1, 1, 4, 0, 0, 0, time.UTC)) {
		t.Fatal("expected inactive: still Dec 31 in America/New_York")
	}
	if !ed.Active(time.Date(2026, 1, 1, 5, 0, 0, 0, time.UTC)) {
		t.Fatal("expected active: now midnight Jan 1 in America/New_York")
	}
}
