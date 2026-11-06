// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"math/rand"
)

// genCtx is one file's generation context: its own seeded random source
// (so file N is reproducible run-to-run but distinct from every other
// file), the shared seed-data pools, and the small pieces of mutable
// state a single file's generation needs (round-robin cursors into the
// two small/finite id pools, and a counter for minting synthetic
// supply-order ids).
type genCtx struct {
	fileIndex int
	rand      *rand.Rand
	seed      *SeedData

	pendingOrderCursor   int
	unresolvedCallCursor int
	syntheticSeq         int // shared monotonic counter for every kind of synthetic id this file mints
}

func newGenCtx(fileIndex int, seed *SeedData) *genCtx {
	return &genCtx{
		fileIndex: fileIndex,
		// Seeded by file index alone: same fileIndex always reproduces
		// the same file byte-for-byte on a re-run of the generator,
		// while different file indices diverge from the very first
		// pick — this is what makes the 5 files simultaneously
		// deterministic (per file) and non-identical (across files).
		rand: rand.New(rand.NewSource(int64(fileIndex))),
		seed: seed,
	}
}

func (g *genCtx) pick(pool []string) string {
	return pool[g.rand.Intn(len(pool))]
}

func (g *genCtx) pickN(pool []string, n int) []string {
	if n > len(pool) {
		n = len(pool)
	}
	idx := g.rand.Perm(len(pool))[:n]
	out := make([]string, n)
	for i, j := range idx {
		out[i] = pool[j]
	}
	return out
}

func (g *genCtx) randInt(lo, hi int) int {
	if hi <= lo {
		return lo
	}
	return lo + g.rand.Intn(hi-lo)
}

func (g *genCtx) randFloat(lo, hi float64) float64 {
	return lo + g.rand.Float64()*(hi-lo)
}

func (g *genCtx) randBool() bool {
	return g.rand.Intn(2) == 0
}

// nextPendingOrderID round-robins the shared PendingOrderIDs pool. The
// pool is intentionally shared/reused across repeated calls and across
// all 5 files: purchase_fulfilled has no already-fulfilled guard (see
// garden-online-store/main.go), so fulfilling the same order twice is a
// harmless no-op, never a source of business errors.
func (g *genCtx) nextPendingOrderID() string {
	id := g.seed.PendingOrderIDs[g.pendingOrderCursor%len(g.seed.PendingOrderIDs)]
	g.pendingOrderCursor++
	return id
}

// nextUnresolvedCallID round-robins the (typically very small, single-digit)
// UnresolvedServiceCallIDs pool. Once exhausted the pool just keeps
// cycling, and resolve_customer_service_call calls against an
// already-resolved id are an expected, spec-consistent business error
// (see stress-test/README.md), not a generation defect.
func (g *genCtx) nextUnresolvedCallID() string {
	id := g.seed.UnresolvedServiceCallIDs[g.unresolvedCallCursor%len(g.seed.UnresolvedServiceCallIDs)]
	g.unresolvedCallCursor++
	return id
}

// nextSyntheticSupplyOrderID mints a fresh id for a new add_supply_order
// call, guaranteed unique within this file and never colliding with
// another file's synthetic ids (both the file index and a per-file
// monotonic sequence number are embedded), so a later paired
// mark_supply_order_fulfilled call can reference it deterministically.
func (g *genCtx) nextSyntheticSupplyOrderID() string {
	return fmt.Sprintf("stress-%d-so-%d", g.fileIndex, g.nextSeq())
}

// nextSyntheticID mints a fresh, file-unique id of the given kind (e.g.
// "ps", "cust", "staff") sharing the same monotonic counter as
// nextSyntheticSupplyOrderID — the kind prefix alone is enough to keep
// different id categories from ever colliding with each other or with a
// real seed id (none of which start with "stress-").
func (g *genCtx) nextSyntheticID(kind string) string {
	return fmt.Sprintf("stress-%d-%s-%d", g.fileIndex, kind, g.nextSeq())
}

func (g *genCtx) nextSeq() int {
	g.syntheticSeq++
	return g.syntheticSeq
}
