// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// rate-limiter is a platform MCP server that enforces per-tool, per-role
// call-rate limits on go-proxy's behalf. It exposes two tools:
//
//   - check_permission: a triggered tool (hidden from ordinary clients) that
//     go-proxy calls automatically, once per call, for any tool present in
//     the "rateLimiting" config section. Its handler blocks on
//     ratelimit.Limiter.Wait, which enforces strict FIFO order among
//     concurrent callers of the same role (see the ratelimit package's own
//     ticketQueue) on top of the usual weighted-share admission logic,
//     until admitted or the caller's own deadline is reached. The deadline
//     is a local context.WithTimeout built from the explicit
//     TimeoutSeconds argument, not inferred from ctx (a Go context
//     deadline doesn't cross the wire) -- go-proxy computes that value
//     from the "rateLimiting" section's own timeoutSeconds. See go-proxy's
//     own rate_limit_gate.go for the caller side.
//   - get_stats: an ordinary tool (admin-only via accessRules, same as any
//     other backend tool -- there's nothing special-cased here) reporting
//     blocked-call counts and average wait time for the last N minutes (up
//     to 3h), broken down by tool and role plus totals.
//
// This server must run as a single pod (see k8s/deployment.yaml's
// replicas: 1): both the Limiter's per-tool decay state and the stats
// recorder below are in-process memory with no cross-replica coordination,
// so a second replica would enforce/report independently-wrong numbers.
package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/configsync"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
	"github.com/pellucon/enterprise-go-mcp-proxy/mcptrace"
	"github.com/pellucon/enterprise-go-mcp-proxy/platform-mcp-servers/rate-limiter/ratelimit"
	"github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxStatsWindow = 3 * time.Hour

// CheckPermissionArgs is check_permission's input. TimeoutSeconds is passed
// explicitly by go-proxy rather than left for this handler to infer from its
// own inbound ctx: a context.WithTimeout deadline set by a caller is a
// local, in-process value that does not travel over the wire, and this
// server's own inbound request ctx carries no deadline info at all
// (verified directly against a real client -- see go-proxy's
// TestPlatformServersOrchestration rate-limiting scenarios) -- so the poll
// loop below bounds itself using this value plus its own time.Now(), not
// ctx.Deadline(). ctx.Done() is still honored as a secondary, best-effort
// signal in case the underlying connection ever is torn down promptly.
type CheckPermissionArgs struct {
	Tool           string   `json:"tool" jsonschema:"the namespaced tool name being checked"`
	Roles          []string `json:"roles" jsonschema:"the caller's roles"`
	TimeoutSeconds *int     `json:"timeoutSeconds,omitempty" jsonschema:"how long the caller is willing to wait, in seconds; absent means wait indefinitely (bounded only by ctx.Done(), if that ever fires)"`
}

// roleKey turns a caller's role set into one deterministic, order-independent
// grouping identity -- used only for stats attribution here (a small,
// intentional duplicate of ratelimit's own unexported canonicalRoleKey,
// since that helper isn't exported and this file can't reach into it).
func roleKey(roles []string) string {
	sorted := append([]string(nil), roles...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

// blockRecord is one completed, ultimately-denied check_permission call.
type blockRecord struct {
	at       time.Time
	tool     string
	roleKey  string
	waitTime time.Duration
}

// statsRecorder tracks blocked calls for up to maxStatsWindow, in memory,
// pruned on every write (records always arrive in non-decreasing time
// order, so pruning from the front is O(evicted), not O(n) per write).
type statsRecorder struct {
	mu      sync.Mutex
	records []blockRecord
}

func (s *statsRecorder) record(tool, roleKey string, waitTime time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.records = append(s.records, blockRecord{at: now, tool: tool, roleKey: roleKey, waitTime: waitTime})
	s.prune(now)
}

func (s *statsRecorder) prune(now time.Time) {
	cutoff := now.Add(-maxStatsWindow)
	i := 0
	for i < len(s.records) && s.records[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		s.records = s.records[i:]
	}
}

// RoleStats is one row of blocked-call stats -- either a specific role's
// row, or a tool/grand total row (Role == "*").
type RoleStats struct {
	Role               string  `json:"role"`
	Count              int     `json:"count"`
	AverageWaitSeconds float64 `json:"averageWaitSeconds"`
}

// ToolStats is one tool's stats: its total row plus a per-role breakdown.
type ToolStats struct {
	Tool   string      `json:"tool"`
	Total  RoleStats   `json:"total"`
	ByRole []RoleStats `json:"byRole"`
}

// StatsArgs is get_stats' input. Minutes defaults to 60 and is clamped to
// [1, 180] (3h, this server's own retention window).
type StatsArgs struct {
	Minutes int `json:"minutes,omitempty" jsonschema:"how many minutes of history to include (default 60, max 180)"`
}

// StatsOutput is get_stats' output.
type StatsOutput struct {
	Minutes int         `json:"minutes"`
	Tools   []ToolStats `json:"tools"`
	Total   RoleStats   `json:"total"`
}

type agg struct {
	count     int
	totalWait time.Duration
}

func (a *agg) row(name string) RoleStats {
	avg := 0.0
	if a.count > 0 {
		avg = a.totalWait.Seconds() / float64(a.count)
	}
	return RoleStats{Role: name, Count: a.count, AverageWaitSeconds: avg}
}

func (s *statsRecorder) stats(minutes int) StatsOutput {
	if minutes <= 0 {
		minutes = 60
	}
	if minutes > 180 {
		minutes = 180
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.prune(now)
	cutoff := now.Add(-time.Duration(minutes) * time.Minute)

	perToolRole := map[string]map[string]*agg{}
	perTool := map[string]*agg{}
	grand := &agg{}
	var toolOrder []string

	for _, r := range s.records {
		if r.at.Before(cutoff) {
			continue
		}
		if _, ok := perToolRole[r.tool]; !ok {
			perToolRole[r.tool] = map[string]*agg{}
			perTool[r.tool] = &agg{}
			toolOrder = append(toolOrder, r.tool)
		}
		if _, ok := perToolRole[r.tool][r.roleKey]; !ok {
			perToolRole[r.tool][r.roleKey] = &agg{}
		}
		perToolRole[r.tool][r.roleKey].count++
		perToolRole[r.tool][r.roleKey].totalWait += r.waitTime
		perTool[r.tool].count++
		perTool[r.tool].totalWait += r.waitTime
		grand.count++
		grand.totalWait += r.waitTime
	}
	sort.Strings(toolOrder)

	out := StatsOutput{Minutes: minutes, Total: grand.row("*")}
	for _, tool := range toolOrder {
		roleNames := make([]string, 0, len(perToolRole[tool]))
		for role := range perToolRole[tool] {
			roleNames = append(roleNames, role)
		}
		sort.Strings(roleNames)

		byRole := make([]RoleStats, 0, len(roleNames))
		for _, role := range roleNames {
			byRole = append(byRole, perToolRole[tool][role].row(role))
		}
		out.Tools = append(out.Tools, ToolStats{
			Tool:   tool,
			Total:  perTool[tool].row("*"),
			ByRole: byRole,
		})
	}
	return out
}

func createServer(limiter *ratelimit.Limiter, stats *statsRecorder) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "rate-limiter", Version: "1.0.0"}, nil)
	server.AddReceivingMiddleware(mcptrace.Middleware())

	mcp.AddTool(server, &mcp.Tool{
		Name:        "check_permission",
		Description: "Request permission to call a rate-limited tool. Blocks internally until admitted or the caller's own deadline is reached.",
		Meta:        mcp.Meta{triggered_tools_runtime.TriggeredToolMetaKey: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args CheckPermissionArgs) (*mcp.CallToolResult, ratelimit.Decision, error) {
		// TimeoutSeconds is applied here, as a LOCAL context.WithTimeout,
		// rather than relying on go-proxy's own outbound deadline to be
		// visible on ctx -- a context.WithTimeout deadline is a local,
		// in-process value that doesn't travel over the wire (this
		// handler's inbound ctx carries no deadline info at all; verified
		// directly, not assumed -- see go-proxy's
		// TestPlatformServersOrchestration rate-limiting scenarios). This
		// is exactly why TimeoutSeconds is a plain argument field instead.
		waitCtx := ctx
		if args.TimeoutSeconds != nil {
			var cancel context.CancelFunc
			waitCtx, cancel = context.WithTimeout(ctx, time.Duration(*args.TimeoutSeconds)*time.Second)
			defer cancel()
		}
		start := time.Now()
		d := limiter.Wait(waitCtx, args.Tool, args.Roles)
		if !d.Allowed {
			stats.record(args.Tool, roleKey(args.Roles), time.Since(start))
		}
		return nil, d, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_stats",
		Description: "Report blocked-call counts and average wait time for the last N minutes (up to 180), broken down by tool and role, plus totals.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args StatsArgs) (*mcp.CallToolResult, StatsOutput, error) {
		return nil, stats.stats(args.Minutes), nil
	})

	return server
}

func main() {
	var port, configPath, natsURL, logLevelConfig string
	flag.StringVar(&port, "port", ":8080", "Port to listen on, e.g. :8080")
	flag.StringVar(&configPath, "config", "./rate_limiting_test.json", `Path to the rate-limiting config file, as a {"version":N,"config":{"tools":{...}}} envelope (matching every configsync-managed section) — this is also what a go-proxy rate-limiting-override ConfigMap is mounted at in k8s`)
	flag.StringVar(&natsURL, "nats-url", "", "NATS server URL for live config reload via a go-proxy _admin.update_rate_limiting call (empty disables live reload: -config is read once at startup)")
	flag.StringVar(&logLevelConfig, "log-level-config", "./log_level_test.json", `Path to the log-level config file, as a {"version":N,"config":{"level":"error|info|debug"}} envelope — also what a go-proxy log-level-override ConfigMap is mounted at in k8s, live-reloaded via the same NATS connection used for rate-limiting config reload; a missing/not-yet-seeded file is not fatal, the process just keeps its default ("info") level`)
	flag.Parse()

	if _, err := os.Stat(configPath); err != nil {
		logging.Fatalf("Rate limiting config file %q not found: %v", configPath, err)
	}

	nc, err := configsync.Connect(natsURL)
	if err != nil {
		logging.Fatalf("Failed to connect to NATS at %q: %v", natsURL, err)
	}

	limiter, err := ratelimit.NewLimiter(nil)
	if err != nil {
		logging.Fatalf("Failed to construct limiter: %v", err)
	}
	applyConfig := func(cfg *ratelimit.Config) {
		if err := limiter.Update(cfg); err != nil {
			logging.Errorf("rate limiting config update rejected: %v", err)
		}
	}

	// initial is nil, not a zero-value fallback: the os.Stat check above
	// already guarantees configPath exists, so NewWatcher always takes the
	// file-parse branch and initial is never actually used - this still
	// fails closed (log.Fatal) on a missing/malformed config.
	if _, err := configsync.NewWatcher("rate-limiting", configPath, nc, (*ratelimit.Config)(nil), ratelimit.ParseConfig, applyConfig); err != nil {
		logging.Fatalf("Failed to load rate limiting config %q: %v", configPath, err)
	}

	// A missing/not-yet-seeded log-level-override ConfigMap must not block
	// startup — unlike configPath above, no os.Stat pre-check: a non-nil
	// initial lets configsync.NewWatcher fall back to it when
	// logLevelConfig doesn't exist yet.
	if _, err := configsync.NewWatcher("log-level", logLevelConfig, nc, &logging.LevelConfig{Level: "info"}, logging.ParseConfig, logging.ApplyConfig); err != nil {
		logging.Fatalf("Failed to load log level config %q: %v", logLevelConfig, err)
	}

	stats := &statsRecorder{}
	server := createServer(limiter, stats)
	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})

	logging.Debugf("Starting rate-limiter on port %s", port)
	if err := http.ListenAndServe(port, handler); err != nil {
		logging.Fatal(err)
	}
}
