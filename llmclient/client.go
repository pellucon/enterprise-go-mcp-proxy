// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Package llmclient calls an OpenAI-compatible chat completions endpoint:
// send a single-turn text prompt, get back the model's text response. Only
// the request/response *shape* is standardized (not the server), so the
// same code works against Gemini's OpenAI-compatible endpoint, real
// OpenAI, or any other OpenAI-compatible provider by varying Config.
//
// A separate library package so any package main CLI can reuse the same
// calling mechanism without duplicating it — currently used by
// ai-mcp-servers/ai-powered-tools (see CLAUDE.md's "AI-powered
// tool discovery" section).
package llmclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Config configures a single ChatCompletion call.
type Config struct {
	// BaseURL is everything before "/chat/completions", e.g.
	// "https://generativelanguage.googleapis.com/v1beta/openai" or
	// "https://api.openai.com/v1".
	BaseURL string
	Model   string
	APIKey  string
	// Timeout bounds how long a single call may take. Zero means no
	// timeout is applied beyond the context's own deadline, if any.
	Timeout time.Duration
	// JSONMode, when true, requests the standard OpenAI
	// "response_format": {"type": "json_object"} mode, constraining the
	// model to emit syntactically valid JSON. Not every OpenAI-compatible
	// provider honors this field (an unrecognized field is typically just
	// ignored, not an error), so it reduces — but does not guarantee
	// against — a malformed-JSON response; callers that parse the result as
	// JSON should still handle a parse failure.
	JSONMode bool
	// System, if set, is sent as a system message ahead of the prompt -
	// where a caller puts its instructions, so that data it passes in the
	// prompt can't pose as instructions as easily.
	System string
}

// chatCompletionRequest/chatMessage mirror the standard OpenAI Chat
// Completions API's request body shape (a small subset: a single-turn,
// text-only prompt — the real API supports multi-turn history, images,
// tool calls, streaming, etc., all omitted here for brevity).
type chatCompletionRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

// responseFormat mirrors the standard OpenAI Chat Completions API's
// "response_format" request field.
type responseFormat struct {
	Type string `json:"type"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatCompletionResponse mirrors the subset of the OpenAI Chat Completions
// response shape this package cares about: the first choice's message text.
type chatCompletionResponse struct {
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
}

// ChatCompletion sends prompt as a single-turn user message to cfg.Model,
// via the standard OpenAI Chat Completions endpoint shape at
// cfg.BaseURL + "/chat/completions", and returns the first choice's
// message content.
func ChatCompletion(ctx context.Context, cfg Config, prompt string) (string, error) {
	reqBody := chatCompletionRequest{
		Model: cfg.Model,
		Messages: []chatMessage{
			{Role: "user", Content: prompt},
		},
	}
	if cfg.System != "" {
		reqBody.Messages = append([]chatMessage{{Role: "system", Content: cfg.System}}, reqBody.Messages...)
	}
	if cfg.JSONMode {
		reqBody.ResponseFormat = &responseFormat{Type: "json_object"}
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request body: %w", err)
	}

	url := strings.TrimSuffix(cfg.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// The standard OpenAI-style auth header — this (not x-goog-api-key,
	// which Gemini's native API uses) is what makes the request
	// OpenAI-compatible, and the same Gemini API key works here too.
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	client := &http.Client{Timeout: cfg.Timeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request to %s failed: %w", url, err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("request to %s returned status %s: %s", url, resp.Status, string(respBytes))
	}

	var result chatCompletionResponse
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return "", fmt.Errorf("failed to parse response json: %w (raw: %s)", err, string(respBytes))
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("response had no choices (raw: %s)", string(respBytes))
	}
	if result.Choices[0].Message.Content == "" {
		return "", fmt.Errorf("response choice had no message content (raw: %s)", string(respBytes))
	}
	return result.Choices[0].Message.Content, nil
}
