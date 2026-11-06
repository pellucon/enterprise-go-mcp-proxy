// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Command generator produces stress-test/data/cli-calls1.txt .. cli-calls5.txt
// — the 5 static, checked-in call files stress-test/driver's REPL clients
// replay. Run manually from the repo root whenever the tool catalog or
// go-gardening-demo seed data changes; NOT invoked automatically by
// stress-test/run.sh, since these files are meant to be reproducible,
// checked-in data, not regenerated on every stress-test run.
//
//	go run ./stress-test/generator
package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
)

// targetGardeningLines / targetProxyLines are approximate targets, not
// exact — "about 5000 calls" per the original spec. The add_supply_order/
// mark_supply_order_fulfilled pairing step below adds extra lines beyond
// targetGardeningLines (one mark_supply_order_fulfilled per
// add_supply_order generated), so a file's final line count always comes
// out a little over these targets, which is expected and fine.
const (
	targetGardeningLines = 4750
	targetProxyLines     = 250
	numFiles             = 5
)

func main() {
	seed, err := loadSeedData()
	if err != nil {
		fmt.Fprintln(os.Stderr, "generator: failed to load seed data:", err)
		os.Exit(1)
	}

	outDir := filepath.Join("stress-test", "data")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "generator: failed to create output dir:", err)
		os.Exit(1)
	}

	for fileIndex := 1; fileIndex <= numFiles; fileIndex++ {
		g := newGenCtx(fileIndex, seed)
		lines, err := generateFile(g)
		if err != nil {
			fmt.Fprintf(os.Stderr, "generator: failed to generate file %d: %v\n", fileIndex, err)
			os.Exit(1)
		}
		outPath := filepath.Join(outDir, fmt.Sprintf("cli-calls%d.txt", fileIndex))
		if err := writeLines(outPath, lines); err != nil {
			fmt.Fprintln(os.Stderr, "generator:", err)
			os.Exit(1)
		}
		fmt.Printf("wrote %s (%d lines)\n", outPath, len(lines))
	}
}

// generateFile builds one file's full call sequence: the weighted
// gardening + proxy/platform pool, shuffled for realistic interleaving,
// then the add_supply_order/mark_supply_order_fulfilled pairing pass, then
// rendered to text lines with the bare-JSON quoting self-check.
func generateFile(g *genCtx) ([]string, error) {
	calls := buildWeightedCalls(g, gardeningTools(), targetGardeningLines)
	calls = append(calls, buildWeightedCalls(g, proxyPlatformTools(), targetProxyLines)...)

	g.rand.Shuffle(len(calls), func(i, j int) { calls[i], calls[j] = calls[j], calls[i] })

	calls = pairSupplyOrderFulfillments(g, calls)

	lines := make([]string, 0, len(calls))
	for _, c := range calls {
		line, err := renderLine(c)
		if err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// buildWeightedCalls allocates target lines across tools proportionally to
// their relative Weight (normalized by the sum of all weights in the
// list), with every tool guaranteed at least gardeningWeightFloor lines
// regardless of how small its raw share would otherwise round to, then
// calls each tool's Build function that many times.
func buildWeightedCalls(g *genCtx, tools []weightedTool, target int) []call {
	sumWeights := 0.0
	for _, t := range tools {
		sumWeights += t.Weight
	}

	counts := make([]int, len(tools))
	for i, t := range tools {
		raw := t.Weight / sumWeights * float64(target)
		counts[i] = int(math.Round(raw))
		if counts[i] < gardeningWeightFloor {
			counts[i] = gardeningWeightFloor
		}
	}

	// Nudge the total back toward `target` by adjusting the
	// highest-weighted tool's count — precision beyond "about N calls"
	// isn't required by the spec, this just keeps the allocation from
	// drifting arbitrarily far off target as tools are added/reweighted.
	total := 0
	highest := 0
	for i, c := range counts {
		total += c
		if tools[i].Weight > tools[highest].Weight {
			highest = i
		}
	}
	diff := target - total
	counts[highest] += diff
	if counts[highest] < gardeningWeightFloor {
		counts[highest] = gardeningWeightFloor
	}

	var calls []call
	for i, t := range tools {
		for n := 0; n < counts[i]; n++ {
			calls = append(calls, call{Tool: t.Tool, Args: t.Build(g)})
		}
	}
	return calls
}

// pairSupplyOrderFulfillments walks calls looking for every
// garden-online-store.add_supply_order entry (its Args["id"] was already
// minted by buildAddSupplyOrder via genCtx.nextSyntheticSupplyOrderID),
// and schedules a matching mark_supply_order_fulfilled call referencing
// that same id at a randomized later position in the same file —
// guaranteeing a valid target deterministically, since supply_orders.json's
// seed rows are all pre-fulfilled and have no valid pending target of
// their own. Insertions are applied from the highest scheduled index down
// to the lowest, so each insertion never invalidates an already-scheduled
// later index.
func pairSupplyOrderFulfillments(g *genCtx, calls []call) []call {
	type scheduled struct {
		insertAt int
		call     call
	}
	var pending []scheduled

	for i, c := range calls {
		if c.Tool != "garden-online-store.add_supply_order" {
			continue
		}
		id, _ := c.Args["id"].(string)
		if id == "" {
			continue
		}
		markArgs := map[string]any{"id": id}
		if g.randBool() {
			markArgs["fulfillmentActualDate"] = nearToday(g, 0, 1)
		}
		insertAt := i + g.randInt(50, 501)
		if insertAt > len(calls) {
			insertAt = len(calls)
		}
		pending = append(pending, scheduled{
			insertAt: insertAt,
			call:     call{Tool: "garden-online-store.mark_supply_order_fulfilled", Args: markArgs},
		})
	}

	sort.Slice(pending, func(i, j int) bool { return pending[i].insertAt > pending[j].insertAt })
	for _, p := range pending {
		calls = append(calls[:p.insertAt], append([]call{p.call}, calls[p.insertAt:]...)...)
	}
	return calls
}

func writeLines(path string, lines []string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer f.Close()
	for _, line := range lines {
		if _, err := fmt.Fprintln(f, line); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
	}
	return nil
}
