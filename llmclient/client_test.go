// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package llmclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newFakeServer(t *testing.T, lastRequestBody *[]byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		*lastRequestBody = body

		resp := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestChatCompletionOmitsResponseFormatByDefault(t *testing.T) {
	var lastRequestBody []byte
	server := newFakeServer(t, &lastRequestBody)

	if _, err := ChatCompletion(context.Background(), Config{BaseURL: server.URL, Model: "test-model", APIKey: "key"}, "prompt"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var req map[string]any
	if err := json.Unmarshal(lastRequestBody, &req); err != nil {
		t.Fatalf("failed to parse request body: %v", err)
	}
	if _, ok := req["response_format"]; ok {
		t.Errorf("expected no response_format field when JSONMode is false, got: %s", lastRequestBody)
	}
}

func TestChatCompletionSetsJSONObjectResponseFormatWhenJSONModeEnabled(t *testing.T) {
	var lastRequestBody []byte
	server := newFakeServer(t, &lastRequestBody)

	if _, err := ChatCompletion(context.Background(), Config{BaseURL: server.URL, Model: "test-model", APIKey: "key", JSONMode: true}, "prompt"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var req struct {
		ResponseFormat struct {
			Type string `json:"type"`
		} `json:"response_format"`
	}
	if err := json.Unmarshal(lastRequestBody, &req); err != nil {
		t.Fatalf("failed to parse request body: %v", err)
	}
	if req.ResponseFormat.Type != "json_object" {
		t.Errorf("expected response_format.type to be \"json_object\", got: %s", lastRequestBody)
	}
}

func TestChatCompletionSendsSystemMessageFirst(t *testing.T) {
	var lastRequestBody []byte
	server := newFakeServer(t, &lastRequestBody)

	if _, err := ChatCompletion(context.Background(), Config{BaseURL: server.URL, Model: "test-model", APIKey: "key", System: "the rules"}, "the data"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var req struct {
		Messages []struct{ Role, Content string } `json:"messages"`
	}
	if err := json.Unmarshal(lastRequestBody, &req); err != nil {
		t.Fatalf("failed to parse request body: %v", err)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[0].Content != "the rules" || req.Messages[1].Role != "user" || req.Messages[1].Content != "the data" {
		t.Errorf("messages = %+v; want a system message then the user prompt", req.Messages)
	}
}
