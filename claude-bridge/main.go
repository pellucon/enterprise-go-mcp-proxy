// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// claude-bridge is a small HTTP server, run directly on the developer's
// machine (never in k8s, never containerized), that wraps
// llmclient.ClaudeCodePrompt so a pod running in a local Colima/k3s cluster
// can reach a real, authenticated Claude Code session without a Linux
// install or copying credentials into the cluster — see
// llmclient.ClaudeCodePrompt's own doc comment for the underlying mechanism
// and its "development-time only" framing, and CLAUDE.md's
// "AI-powered tool discovery" section for how scripts/deploy.sh
// starts/stops this process and how ai-powered-tools calls it.
//
// Started/stopped exclusively by scripts/deploy.sh and
// scripts/cleanup-k8s-cluster.sh — never run this manually alongside a real
// deployment.
//
// Security note: this binds to all interfaces (not just localhost), since
// a pod inside the cluster needs to reach it via host.docker.internal — but
// that address was empirically confirmed (against this repo's actual local
// Colima/k3s setup) to also be reachable via the host's real LAN IP, not
// just from the cluster's own VM. The X-Claude-Bridge-Token check exists
// specifically to stop arbitrary devices on the same network from calling
// this, not to defend against a network eavesdropper — there's no TLS, so
// the token itself is visible to anyone who can already sniff the LAN
// segment. An accepted limitation for a personal dev-only tool, not a real
// security boundary.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/pellucon/enterprise-go-mcp-proxy/llmclient"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
)

// promptFunc abstracts "given a prompt, return the LLM's text response" so
// tests can inject a fake instead of requiring a real, locally-
// authenticated Claude Code install — mirrors
// ai-powered-tools/runtime.go's completionFunc pattern.
type promptFunc func(ctx context.Context, system, prompt string) (string, error)

// newHandler builds the bridge's HTTP handler. token is the shared secret
// every /prompt caller must present; prompt is what actually answers a
// request (llmclient.ClaudeCodePrompt in main(), a fake in tests).
// maxRequestBytes caps a /prompt request body; maxConcurrentPrompts caps
// how many claude processes run at once (each is a whole Claude Code
// session). Callers beyond it wait for a slot, or until they give up.
const (
	maxRequestBytes      = 4 << 20
	maxConcurrentPrompts = 4
)

func newHandler(token string, prompt promptFunc) http.Handler {
	mux := http.NewServeMux()
	slots := make(chan struct{}, maxConcurrentPrompts)

	// Unauthenticated on purpose — used only by scripts/deploy.sh's
	// readiness poll after starting the process. Never touches the token
	// check or spawns a claude CLI call, so polling it can't be mistaken
	// for "auth works" the way polling /prompt with a throwaway request
	// would be.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("POST /prompt", func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Claude-Bridge-Token")
		// subtle.ConstantTimeCompare over a plain == removes any timing
		// side-channel on the comparison itself — cheap, even though the
		// realistic threat model here (LAN exposure, not a targeted
		// attacker measuring response times) doesn't strictly require it.
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var req struct {
			Prompt string `json:"prompt"`
			// System, optional, is passed as an addition to the system
			// prompt (see llmclient.ClaudeCodePromptWithSystem).
			System string `json:"system,omitempty"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes)).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
			return
		}

		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-r.Context().Done():
			return
		}

		// r.Context() is cancelled if the caller (ai-powered-tools)
		// disconnects or its own request times out — ClaudeCodePrompt's use
		// of exec.CommandContext means that cancels the underlying `claude`
		// subprocess too, rather than leaving it running unattended.
		result, err := prompt(r.Context(), req.System, req.Prompt)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"result": result})
	})

	return mux
}

func main() {
	var port, tokenFile, logLevel string
	flag.StringVar(&port, "port", ":8099", "Port to listen on, e.g. :8099")
	flag.StringVar(&tokenFile, "token-file", "", "Path to a file containing the shared-secret token every caller must present (required — unlike ai-powered-tools' optional-file convention, this standalone process has no reason to run with no auth check at all)")
	flag.StringVar(&logLevel, "log-level", "info", "Log severity: error, info, or debug. Set once at startup — this process has no live-reload mechanism.")
	flag.Parse()
	logging.Init(logLevel)

	if tokenFile == "" {
		logging.Fatal("-token-file is required")
	}
	tokenBytes, err := os.ReadFile(tokenFile)
	if err != nil {
		logging.Fatalf("failed to read -token-file %s: %v", tokenFile, err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		logging.Fatalf("-token-file %s is empty", tokenFile)
	}
	// Never log the token itself — only which file it came from.
	logging.Infof("startup configuration: tokenFile=%s", tokenFile)

	handler := newHandler(token, llmclient.ClaudeCodePromptWithSystem)

	logging.Debugf("Starting claude-bridge on port %s", port)
	if err := http.ListenAndServe(port, handler); err != nil {
		logging.Fatal(err)
	}
}
