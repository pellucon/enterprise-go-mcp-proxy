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

// resourceTemplatePollUpdate captures the result of a single onChanged
// callback invocation so assertions can happen on the test goroutine instead
// of inside the poller's own goroutine.
type resourceTemplatePollUpdate struct {
	serverName string
	templates  []*mcp.ResourceTemplate
}

func newTestResourceTemplate(uriTemplate string) *mcp.ResourceTemplate {
	return &mcp.ResourceTemplate{
		URITemplate: uriTemplate,
		Name:        uriTemplate,
		Description: "test resource template " + uriTemplate,
	}
}

func noopResourceTemplateHandler(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	return &mcp.ReadResourceResult{}, nil
}

func newTestResourceTemplateOnChanged(updates chan<- resourceTemplatePollUpdate) ResourceTemplatesChangedFunc {
	return func(serverName string, templates []*mcp.ResourceTemplate) {
		updates <- resourceTemplatePollUpdate{serverName: serverName, templates: templates}
	}
}

// waitForResourceTemplatePoll blocks until an onChanged callback reports
// exactly `want` (order-independent), ignoring any intermediate updates that
// don't yet match (e.g. duplicate or superseded notifications).
func waitForResourceTemplatePoll(t *testing.T, updates <-chan resourceTemplatePollUpdate, want []string) {
	t.Helper()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case u := <-updates:
			got := resourceTemplateURIs(u.templates)
			if sameElements(got, want) {
				return
			}
			t.Logf("intermediate poll update: %v (want %v)", got, want)
		case <-deadline:
			t.Fatalf("timed out waiting for poll update: want %v", want)
		}
	}
}

func resourceTemplateURIs(templates []*mcp.ResourceTemplate) []string {
	uris := make([]string, len(templates))
	for i, tmpl := range templates {
		uris[i] = tmpl.URITemplate
	}
	return uris
}

func TestResourceTemplatePollerDetectsAddedTemplate(t *testing.T) {
	backend, httpServer := newPollerTestBackend()
	defer httpServer.Close()

	attrs := &discover_mcp_servers.ServerAttributes{ServerName: "test-backend", ServerUrl: httpServer.URL}

	updates := make(chan resourceTemplatePollUpdate, 10)
	poller := NewResourceTemplatePoller(1, newTestResourceTemplateOnChanged(updates))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller.Start(ctx, attrs)
	defer poller.Stop(attrs.ServerName)

	// Baseline: the backend starts out with no resource templates.
	waitForResourceTemplatePoll(t, updates, nil)

	backend.AddResourceTemplate(newTestResourceTemplate("file:///{path}"), noopResourceTemplateHandler)
	waitForResourceTemplatePoll(t, updates, []string{"file:///{path}"})
}

func TestResourceTemplatePollerDetectsRemovedTemplate(t *testing.T) {
	backend, httpServer := newPollerTestBackend()
	defer httpServer.Close()

	backend.AddResourceTemplate(newTestResourceTemplate("file:///{path}"), noopResourceTemplateHandler)
	backend.AddResourceTemplate(newTestResourceTemplate("git:///{ref}"), noopResourceTemplateHandler)

	attrs := &discover_mcp_servers.ServerAttributes{ServerName: "test-backend", ServerUrl: httpServer.URL}

	updates := make(chan resourceTemplatePollUpdate, 10)
	poller := NewResourceTemplatePoller(1, newTestResourceTemplateOnChanged(updates))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller.Start(ctx, attrs)
	defer poller.Stop(attrs.ServerName)

	waitForResourceTemplatePoll(t, updates, []string{"file:///{path}", "git:///{ref}"})

	backend.RemoveResourceTemplates("file:///{path}")
	waitForResourceTemplatePoll(t, updates, []string{"git:///{ref}"})
}

// TestResourceTemplatePollerDetectsModifiedTemplate proves that
// fingerprinting isn't URI-template-only: the URI template stays the same
// across the change, so a callback only fires if the description change is
// actually detected.
func TestResourceTemplatePollerDetectsModifiedTemplate(t *testing.T) {
	backend, httpServer := newPollerTestBackend()
	defer httpServer.Close()

	backend.AddResourceTemplate(newTestResourceTemplate("file:///{path}"), noopResourceTemplateHandler)

	attrs := &discover_mcp_servers.ServerAttributes{ServerName: "test-backend", ServerUrl: httpServer.URL}

	updates := make(chan resourceTemplatePollUpdate, 10)
	poller := NewResourceTemplatePoller(1, newTestResourceTemplateOnChanged(updates))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller.Start(ctx, attrs)
	defer poller.Stop(attrs.ServerName)

	waitForResourceTemplatePoll(t, updates, []string{"file:///{path}"})

	// The SDK has no in-place update, so simulate a modified template via
	// remove+add of the same URI template with a different description.
	backend.RemoveResourceTemplates("file:///{path}")
	backend.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "file:///{path}",
		Name:        "file:///{path}",
		Description: "an updated description",
	}, noopResourceTemplateHandler)

	deadline := time.After(5 * time.Second)
	for {
		select {
		case u := <-updates:
			if len(u.templates) == 1 && u.templates[0].Description == "an updated description" {
				return
			}
			t.Logf("intermediate poll update: %+v", u.templates)
		case <-deadline:
			t.Fatalf("timed out waiting for modified-template update to be detected")
		}
	}
}

func TestResourceTemplatePollerNoCallbackWhenUnchanged(t *testing.T) {
	backend, httpServer := newPollerTestBackend()
	defer httpServer.Close()

	backend.AddResourceTemplate(newTestResourceTemplate("file:///{path}"), noopResourceTemplateHandler)

	attrs := &discover_mcp_servers.ServerAttributes{ServerName: "test-backend", ServerUrl: httpServer.URL}

	updates := make(chan resourceTemplatePollUpdate, 10)
	poller := NewResourceTemplatePoller(1, newTestResourceTemplateOnChanged(updates))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller.Start(ctx, attrs)
	defer poller.Stop(attrs.ServerName)

	waitForResourceTemplatePoll(t, updates, []string{"file:///{path}"})

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
		t.Fatalf("expected no further callbacks once resource templates stabilized, got %d", count)
	}
}

func TestResourceTemplatePollerStopHaltsPolling(t *testing.T) {
	backend, httpServer := newPollerTestBackend()
	defer httpServer.Close()

	attrs := &discover_mcp_servers.ServerAttributes{ServerName: "test-backend", ServerUrl: httpServer.URL}

	updates := make(chan resourceTemplatePollUpdate, 10)
	poller := NewResourceTemplatePoller(1, newTestResourceTemplateOnChanged(updates))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller.Start(ctx, attrs)

	waitForResourceTemplatePoll(t, updates, nil)

	poller.Stop(attrs.ServerName)

	backend.AddResourceTemplate(newTestResourceTemplate("file:///{path}"), noopResourceTemplateHandler)

	select {
	case u := <-updates:
		t.Fatalf("expected no callback after Stop, got %+v", u)
	case <-time.After(3 * time.Second):
	}
}
