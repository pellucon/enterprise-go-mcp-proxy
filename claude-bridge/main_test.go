// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fakePrompt(result string, err error) promptFunc {
	return func(ctx context.Context, system, prompt string) (string, error) {
		return result, err
	}
}

func doPromptRequest(t *testing.T, handler http.Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/prompt", strings.NewReader(body))
	if token != "" {
		req.Header.Set("X-Claude-Bridge-Token", token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestHandlerPromptSuccessWithCorrectToken(t *testing.T) {
	handler := newHandler("secret-token", fakePrompt("hello back", nil))

	rec := doPromptRequest(t, handler, "secret-token", `{"prompt":"hi"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Result != "hello back" {
		t.Errorf("expected result %q, got %q", "hello back", resp.Result)
	}
}

func TestHandlerPromptRejectsMissingToken(t *testing.T) {
	handler := newHandler("secret-token", fakePrompt("should not be reached", nil))

	rec := doPromptRequest(t, handler, "", `{"prompt":"hi"}`)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing token, got %d", rec.Code)
	}
}

func TestHandlerPromptRejectsWrongToken(t *testing.T) {
	handler := newHandler("secret-token", fakePrompt("should not be reached", nil))

	rec := doPromptRequest(t, handler, "wrong-token", `{"prompt":"hi"}`)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for wrong token, got %d", rec.Code)
	}
}

func TestHandlerPromptPropagatesPromptFuncError(t *testing.T) {
	handler := newHandler("secret-token", fakePrompt("", errors.New("claude CLI not found")))

	rec := doPromptRequest(t, handler, "secret-token", `{"prompt":"hi"}`)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 when promptFunc fails, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "claude CLI not found") {
		t.Errorf("expected error body to mention the underlying error, got: %s", rec.Body.String())
	}
}

func TestHandlerPromptRejectsInvalidBody(t *testing.T) {
	handler := newHandler("secret-token", fakePrompt("should not be reached", nil))

	rec := doPromptRequest(t, handler, "secret-token", `not json`)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON body, got %d", rec.Code)
	}
}

func TestHandlerHealthzIsUnauthenticated(t *testing.T) {
	handler := newHandler("secret-token", fakePrompt("should not be reached", nil))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 from /healthz with no token, got %d", rec.Code)
	}
}

func TestHandlerPassesSystemPrompt(t *testing.T) {
	var gotSystem, gotPrompt string
	handler := newHandler("secret-token", func(ctx context.Context, system, prompt string) (string, error) {
		gotSystem, gotPrompt = system, prompt
		return "ok", nil
	})
	if rec := doPromptRequest(t, handler, "secret-token", `{"prompt":"the data","system":"the rules"}`); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotSystem != "the rules" || gotPrompt != "the data" {
		t.Errorf("got system=%q prompt=%q", gotSystem, gotPrompt)
	}
}

func TestHandlerRejectsOversizedBody(t *testing.T) {
	h := newHandler("tok", func(context.Context, string, string) (string, error) { return "ok", nil })
	body := `{"prompt":"` + strings.Repeat("a", maxRequestBytes) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/prompt", strings.NewReader(body))
	req.Header.Set("X-Claude-Bridge-Token", "tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 for a body over %d bytes", rec.Code, maxRequestBytes)
	}
}

func TestHandlerLimitsConcurrentPrompts(t *testing.T) {
	var running, peak atomic.Int32
	release := make(chan struct{})
	h := newHandler("tok", func(context.Context, string, string) (string, error) {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		running.Add(-1)
		return "ok", nil
	})
	var wg sync.WaitGroup
	for i := 0; i < maxConcurrentPrompts+3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/prompt", strings.NewReader(`{"prompt":"p"}`))
			req.Header.Set("X-Claude-Bridge-Token", "tok")
			h.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if p := peak.Load(); p > maxConcurrentPrompts {
		t.Errorf("%d prompts ran at once; want at most %d", p, maxConcurrentPrompts)
	}
}
