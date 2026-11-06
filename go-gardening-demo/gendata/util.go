// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"math"
	"math/rand"
	"time"
)

const dateLayout = "2006-01-02"

func fmtDate(t time.Time) string {
	return t.Format(dateLayout)
}

// randDate returns a uniformly random date in [start, end] (inclusive).
func randDate(rng *rand.Rand, start, end time.Time) time.Time {
	days := int(end.Sub(start).Hours() / 24)
	if days <= 0 {
		return start
	}
	return start.AddDate(0, 0, rng.Intn(days+1))
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// roundToNinetyNine snaps a computed retail price to the nearest
// "$X.99"-style price point, the way an actual storefront would list it -
// a plain round2 would produce prices like $14.37 that don't read as a
// real price tag. Nearest, not always-up: always rounding up to the next
// X.99 can nearly double a cheap per-unit cost on its own (e.g. $0.60 ->
// $0.99), independent of and compounding with whatever markup was
// applied - rounding to the nearer of the current or next dollar's X.99
// keeps that rounding step roughly neutral instead of systematically
// inflating margins.
func roundToNinetyNine(v float64) float64 {
	if v < 1 {
		return round2(v)
	}
	floor := math.Floor(v)
	down := floor - 0.01 // previous whole dollar's X.99, e.g. 4.99 for v in [4,5)
	up := floor + 0.99   // this range's own X.99, e.g. 5.99 for v in [5,6)
	if v-down <= up-v {
		return down
	}
	return up
}
