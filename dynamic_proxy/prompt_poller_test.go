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

// promptPollUpdate captures the result of a single onChanged callback
// invocation so assertions can happen on the test goroutine instead of
// inside the poller's own goroutine.
type promptPollUpdate struct {
	serverName string
	prompts    []*mcp.Prompt
}

func newTestPrompt(name string) *mcp.Prompt {
	return &mcp.Prompt{
		Name:        name,
		Description: "test prompt " + name,
	}
}

func noopPromptHandler(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	return &mcp.GetPromptResult{}, nil
}

func newTestPromptOnChanged(updates chan<- promptPollUpdate) PromptsChangedFunc {
	return func(serverName string, prompts []*mcp.Prompt) {
		updates <- promptPollUpdate{serverName: serverName, prompts: prompts}
	}
}

// waitForPromptPoll blocks until an onChanged callback reports exactly
// `want` (order-independent), ignoring any intermediate updates that don't
// yet match (e.g. duplicate or superseded notifications).
func waitForPromptPoll(t *testing.T, updates <-chan promptPollUpdate, want []string) {
	t.Helper()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case u := <-updates:
			got := promptNames(u.prompts)
			if sameElements(got, want) {
				return
			}
			t.Logf("intermediate poll update: %v (want %v)", got, want)
		case <-deadline:
			t.Fatalf("timed out waiting for poll update: want %v", want)
		}
	}
}

func promptNames(prompts []*mcp.Prompt) []string {
	names := make([]string, len(prompts))
	for i, p := range prompts {
		names[i] = p.Name
	}
	return names
}

func TestPromptPollerDetectsAddedPrompt(t *testing.T) {
	backend, httpServer := newPollerTestBackend()
	defer httpServer.Close()

	attrs := &discover_mcp_servers.ServerAttributes{ServerName: "test-backend", ServerUrl: httpServer.URL}

	updates := make(chan promptPollUpdate, 10)
	poller := NewPromptPoller(1, newTestPromptOnChanged(updates))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller.Start(ctx, attrs)
	defer poller.Stop(attrs.ServerName)

	// Baseline: the backend starts out with no prompts.
	waitForPromptPoll(t, updates, nil)

	backend.AddPrompt(newTestPrompt("prompt_one"), noopPromptHandler)
	waitForPromptPoll(t, updates, []string{"prompt_one"})
}

func TestPromptPollerDetectsRemovedPrompt(t *testing.T) {
	backend, httpServer := newPollerTestBackend()
	defer httpServer.Close()

	backend.AddPrompt(newTestPrompt("prompt_one"), noopPromptHandler)
	backend.AddPrompt(newTestPrompt("prompt_two"), noopPromptHandler)

	attrs := &discover_mcp_servers.ServerAttributes{ServerName: "test-backend", ServerUrl: httpServer.URL}

	updates := make(chan promptPollUpdate, 10)
	poller := NewPromptPoller(1, newTestPromptOnChanged(updates))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller.Start(ctx, attrs)
	defer poller.Stop(attrs.ServerName)

	waitForPromptPoll(t, updates, []string{"prompt_one", "prompt_two"})

	backend.RemovePrompts("prompt_one")
	waitForPromptPoll(t, updates, []string{"prompt_two"})
}

// TestPromptPollerDetectsModifiedPrompt proves that fingerprinting isn't
// name-only: the prompt name stays the same across the change, so a
// callback only fires if the description change is actually detected.
func TestPromptPollerDetectsModifiedPrompt(t *testing.T) {
	backend, httpServer := newPollerTestBackend()
	defer httpServer.Close()

	backend.AddPrompt(newTestPrompt("prompt_one"), noopPromptHandler)

	attrs := &discover_mcp_servers.ServerAttributes{ServerName: "test-backend", ServerUrl: httpServer.URL}

	updates := make(chan promptPollUpdate, 10)
	poller := NewPromptPoller(1, newTestPromptOnChanged(updates))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller.Start(ctx, attrs)
	defer poller.Stop(attrs.ServerName)

	waitForPromptPoll(t, updates, []string{"prompt_one"})

	// The SDK has no in-place update, so simulate a modified prompt via
	// remove+add of the same name with a different description.
	backend.RemovePrompts("prompt_one")
	backend.AddPrompt(&mcp.Prompt{
		Name:        "prompt_one",
		Description: "an updated description",
	}, noopPromptHandler)

	deadline := time.After(5 * time.Second)
	for {
		select {
		case u := <-updates:
			if len(u.prompts) == 1 && u.prompts[0].Description == "an updated description" {
				return
			}
			t.Logf("intermediate poll update: %+v", u.prompts)
		case <-deadline:
			t.Fatalf("timed out waiting for modified-prompt update to be detected")
		}
	}
}

func TestPromptPollerNoCallbackWhenUnchanged(t *testing.T) {
	backend, httpServer := newPollerTestBackend()
	defer httpServer.Close()

	backend.AddPrompt(newTestPrompt("prompt_one"), noopPromptHandler)

	attrs := &discover_mcp_servers.ServerAttributes{ServerName: "test-backend", ServerUrl: httpServer.URL}

	updates := make(chan promptPollUpdate, 10)
	poller := NewPromptPoller(1, newTestPromptOnChanged(updates))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller.Start(ctx, attrs)
	defer poller.Stop(attrs.ServerName)

	waitForPromptPoll(t, updates, []string{"prompt_one"})

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
		t.Fatalf("expected no further callbacks once prompts stabilized, got %d", count)
	}
}

func TestPromptPollerStopHaltsPolling(t *testing.T) {
	backend, httpServer := newPollerTestBackend()
	defer httpServer.Close()

	attrs := &discover_mcp_servers.ServerAttributes{ServerName: "test-backend", ServerUrl: httpServer.URL}

	updates := make(chan promptPollUpdate, 10)
	poller := NewPromptPoller(1, newTestPromptOnChanged(updates))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller.Start(ctx, attrs)

	waitForPromptPoll(t, updates, nil)

	poller.Stop(attrs.ServerName)

	backend.AddPrompt(newTestPrompt("prompt_one"), noopPromptHandler)

	select {
	case u := <-updates:
		t.Fatalf("expected no callback after Stop, got %+v", u)
	case <-time.After(3 * time.Second):
	}
}
