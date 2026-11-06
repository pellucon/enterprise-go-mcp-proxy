// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package dynamic_proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pellucon/enterprise-go-mcp-proxy/discover_mcp_servers"
)

// TestFetchersFollowPagination checks that every feature poller's fetch
// returns a backend's whole list, not just the first page: the backend
// serves 5 of each kind in pages of 2, so a single list call would see 2.
func TestFetchersFollowPagination(t *testing.T) {
	const n = 5
	backend := mcp.NewServer(&mcp.Implementation{Name: "paging-backend", Version: "1.0.0"}, &mcp.ServerOptions{PageSize: 2})
	for i := 0; i < n; i++ {
		mcp.AddTool(backend, newTestTool(fmt.Sprintf("tool_%d", i)), noopToolHandler)
		backend.AddPrompt(&mcp.Prompt{Name: fmt.Sprintf("prompt_%d", i)}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{}, nil
		})
		backend.AddResource(&mcp.Resource{Name: fmt.Sprintf("res_%d", i), URI: fmt.Sprintf("test://res/%d", i)}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{}, nil
		})
		backend.AddResourceTemplate(&mcp.ResourceTemplate{Name: fmt.Sprintf("tmpl_%d", i), URITemplate: fmt.Sprintf("test://tmpl%d/{id}", i)}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{}, nil
		})
	}
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backend }, nil))
	defer httpServer.Close()

	attrs := &discover_mcp_servers.ServerAttributes{ServerName: "paging-backend", ServerUrl: httpServer.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tools, err := fetchTools(ctx, attrs)
	check(t, "tools", len(tools), err, n)
	prompts, err := fetchPrompts(ctx, attrs)
	check(t, "prompts", len(prompts), err, n)
	resources, err := fetchResources(ctx, attrs)
	check(t, "resources", len(resources), err, n)
	templates, err := fetchResourceTemplates(ctx, attrs)
	check(t, "resource templates", len(templates), err, n)
}

func check(t *testing.T, kind string, got int, err error, want int) {
	t.Helper()
	if err != nil {
		t.Fatalf("fetch %s: %v", kind, err)
	}
	if got != want {
		t.Errorf("fetched %d %s, want all %d (pagination not followed?)", got, kind, want)
	}
}

// TestCollectAllPagesCapsItems checks that a backend listing more than
// maxItemsPerServer items is refused rather than held in memory.
func TestCollectAllPagesCapsItems(t *testing.T) {
	endless := func(yield func(int, error) bool) {
		for i := 0; ; i++ {
			if !yield(i, nil) {
				return
			}
		}
	}
	if _, err := collectAllPages[int](endless); err == nil {
		t.Fatal("an endless list was accepted")
	}
	exact := func(yield func(int, error) bool) {
		for i := 0; i < maxItemsPerServer; i++ {
			if !yield(i, nil) {
				return
			}
		}
	}
	if items, err := collectAllPages[int](exact); err != nil || len(items) != maxItemsPerServer {
		t.Fatalf("a list of exactly the cap: %d items, %v", len(items), err)
	}
}
