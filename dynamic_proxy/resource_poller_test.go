// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package dynamic_proxy

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pellucon/enterprise-go-mcp-proxy/discover_mcp_servers"
)

// resourcePollUpdate captures the result of a single onChanged callback
// invocation so assertions can happen on the test goroutine instead of
// inside the poller's own goroutine.
type resourcePollUpdate struct {
	serverName string
	resources  []*mcp.Resource
}

func newTestResource(uri string) *mcp.Resource {
	return &mcp.Resource{
		URI:         uri,
		Name:        uri,
		Description: "test resource " + uri,
	}
}

func noopResourceHandler(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	return &mcp.ReadResourceResult{}, nil
}

func newTestResourceOnChanged(updates chan<- resourcePollUpdate) ResourcesChangedFunc {
	return func(serverName string, resources []*mcp.Resource) {
		updates <- resourcePollUpdate{serverName: serverName, resources: resources}
	}
}

// waitForResourcePoll blocks until an onChanged callback reports exactly
// `want` (order-independent), ignoring any intermediate updates that don't
// yet match (e.g. duplicate or superseded notifications).
func waitForResourcePoll(t *testing.T, updates <-chan resourcePollUpdate, want []string) {
	t.Helper()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case u := <-updates:
			got := resourceURIs(u.resources)
			if sameElements(got, want) {
				return
			}
			t.Logf("intermediate poll update: %v (want %v)", got, want)
		case <-deadline:
			t.Fatalf("timed out waiting for poll update: want %v", want)
		}
	}
}

func resourceURIs(resources []*mcp.Resource) []string {
	uris := make([]string, len(resources))
	for i, r := range resources {
		uris[i] = r.URI
	}
	return uris
}

func TestResourcePollerDetectsAddedResource(t *testing.T) {
	backend, httpServer := newPollerTestBackend()
	defer httpServer.Close()

	attrs := &discover_mcp_servers.ServerAttributes{ServerName: "test-backend", ServerUrl: httpServer.URL}

	updates := make(chan resourcePollUpdate, 10)
	poller := NewResourcePoller(1, newTestResourceOnChanged(updates))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller.Start(ctx, attrs)
	defer poller.Stop(attrs.ServerName)

	// Baseline: the backend starts out with no resources.
	waitForResourcePoll(t, updates, nil)

	backend.AddResource(newTestResource("file:///one"), noopResourceHandler)
	waitForResourcePoll(t, updates, []string{"file:///one"})
}

func TestResourcePollerDetectsRemovedResource(t *testing.T) {
	backend, httpServer := newPollerTestBackend()
	defer httpServer.Close()

	backend.AddResource(newTestResource("file:///one"), noopResourceHandler)
	backend.AddResource(newTestResource("file:///two"), noopResourceHandler)

	attrs := &discover_mcp_servers.ServerAttributes{ServerName: "test-backend", ServerUrl: httpServer.URL}

	updates := make(chan resourcePollUpdate, 10)
	poller := NewResourcePoller(1, newTestResourceOnChanged(updates))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller.Start(ctx, attrs)
	defer poller.Stop(attrs.ServerName)

	waitForResourcePoll(t, updates, []string{"file:///one", "file:///two"})

	backend.RemoveResources("file:///one")
	waitForResourcePoll(t, updates, []string{"file:///two"})
}

// TestResourcePollerDetectsModifiedResource proves that fingerprinting isn't
// URI-only: the resource URI stays the same across the change, so a
// callback only fires if the description change is actually detected.
func TestResourcePollerDetectsModifiedResource(t *testing.T) {
	backend, httpServer := newPollerTestBackend()
	defer httpServer.Close()

	backend.AddResource(newTestResource("file:///one"), noopResourceHandler)

	attrs := &discover_mcp_servers.ServerAttributes{ServerName: "test-backend", ServerUrl: httpServer.URL}

	updates := make(chan resourcePollUpdate, 10)
	poller := NewResourcePoller(1, newTestResourceOnChanged(updates))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller.Start(ctx, attrs)
	defer poller.Stop(attrs.ServerName)

	waitForResourcePoll(t, updates, []string{"file:///one"})

	// The SDK has no in-place update, so simulate a modified resource via
	// remove+add of the same URI with a different description.
	backend.RemoveResources("file:///one")
	backend.AddResource(&mcp.Resource{
		URI:         "file:///one",
		Name:        "file:///one",
		Description: "an updated description",
	}, noopResourceHandler)

	deadline := time.After(5 * time.Second)
	for {
		select {
		case u := <-updates:
			if len(u.resources) == 1 && u.resources[0].Description == "an updated description" {
				return
			}
			t.Logf("intermediate poll update: %+v", u.resources)
		case <-deadline:
			t.Fatalf("timed out waiting for modified-resource update to be detected")
		}
	}
}

func TestResourcePollerNoCallbackWhenUnchanged(t *testing.T) {
	backend, httpServer := newPollerTestBackend()
	defer httpServer.Close()

	backend.AddResource(newTestResource("file:///one"), noopResourceHandler)

	attrs := &discover_mcp_servers.ServerAttributes{ServerName: "test-backend", ServerUrl: httpServer.URL}

	updates := make(chan resourcePollUpdate, 10)
	poller := NewResourcePoller(1, newTestResourceOnChanged(updates))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller.Start(ctx, attrs)
	defer poller.Stop(attrs.ServerName)

	waitForResourcePoll(t, updates, []string{"file:///one"})

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
		t.Fatalf("expected no further callbacks once resources stabilized, got %d", count)
	}
}

func TestResourcePollerStopHaltsPolling(t *testing.T) {
	backend, httpServer := newPollerTestBackend()
	defer httpServer.Close()

	attrs := &discover_mcp_servers.ServerAttributes{ServerName: "test-backend", ServerUrl: httpServer.URL}

	updates := make(chan resourcePollUpdate, 10)
	poller := NewResourcePoller(1, newTestResourceOnChanged(updates))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller.Start(ctx, attrs)

	waitForResourcePoll(t, updates, nil)

	poller.Stop(attrs.ServerName)

	backend.AddResource(newTestResource("file:///one"), noopResourceHandler)

	select {
	case u := <-updates:
		t.Fatalf("expected no callback after Stop, got %+v", u)
	case <-time.After(3 * time.Second):
	}
}
