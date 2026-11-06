// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package dynamic_proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pellucon/enterprise-go-mcp-proxy/discover_mcp_servers"
)

type progressArgs struct {
	Who string `json:"who"`
}

// startProgressGateway runs a backend whose "work" tool reports progress 3
// times before returning, and a gateway in front of it whose backend
// connection - one session shared by every client, as in go-proxy - relays
// that progress. It returns the gateway's URL.
func startProgressGateway(t *testing.T) (string, *Gateway) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	backend := mcp.NewServer(&mcp.Implementation{Name: "progress-backend", Version: "1.0.0"}, nil)
	mcp.AddTool(backend, &mcp.Tool{Name: "work", Description: "reports progress"}, func(ctx context.Context, req *mcp.CallToolRequest, args progressArgs) (*mcp.CallToolResult, any, error) {
		if token := req.Params.GetProgressToken(); token != nil {
			for i := 1; i <= 3; i++ {
				_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
					ProgressToken: token, Progress: float64(i), Total: 3, Message: fmt.Sprintf("%s-%d", args.Who, i),
				})
				time.Sleep(10 * time.Millisecond)
			}
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done " + args.Who}}}, nil, nil
	})
	backendHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backend }, nil))
	t.Cleanup(backendHTTP.Close)

	var gateway *Gateway
	var sessMu sync.Mutex
	var shared *mcp.ClientSession
	gateway = NewGateway(func(_ context.Context, _ string, server *discover_mcp_servers.ServerAttributes) (*mcp.ClientSession, error) {
		sessMu.Lock()
		defer sessMu.Unlock()
		if shared != nil {
			return shared, nil
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "gw-backend-client", Version: "1.0.0"}, &mcp.ClientOptions{ProgressNotificationHandler: gateway.HandleBackendProgress})
		s, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.ServerUrl}, nil)
		if err == nil {
			shared = s
		}
		return s, err
	}, nil, 1, 1, 1, 1)
	// Close the shared backend session before the backend server shuts down
	// (cleanups run last-in-first-out); an open session would keep
	// backendHTTP.Close waiting forever.
	t.Cleanup(func() {
		sessMu.Lock()
		defer sessMu.Unlock()
		if shared != nil {
			shared.Close()
		}
	})
	servers := discover_mcp_servers.ServerMap{"pb": {ServerName: "pb", ServerUrl: backendHTTP.URL}}
	gateway.OnAdded(ctx, servers)
	t.Cleanup(func() { gateway.OnRemoved(servers) })

	gatewayHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return gateway.Server() }, &mcp.StreamableHTTPOptions{Stateless: true}))
	t.Cleanup(gatewayHTTP.Close)
	return gatewayHTTP.URL, gateway
}

// progressClient connects to url and records every progress notification.
type progressClient struct {
	session *mcp.ClientSession
	mu      sync.Mutex
	got     []*mcp.ProgressNotificationParams
}

func newProgressClient(t *testing.T, ctx context.Context, url string) *progressClient {
	t.Helper()
	pc := &progressClient{}
	client := mcp.NewClient(&mcp.Implementation{Name: "progress-test-client", Version: "1.0.0"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			pc.mu.Lock()
			pc.got = append(pc.got, req.Params)
			pc.mu.Unlock()
		},
	})
	s, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	pc.session = s
	return pc
}

func (pc *progressClient) messages() []string {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	var out []string
	for _, p := range pc.got {
		out = append(out, fmt.Sprintf("%v:%s", p.ProgressToken, p.Message))
	}
	sort.Strings(out)
	return out
}

func waitForTool(t *testing.T, ctx context.Context, s *mcp.ClientSession, name string) {
	t.Helper()
	for {
		res, err := s.ListTools(ctx, nil)
		if err == nil {
			for _, tl := range res.Tools {
				if tl.Name == name {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("tool %s never appeared", name)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func callWork(t *testing.T, ctx context.Context, s *mcp.ClientSession, who string, token any) {
	t.Helper()
	params := &mcp.CallToolParams{Name: "pb.work", Arguments: map[string]any{"who": who}}
	if token != nil {
		params.SetProgressToken(token)
	}
	if _, err := s.CallTool(ctx, params); err != nil {
		t.Errorf("call for %s: %v", who, err)
	}
}

func TestProgressIsRelayedToTheCallingClient(t *testing.T) {
	url, gateway := startProgressGateway(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pc := newProgressClient(t, ctx, url)
	waitForTool(t, ctx, pc.session, "pb.work")
	callWork(t, ctx, pc.session, "A", "1")

	want := []string{"1:A-1", "1:A-2", "1:A-3"}
	if got := pc.messages(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("progress received = %v, want %v (under the client's own token)", got, want)
	}
	if n := gateway.progress.pending(); n != 0 {
		t.Errorf("%d relay entries left after the call, want 0", n)
	}
}

func TestProgressGoesOnlyToItsOwnClient(t *testing.T) {
	url, gateway := startProgressGateway(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	a := newProgressClient(t, ctx, url)
	b := newProgressClient(t, ctx, url)
	waitForTool(t, ctx, a.session, "pb.work")

	// Both clients use the same token over one shared backend session.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); callWork(t, ctx, a.session, "A", "1") }()
	go func() { defer wg.Done(); callWork(t, ctx, b.session, "B", "1") }()
	wg.Wait()

	if got, want := a.messages(), []string{"1:A-1", "1:A-2", "1:A-3"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("client A received %v, want only its own %v", got, want)
	}
	if got, want := b.messages(), []string{"1:B-1", "1:B-2", "1:B-3"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("client B received %v, want only its own %v", got, want)
	}
	if n := gateway.progress.pending(); n != 0 {
		t.Errorf("%d relay entries left after the calls, want 0", n)
	}
}

func TestNoProgressWithoutAToken(t *testing.T) {
	url, gateway := startProgressGateway(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pc := newProgressClient(t, ctx, url)
	waitForTool(t, ctx, pc.session, "pb.work")
	callWork(t, ctx, pc.session, "A", nil)

	if got := pc.messages(); len(got) != 0 {
		t.Errorf("received %v without asking for progress, want nothing", got)
	}
	if n := gateway.progress.pending(); n != 0 {
		t.Errorf("%d relay entries left, want 0", n)
	}
}

// TestProgressWithoutMessages checks WithoutProgressMessages: the progress
// itself still reaches the client, but not the backend's message text.
func TestProgressWithoutMessages(t *testing.T) {
	url, gateway := startProgressGateway(t)
	gateway.Server().AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			return next(WithoutProgressMessages(ctx), method, req)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pc := newProgressClient(t, ctx, url)
	waitForTool(t, ctx, pc.session, "pb.work")
	callWork(t, ctx, pc.session, "A", "1")

	want := []string{"1:", "1:", "1:"}
	if got := pc.messages(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("progress received = %v, want %v (three updates, no message text)", got, want)
	}
}
