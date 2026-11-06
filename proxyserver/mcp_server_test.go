// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

/*
Safer way to wrap http witout missing important methods:
import "github.com/felixge/httpsnoop"

If you absolutely must use an HTTP-layer interceptor, use a trusted, battle-tested third-party library like
github.com/felixge/httpsnoop. It uses low-level runtime reflection to automatically capture and forward every
single hidden standard library interface perfectly:

	func NotificationMiddleware(next http.Handler) http.Handler {
	    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

	        // httpsnoop hooks into 'Write' but perfectly preserves Hijacker,
	        // Flusher, CloseNotifier, Pusher, and ReaderFrom dynamically!
	        hooks := httpsnoop.Hooks{
	            Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
	                return func(b []byte) (int, error) {
	                    fmt.Println("********** Safe payload hook:", string(b))
	                    return next(b)
	                }
	            },
	        }

	        wrappedResponseWriter := httpsnoop.Wrap(w, hooks)
	        next.ServeHTTP(wrappedResponseWriter, r)
	    })
	}
*/
package proxyserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func NotificationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Use a custom response writer to catch chunks
		interceptor := &responseInterceptor{ResponseWriter: w}

		next.ServeHTTP(interceptor, r)
	})
}

func (ri *responseInterceptor) Flush() {
	// If the underlying writer supports flushing, pass it down
	if flusher, ok := ri.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

type responseInterceptor struct {
	http.ResponseWriter
}

func (ri *responseInterceptor) Write(b []byte) (int, error) {
	// Look for the raw JSON-RPC string being streamed down the pipe

	fmt.Println("********** UDI Observed raw network payload:", string(b))
	return ri.ResponseWriter.Write(b)
}

// runBackendServer serves server on port until ctx is canceled, at which
// point it gracefully shuts down and returns.
func runBackendServer(ctx context.Context, port string, server *mcp.Server) {
	log.Println("Starting the tools Server on port " + port)
	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
	srv := &http.Server{Addr: port, Handler: NotificationMiddleware(handler)}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("backend server on %s stopped with error: %v", port, err)
	}
}

// runMCPServer starts server on port in the background and returns a
// *sync.WaitGroup that's Done once the server has fully stopped listening.
// Callers that cancel ctx must Wait() on the returned WaitGroup before
// treating the server as gone.
func runMCPServer(ctx context.Context, port string, server *mcp.Server) *sync.WaitGroup {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runBackendServer(ctx, port, server)
	}()
	return &wg
}

// startTestBackend starts server on port via runMCPServer and registers a
// t.Cleanup that cancels ctx and waits for the backend to fully stop before
// the test is considered finished. Shared by every root-package test that
// spins up an in-process backend server.
func startTestBackend(t *testing.T, ctx context.Context, cancel context.CancelFunc, port string, server *mcp.Server) {
	t.Helper()
	wg := runMCPServer(ctx, port, server)
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
}

// startTestProxy starts doRunOn(ctx, cfgPath, addr) in the background,
// waits briefly for it to come up (failing the test immediately if it
// exits early), and registers a t.Cleanup that cancels ctx and waits for
// doRunOn to fully return — including its internal HTTP server and
// background monitor/k8s-sync goroutines (see runProxyServerOn) — before
// the test is considered finished.
func startTestProxy(t *testing.T, ctx context.Context, cancel context.CancelFunc, cfgPath, addr string) {
	t.Helper()
	errChan := make(chan error, 1)
	go func() {
		errChan <- doRunOn(ctx, cfgPath, addr)
	}()

	// alreadyConsumed tracks whether the select below already read the one
	// value errChan will ever receive, so the Cleanup doesn't block forever
	// trying to read it a second time.
	alreadyConsumed := false
	t.Cleanup(func() {
		cancel()
		if !alreadyConsumed {
			<-errChan
		}
	})

	select {
	case err := <-errChan:
		alreadyConsumed = true
		t.Fatalf("proxy failed to start: %v", err)
	case <-time.After(3 * time.Second):
		log.Print("Waited for the proxy to start")
	}
}
