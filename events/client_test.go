// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package events

import (
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

const testDictionaryJSON = `{
  "eventTypes": {
    "alert": {
      "subtypes": ["tool-call-rejected", "result-rejected"],
      "metadata": {
        "tool":     { "type": "string", "required": true },
        "reason":   { "type": "string", "required": true },
        "severity": { "type": "string", "required": false, "allowedValues": ["warning", "error"] }
      }
    },
    "other-event": {
      "metadata": {
        "tool": { "type": "string", "required": true }
      }
    }
  }
}`

// startTestNATSServer starts an embedded, in-process NATS server on a
// random free port for the duration of the test - no real NATS binary or
// k8s needed, mirroring dynamic_proxy's httptest.Server-on-random-port
// convention.
func startTestNATSServer(t *testing.T) string {
	t.Helper()
	opts := &natsserver.Options{
		Host:   "127.0.0.1",
		Port:   -1,
		NoLog:  true,
		NoSigs: true,
	}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("failed to create embedded NATS server: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(4 * time.Second) {
		t.Fatal("embedded NATS server not ready in time")
	}
	t.Cleanup(ns.Shutdown)
	return ns.ClientURL()
}

// newTestClient builds a Client connected to url and immediately sets its
// dictionary from testDictionaryJSON, standing in for what a
// configsync.Watcher[Dictionary]'s onChange normally does in a real server
// (see the 3 real callers' main.go) — this package doesn't depend on
// configsync, so tests exercise SetDictionary directly instead.
func newTestClient(t *testing.T, url string) *Client {
	t.Helper()
	c, err := NewClient(url)
	if err != nil {
		t.Fatalf("failed to build client: %v", err)
	}
	dict, err := ParseDictionary([]byte(testDictionaryJSON))
	if err != nil {
		t.Fatalf("failed to parse test dictionary: %v", err)
	}
	c.SetDictionary(dict)
	return c
}

func TestPublishSubscribeRoundTrip(t *testing.T) {
	url := startTestNATSServer(t)

	publisher := newTestClient(t, url)
	defer publisher.Close()

	subscriber := newTestClient(t, url)
	defer subscriber.Close()

	received := make(chan *Event, 1)
	if _, err := subscriber.Subscribe("events.>", func(e *Event) {
		received <- e
	}); err != nil {
		t.Fatalf("failed to subscribe: %v", err)
	}
	// Ensure the subscription has round-tripped to the server before
	// publishing from a different connection - core NATS drops a publish
	// with no registered subscriber yet, it doesn't queue it.
	if err := subscriber.Flush(); err != nil {
		t.Fatalf("failed to flush subscriber: %v", err)
	}

	want := &Event{
		Type:     "alert",
		Subtype:  "tool-call-rejected",
		Time:     time.Now().UTC(),
		Title:    "Tool call rejected",
		Text:     "call-authorization rejected the call",
		Metadata: map[string]any{"tool": "bad_tool", "reason": "name contains \"bad\""},
	}
	if err := publisher.Publish(want); err != nil {
		t.Fatalf("failed to publish: %v", err)
	}

	select {
	case got := <-received:
		if got.Type != want.Type || got.Subtype != want.Subtype || got.Title != want.Title {
			t.Errorf("received event doesn't match published event: got %+v, want %+v", got, want)
		}
		if got.Metadata["tool"] != "bad_tool" {
			t.Errorf("expected metadata tool=bad_tool, got %v", got.Metadata["tool"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for published event to be received")
	}
}

func TestNewClientEmptyURLIsNoOp(t *testing.T) {
	client, err := NewClient("")
	if err != nil {
		t.Fatalf("expected no error for an empty natsURL, got: %v", err)
	}
	if client != nil {
		t.Fatalf("expected NewClient(\"\") to return a nil *Client, got %+v", client)
	}
}

func TestPublishFailsClosedWithNoDictionaryLoaded(t *testing.T) {
	url := startTestNATSServer(t)
	client, err := NewClient(url)
	if err != nil {
		t.Fatalf("failed to build client: %v", err)
	}
	defer client.Close()

	err = client.Publish(&Event{Type: "alert", Subtype: "tool-call-rejected", Time: time.Now().UTC(), Title: "t", Text: "t", Metadata: map[string]any{"tool": "x", "reason": "x"}})
	if err == nil {
		t.Error("expected Publish to fail closed before any dictionary has ever been set")
	}
}

func TestPublishRejectsUnknownEventType(t *testing.T) {
	url := startTestNATSServer(t)
	client := newTestClient(t, url)
	defer client.Close()

	err := client.Publish(&Event{Type: "not-in-dictionary", Time: time.Now().UTC(), Title: "t", Text: "t"})
	if err == nil {
		t.Error("expected Publish to reject an event type absent from the dictionary")
	}
}

func TestPublishRejectsMissingRequiredMetadata(t *testing.T) {
	url := startTestNATSServer(t)
	client := newTestClient(t, url)
	defer client.Close()

	// "reason" is required by the dictionary but omitted here.
	err := client.Publish(&Event{
		Type:     "alert",
		Subtype:  "tool-call-rejected",
		Time:     time.Now().UTC(),
		Title:    "t",
		Text:     "t",
		Metadata: map[string]any{"tool": "x"},
	})
	if err == nil {
		t.Error("expected Publish to reject an event missing a required metadata key")
	}
}

func TestPublishRejectsUndeclaredSubtype(t *testing.T) {
	url := startTestNATSServer(t)
	client := newTestClient(t, url)
	defer client.Close()

	err := client.Publish(&Event{
		Type:     "alert",
		Subtype:  "not-a-declared-subtype",
		Time:     time.Now().UTC(),
		Title:    "t",
		Text:     "t",
		Metadata: map[string]any{"tool": "x", "reason": "x"},
	})
	if err == nil {
		t.Error("expected Publish to reject a subtype absent from the dictionary's declared subtypes")
	}
}

func TestPublishRejectsSubtypeOnTypeThatDoesNotUseSubtypes(t *testing.T) {
	url := startTestNATSServer(t)
	client := newTestClient(t, url)
	defer client.Close()

	// "other-event" declares no subtypes at all, so any non-empty Subtype
	// must be rejected, not silently allowed through.
	err := client.Publish(&Event{
		Type:     "other-event",
		Subtype:  "unexpected",
		Time:     time.Now().UTC(),
		Title:    "t",
		Text:     "t",
		Metadata: map[string]any{"tool": "x"},
	})
	if err == nil {
		t.Error("expected Publish to reject a subtype on an event type that declares no subtypes")
	}
}

func TestPublishRejectsValueOutsideAllowedValues(t *testing.T) {
	url := startTestNATSServer(t)
	client := newTestClient(t, url)
	defer client.Close()

	err := client.Publish(&Event{
		Type:    "alert",
		Subtype: "tool-call-rejected",
		Time:    time.Now().UTC(),
		Title:   "t",
		Text:    "t",
		Metadata: map[string]any{
			"tool":     "x",
			"reason":   "x",
			"severity": "catastrophic", // not in ["warning", "error"]
		},
	})
	if err == nil {
		t.Error("expected Publish to reject a value outside the dictionary's allowedValues")
	}
}

// TestSubscribeDropsInvalidEvents checks that Subscribe hands on only
// events that pass the dictionary and arrived on their own subject - raw
// messages that didn't go through a validating Publish are dropped.
func TestSubscribeDropsInvalidEvents(t *testing.T) {
	url := startTestNATSServer(t)
	subscriber := newTestClient(t, url)
	defer subscriber.Close()
	received := make(chan *Event, 10)
	if _, err := subscriber.Subscribe("events.>", func(e *Event) { received <- e }); err != nil {
		t.Fatal(err)
	}
	if err := subscriber.Flush(); err != nil {
		t.Fatal(err)
	}

	raw, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	valid := `{"type":"alert","subtype":"tool-call-rejected","title":"t","metadata":{"tool":"x","reason":"r"}}`
	for subject, body := range map[string]string{
		"events.alert.tool-call-rejected":  `{"type":"forged","subtype":"x","metadata":{}}`,                 // unknown type
		"events.alert.other":               valid,                                                           // valid event, wrong subject
		"events.alert.tool-call-rejected2": `{"type":"alert","subtype":"tool-call-rejected","metadata":{}}`, // missing required keys
		"events.junk":                      `not json`,
	} {
		raw.Publish(subject, []byte(body))
	}
	raw.Publish("events.alert.tool-call-rejected", []byte(valid))
	raw.Flush()

	select {
	case got := <-received:
		if got.Type != "alert" || got.Metadata["tool"] != "x" {
			t.Errorf("received %+v; want only the valid event", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the valid event wasn't delivered")
	}
	select {
	case got := <-received:
		t.Errorf("an invalid event was delivered: %+v", got)
	case <-time.After(300 * time.Millisecond):
	}
}
