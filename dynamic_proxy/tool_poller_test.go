// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package dynamic_proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pellucon/enterprise-go-mcp-proxy/discover_mcp_servers"
)

// pollUpdate captures the result of a single onChanged callback invocation
// so assertions can happen on the test goroutine instead of inside the
// poller's own goroutine.
type pollUpdate struct {
	serverName string
	tools      []*mcp.Tool
}

func newTestTool(name string) *mcp.Tool {
	return &mcp.Tool{
		Name:        name,
		Description: "test tool " + name,
		InputSchema: map[string]any{"type": "object"},
	}
}

func noopToolHandler(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{}, nil, nil
}

// newPollerTestBackend stands up a real, self-contained MCP server so tests
// don't depend on any externally-started process.
func newPollerTestBackend() (*mcp.Server, *httptest.Server) {
	backend := mcp.NewServer(&mcp.Implementation{
		Name:    "test-backend",
		Version: "1.0.0",
	}, nil)
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return backend
	}, nil))
	return backend, httpServer
}

func newTestOnChanged(updates chan<- pollUpdate) ToolsChangedFunc {
	return func(serverName string, tools []*mcp.Tool) {
		updates <- pollUpdate{serverName: serverName, tools: tools}
	}
}

// waitForPoll blocks until an onChanged callback reports exactly `want`
// (order-independent), ignoring any intermediate updates that don't yet
// match (e.g. duplicate or superseded notifications).
func waitForPoll(t *testing.T, updates <-chan pollUpdate, want []string) {
	t.Helper()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case u := <-updates:
			got := toolNames(u.tools)
			if sameElements(got, want) {
				return
			}
			t.Logf("intermediate poll update: %v (want %v)", got, want)
		case <-deadline:
			t.Fatalf("timed out waiting for poll update: want %v", want)
		}
	}
}

func TestToolPollerDetectsAddedTool(t *testing.T) {
	backend, httpServer := newPollerTestBackend()
	defer httpServer.Close()

	attrs := &discover_mcp_servers.ServerAttributes{ServerName: "test-backend", ServerUrl: httpServer.URL}

	updates := make(chan pollUpdate, 10)
	poller := NewToolPoller(1, newTestOnChanged(updates))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller.Start(ctx, attrs)
	defer poller.Stop(attrs.ServerName)

	// Baseline: the backend starts out with no tools.
	waitForPoll(t, updates, nil)

	mcp.AddTool(backend, newTestTool("tool_one"), noopToolHandler)
	waitForPoll(t, updates, []string{"tool_one"})
}

func TestToolPollerDetectsRemovedTool(t *testing.T) {
	backend, httpServer := newPollerTestBackend()
	defer httpServer.Close()

	mcp.AddTool(backend, newTestTool("tool_one"), noopToolHandler)
	mcp.AddTool(backend, newTestTool("tool_two"), noopToolHandler)

	attrs := &discover_mcp_servers.ServerAttributes{ServerName: "test-backend", ServerUrl: httpServer.URL}

	updates := make(chan pollUpdate, 10)
	poller := NewToolPoller(1, newTestOnChanged(updates))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller.Start(ctx, attrs)
	defer poller.Stop(attrs.ServerName)

	waitForPoll(t, updates, []string{"tool_one", "tool_two"})

	backend.RemoveTools("tool_one")
	waitForPoll(t, updates, []string{"tool_two"})
}

// TestToolPollerDetectsModifiedTool proves that fingerprinting isn't
// name-only: the tool name stays the same across the change, so a callback
// only fires if the description/input-schema change is actually detected.
func TestToolPollerDetectsModifiedTool(t *testing.T) {
	backend, httpServer := newPollerTestBackend()
	defer httpServer.Close()

	mcp.AddTool(backend, newTestTool("tool_one"), noopToolHandler)

	attrs := &discover_mcp_servers.ServerAttributes{ServerName: "test-backend", ServerUrl: httpServer.URL}

	updates := make(chan pollUpdate, 10)
	poller := NewToolPoller(1, newTestOnChanged(updates))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller.Start(ctx, attrs)
	defer poller.Stop(attrs.ServerName)

	waitForPoll(t, updates, []string{"tool_one"})

	// The SDK has no in-place update, so simulate a modified tool via
	// remove+add of the same name with a different description/schema.
	backend.RemoveTools("tool_one")
	mcp.AddTool(backend, &mcp.Tool{
		Name:        "tool_one",
		Description: "an updated description",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}},
	}, noopToolHandler)

	deadline := time.After(5 * time.Second)
	for {
		select {
		case u := <-updates:
			if len(u.tools) == 1 && u.tools[0].Description == "an updated description" {
				return
			}
			t.Logf("intermediate poll update: %+v", u.tools)
		case <-deadline:
			t.Fatalf("timed out waiting for modified-tool update to be detected")
		}
	}
}

func TestToolPollerNoCallbackWhenUnchanged(t *testing.T) {
	backend, httpServer := newPollerTestBackend()
	defer httpServer.Close()

	mcp.AddTool(backend, newTestTool("tool_one"), noopToolHandler)

	attrs := &discover_mcp_servers.ServerAttributes{ServerName: "test-backend", ServerUrl: httpServer.URL}

	updates := make(chan pollUpdate, 10)
	poller := NewToolPoller(1, newTestOnChanged(updates))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller.Start(ctx, attrs)
	defer poller.Stop(attrs.ServerName)

	waitForPoll(t, updates, []string{"tool_one"})

	// Let a few more ticks pass with nothing changing on the backend.
	time.Sleep(3 * time.Second)

	count := 0
drain:
	for {
		select {
		case <-updates:
			count++
		default:
			break drain
		}
	}
	if count != 0 {
		t.Fatalf("expected no further callbacks once tools stabilized, got %d", count)
	}
}

func TestToolPollerStopHaltsPolling(t *testing.T) {
	backend, httpServer := newPollerTestBackend()
	defer httpServer.Close()

	attrs := &discover_mcp_servers.ServerAttributes{ServerName: "test-backend", ServerUrl: httpServer.URL}

	updates := make(chan pollUpdate, 10)
	poller := NewToolPoller(1, newTestOnChanged(updates))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller.Start(ctx, attrs)

	waitForPoll(t, updates, nil)

	poller.Stop(attrs.ServerName)

	mcp.AddTool(backend, newTestTool("tool_one"), noopToolHandler)

	select {
	case u := <-updates:
		t.Fatalf("expected no callback after Stop, got %+v", u)
	case <-time.After(3 * time.Second):
	}
}

// TestFeaturePollerPendingFirstPolls checks the readiness signal: a started
// server counts as pending until its first poll has been attempted, and a
// failed attempt counts as done (one unreachable backend mustn't hold the
// proxy back).
func TestFeaturePollerPendingFirstPolls(t *testing.T) {
	release := make(chan struct{})
	fetch := func(ctx context.Context, attrs *discover_mcp_servers.ServerAttributes) ([]*mcp.Tool, error) {
		if attrs.ServerName == "slow" {
			<-release
		}
		return nil, context.DeadlineExceeded // a failed poll still counts as attempted
	}
	p := NewFeaturePoller[*mcp.Tool](60, "TestPoller", fetch, func(tl *mcp.Tool) string { return tl.Name }, func(string, []*mcp.Tool) {})

	if got := p.PendingFirstPolls(); got != 0 {
		t.Fatalf("before any Start: pending = %d, want 0", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx, &discover_mcp_servers.ServerAttributes{ServerName: "fast"})
	p.Start(ctx, &discover_mcp_servers.ServerAttributes{ServerName: "slow"})

	waitFor := func(want int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if p.PendingFirstPolls() == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("pending = %d, want %d", p.PendingFirstPolls(), want)
	}
	waitFor(1) // "fast" failed quickly (counts as done); "slow" still in flight
	close(release)
	waitFor(0)

	p.Stop("slow")
	if got := p.PendingFirstPolls(); got != 0 {
		t.Errorf("after Stop: pending = %d, want 0", got)
	}
}
