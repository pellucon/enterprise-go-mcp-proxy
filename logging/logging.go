// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Package logging is the single, shared logging mechanism for every server
// in this module (go-proxy itself, every platform-mcp-servers/* backend,
// ai-mcp-servers/ai-powered-tools, the go-gardening-demo/servers/*
// backends, demo-authorization-server, and claude-bridge). It replaces the
// previously-mixed stdlib "log", one isolated "log/slog" usage, and a
// couple of bare fmt.Printf call sites with one API and one runtime-
// adjustable severity threshold.
//
// Four message severities exist: Error, Warning, Info (reserved, unused for
// now — nothing is currently classified as Info), and Debug. The
// configurable threshold itself only ever takes one of three values —
// "error", "info", or "debug" (see Init) — Warning has no dedicated
// threshold setting of its own; it sits between Info and Error in slog's
// own ordering, so a Warning message is visible whenever the threshold is
// "info" or "debug", and suppressed when it's "error", the same as an Info
// message would be. The threshold is held in a
// package-level *slog.LevelVar, which slog.HandlerOptions.Level accepts as
// a live Leveler rather than a fixed value: the handler calls
// levelVar.Level() on every single log call, so changing it via Init/
// SetLevel takes effect immediately, on the very next call, in every
// goroutine — no logger reconstruction, no restart. See ApplyConfig
// (config.go) for how this is wired to configsync for cross-pod, no-
// restart level changes in go-proxy/platform-mcp-servers/ai-powered-tools;
// every other server just calls Init once at startup from a CLI flag.
package logging

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
)

// levelVar is the single Leveler every log call in the process is gated by.
// slog.LevelVar's Set/Level methods are documented safe for concurrent use
// (backed by an atomic.Int64 internally), so Init/SetLevel may be called
// from any goroutine — main's own startup path, an HTTP handler, or a
// configsync NATS-reload callback — with no extra locking here.
var levelVar = new(slog.LevelVar)

// std is constructed once, at package-init time: Go guarantees every
// package-level var is initialized before main() (and so before any
// goroutine can call into this package) runs, so no sync.Once/mutex is
// needed around this construction.
var std = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: levelVar}))

func normalizeLevel(level string) string { return strings.ToLower(strings.TrimSpace(level)) }

// Init sets the process's current log level ("error", "info", or "debug",
// case-insensitive; empty means "info"). Safe — and intended — to call more
// than once and from any goroutine: go-proxy calls it at startup from its
// parsed config, then again every time its log-level configsync Watcher
// fires, each call simply replacing the process-wide effective level, live.
// An unknown level string falls back to "info" and is itself reported at
// Error, so a typo'd override is visible rather than silently ignored.
func Init(level string) {
	switch normalizeLevel(level) {
	case "debug":
		levelVar.Set(slog.LevelDebug)
	case "error":
		levelVar.Set(slog.LevelError)
	case "info", "":
		levelVar.Set(slog.LevelInfo)
	default:
		levelVar.Set(slog.LevelInfo)
		Errorf("logging: unknown level %q, defaulting to info", level)
	}
}

// Errorf logs a formatted message at Error level — the highest severity,
// never suppressed regardless of the current level.
func Errorf(format string, args ...any) { std.Error(fmt.Sprintf(format, args...)) }

// Warnf logs a formatted message at Warning level — for a condition worth
// an operator's attention that isn't itself a failure (e.g. a transient,
// self-retrying error). Visible whenever the current level is "info" or
// "debug"; suppressed when it's "error". See the package doc comment.
func Warnf(format string, args ...any) { std.Warn(fmt.Sprintf(format, args...)) }

// Debugf logs a formatted message at Debug level — the most verbose,
// suppressed unless the current level is Debug.
func Debugf(format string, args ...any) { std.Debug(fmt.Sprintf(format, args...)) }

// Infof logs a formatted message at Info level. Reserved for future use —
// nothing in this pass classifies a message as Info.
func Infof(format string, args ...any) { std.Info(fmt.Sprintf(format, args...)) }

// Error logs v (formatted like fmt.Sprint) at Error level.
func Error(v ...any) { std.Error(fmt.Sprint(v...)) }

// Warn logs v (formatted like fmt.Sprint) at Warning level. See Warnf.
func Warn(v ...any) { std.Warn(fmt.Sprint(v...)) }

// Debug logs v (formatted like fmt.Sprint) at Debug level.
func Debug(v ...any) { std.Debug(fmt.Sprint(v...)) }

// Info logs v (formatted like fmt.Sprint) at Info level. Reserved for
// future use — nothing in this pass classifies a message as Info.
func Info(v ...any) { std.Info(fmt.Sprint(v...)) }

// Fatalf logs a formatted message at Error level, then exits the process.
// Only for a genuine startup-time fatal condition (config/dependency
// validation in a main()) — never for a per-request/per-backend failure.
func Fatalf(format string, args ...any) {
	std.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}

// Fatal logs v at Error level, then exits the process. See Fatalf.
func Fatal(v ...any) {
	std.Error(fmt.Sprint(v...))
	os.Exit(1)
}

// CurrentLevel returns the process's current log level as "error", "info",
// or "debug" — the inverse of Init. Mainly for tests proving a level change
// actually took effect; also usable by any future introspection endpoint.
func CurrentLevel() string {
	switch levelVar.Level() {
	case slog.LevelDebug:
		return "debug"
	case slog.LevelError:
		return "error"
	default:
		return "info"
	}
}

// Size describes v for a log line by its encoded size only, never its
// content: tool arguments and results can hold personal data or secrets,
// and the debug level that logs them can be switched on at runtime
// (_admin.update_log_level), cluster-wide.
func Size(v any) string {
	var n int
	switch b := v.(type) {
	case nil:
		return "none"
	case []byte:
		n = len(b)
	case string:
		n = len(b)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return "unencodable"
		}
		if string(raw) == "null" {
			return "none"
		}
		n = len(raw)
	}
	return fmt.Sprintf("%d bytes", n)
}

// URL returns rawURL with any password replaced by "xxxxx", for logging a
// URL that may embed credentials.
func URL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "unparseable URL"
	}
	return u.Redacted()
}
