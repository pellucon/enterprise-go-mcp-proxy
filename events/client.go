// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package events

import (
	"encoding/json"
	"fmt"
	"sync"
	"unicode/utf8"

	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
	"github.com/pellucon/enterprise-go-mcp-proxy/natsclient"

	"github.com/nats-io/nats.go"
)

// Client is a thin wrapper around a NATS connection plus a Dictionary. Any
// Client may both publish and subscribe — there is no per-client permission
// model.
//
// A caller must SetDictionary before any Publish/Subscribe call; every real
// caller in this repo wires SetDictionary in as a configsync.Watcher[Dictionary]'s
// onChange, which fires once synchronously during construction — see the 3
// real callers' main.go — so the dictionary is already loaded by the time
// NewWatcher returns. Publish/Subscribe fail closed (a clear error, never a
// nil dereference) if called before any dictionary has ever been set.
type Client struct {
	conn *nats.Conn

	dictMu     sync.RWMutex
	dictionary *Dictionary
}

// NewClient connects to the NATS server at natsURL. An empty natsURL
// returns (nil, nil), mirroring configsync.Connect's exact convention -
// callers are expected to nil-check the result the same way they'd
// nil-check any other optional capability in this codebase (auth,
// internalAuth, orchestration) before calling Publish/Subscribe/
// SetDictionary/Close on it; a nil *Client is not itself safe to call a
// method on, it's the "NATS isn't configured, skip this whole feature"
// signal at the caller.
//
// RetryOnFailedConnect(true) is deliberate: in k8s, a backend's own pod can
// easily start before NATS has finished rolling out (or even before its
// own pod's networking/DNS is fully ready), and without this option
// nats.Connect fails outright on that first attempt with no retry at all -
// which is indistinguishable from a real, permanent NATS outage. With it,
// NewClient succeeds immediately and the connection retries in the
// background (up to nats.DefaultMaxReconnect attempts, nats.DefaultReconnectWait
// apart - about 2 minutes by default), which comfortably covers a normal
// startup race without requiring exact deploy-order sequencing to be
// perfect. A genuinely unreachable NATS (wrong URL, real outage lasting
// longer than the retry budget) still surfaces - just via a later
// Publish/Subscribe call failing, or via the DisconnectErrHandler/
// ReconnectHandler log lines below, rather than at startup.
func NewClient(natsURL string) (*Client, error) {
	if natsURL == "" {
		return nil, nil
	}
	authOpts, err := natsclient.Options()
	if err != nil {
		return nil, err
	}
	conn, err := nats.Connect(natsURL, append([]nats.Option{
		nats.RetryOnFailedConnect(true),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				logging.Errorf("events: disconnected from NATS at %s: %v", natsURL, err)
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			logging.Debugf("events: (re)connected to NATS at %s", nc.ConnectedUrl())
		}),
	}, authOpts...)...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to NATS at %s: %w", natsURL, err)
	}

	return &Client{
		conn: conn,
	}, nil
}

// SetDictionary atomically swaps the Dictionary Publish/Subscribe validates
// against. Safe for concurrent use with Publish/Subscribe.
func (c *Client) SetDictionary(d *Dictionary) {
	c.dictMu.Lock()
	c.dictionary = d
	c.dictMu.Unlock()
}

func (c *Client) getDictionary() (*Dictionary, error) {
	c.dictMu.RLock()
	defer c.dictMu.RUnlock()
	if c.dictionary == nil {
		return nil, fmt.Errorf("events: client has no dictionary loaded yet")
	}
	return c.dictionary, nil
}

// Publish validates e against the current Dictionary and publishes it to
// e.Subject(). Fails closed if no dictionary has ever been set, or e
// doesn't match the Dictionary.
func (c *Client) Publish(e *Event) error {
	dict, err := c.getDictionary()
	if err != nil {
		return err
	}
	e = capText(e)
	if err := dict.Validate(e); err != nil {
		return fmt.Errorf("event failed dictionary validation: %w", err)
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}
	return c.conn.Publish(e.Subject(), data)
}

// Subscribe subscribes to subjectFilter (a NATS subject, which may use "*"
// or ">" wildcards, e.g. "events.>" for every event) and invokes handler
// for each received Event that passes the client's Dictionary (the same
// check Publish applies) and arrived on its own subject. Any other message -
// malformed, an unknown type or subtype, missing a required key, on the
// wrong subject, or received before a dictionary is set - is logged and
// dropped rather than passed to handler or treated as a fatal error.
func (c *Client) Subscribe(subjectFilter string, handler func(*Event)) (*nats.Subscription, error) {
	return c.conn.Subscribe(subjectFilter, func(msg *nats.Msg) {
		var e Event
		if err := json.Unmarshal(msg.Data, &e); err != nil {
			logging.Errorf("events: dropping malformed message on subject %s: %v", msg.Subject, err)
			return
		}
		// The same check Publish applies, so an event that didn't come
		// from a validating publisher (NATS permissions should already
		// stop anyone else publishing) is never handed on.
		dict, err := c.getDictionary()
		if err == nil {
			err = dict.Validate(&e)
		}
		if err == nil && msg.Subject != e.Subject() {
			err = fmt.Errorf("event says it's %s", e.Subject())
		}
		if err != nil {
			logging.Errorf("events: dropping invalid event on subject %s: %v", msg.Subject, err)
			return
		}
		handler(&e)
	})
}

// Flush blocks until all buffered NATS protocol messages (notably a
// preceding Subscribe's SUB command) have been round-tripped to the
// server, so a subsequent Publish from a different connection is
// guaranteed to be seen by that subscription. Core NATS is fire-and-forget
// with no queuing for a not-yet-registered subscriber, so callers that
// Subscribe and then immediately expect to receive a Publish sent from
// elsewhere should Flush in between to avoid a race.
func (c *Client) Flush() error {
	return c.conn.Flush()
}

// Close closes the underlying NATS connection.
func (c *Client) Close() {
	c.conn.Close()
}

// IsConnected reports whether the underlying NATS connection is currently
// up, for a caller's own /healthz endpoint to gate k8s readiness on an
// actual NATS connection rather than just process liveness.
func (c *Client) IsConnected() bool {
	return c.conn.IsConnected()
}

// maxTextBytes caps an event's title, text and string metadata values.
// Some carry text a backend controls (a failed call's error, a guardrail's
// reason), and NATS refuses a message over its max_payload (1 MB): an
// oversized one would make the event, and its audit record, vanish.
// Request and result payloads are capped separately, by go-proxy's
// auditPayload.
const maxTextBytes = 4 << 10

// capText returns e with its title, text and top-level string metadata
// values cut to maxTextBytes (at a UTF-8 boundary, marked as truncated).
// e itself is left unchanged.
func capText(e *Event) *Event {
	capped := *e
	capped.Title = truncateText(e.Title)
	capped.Text = truncateText(e.Text)
	if e.Metadata != nil {
		capped.Metadata = make(map[string]any, len(e.Metadata))
		for k, v := range e.Metadata {
			if s, ok := v.(string); ok {
				v = truncateText(s)
			}
			capped.Metadata[k] = v
		}
	}
	return &capped
}

func truncateText(s string) string {
	if len(s) <= maxTextBytes {
		return s
	}
	cut := maxTextBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("... (truncated, %d bytes in all)", len(s))
}
