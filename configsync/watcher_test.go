// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package configsync

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// startTestNATSServer starts an embedded, in-process NATS server on a
// random free port for the duration of the test, mirroring
// events/client_test.go's identical helper.
func startTestNATSServer(t *testing.T) string {
	t.Helper()
	opts := &natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true}
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

func parseTestConfig(data []byte) (*testConfig, error) {
	var cfg testConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func writeVersioned(t *testing.T, path string, version int64, cfg testConfig) {
	t.Helper()
	raw, err := json.Marshal(VersionedConfig[testConfig]{Version: version, Config: cfg})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestWatcher_NoPathFallsBackToInitial(t *testing.T) {
	initial := &testConfig{Rules: []string{"initial"}}
	w, err := NewWatcher[testConfig]("widgets", "", nil, initial, parseTestConfig, nil)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	if got := w.Current(); len(got.Rules) != 1 || got.Rules[0] != "initial" {
		t.Errorf("expected the fallback initial value, got %+v", got)
	}
}

func TestWatcher_LoadsExistingOverrideFileAtConstruction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeVersioned(t, path, 1, testConfig{Rules: []string{"from-file"}})

	w, err := NewWatcher[testConfig]("widgets", path, nil, &testConfig{Rules: []string{"initial"}}, parseTestConfig, nil)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	if got := w.Current(); len(got.Rules) != 1 || got.Rules[0] != "from-file" {
		t.Errorf("expected the on-disk override to win over the fallback initial, got %+v", got)
	}
}

func TestWatcher_ConstructionFailsClosedOnMalformedOverrideFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := NewWatcher[testConfig]("widgets", path, nil, &testConfig{}, parseTestConfig, nil); err == nil {
		t.Fatal("expected construction to fail closed on a malformed override file")
	}
}

func TestWatcher_ReloadsOnNATSPing(t *testing.T) {
	url := startTestNATSServer(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	writeVersioned(t, path, 1, testConfig{Rules: []string{"v1"}})

	w, err := NewWatcher[testConfig]("widgets", path, nc, &testConfig{}, parseTestConfig, nil)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Close()

	// Simulate: an admin update wrote the new k8s ConfigMap content, kubelet
	// synced it to this mounted file, and now the ping arrives.
	writeVersioned(t, path, 2, testConfig{Rules: []string{"v1", "v2"}})
	if err := nc.Publish(ConfigSubject("widgets"), nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := w.Current(); len(got.Rules) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Watcher did not reload within the deadline, current=%+v", w.Current())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWatcher_ReloadKeepsLastGoodValueOnTimeout(t *testing.T) {
	oldTimeout, oldBackoff, oldMax := ReloadTimeout, ReloadInitialBackoff, ReloadMaxBackoff
	ReloadTimeout = 200 * time.Millisecond
	ReloadInitialBackoff = 20 * time.Millisecond
	ReloadMaxBackoff = 50 * time.Millisecond
	defer func() { ReloadTimeout, ReloadInitialBackoff, ReloadMaxBackoff = oldTimeout, oldBackoff, oldMax }()

	url := startTestNATSServer(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	writeVersioned(t, path, 1, testConfig{Rules: []string{"v1"}})

	w, err := NewWatcher[testConfig]("widgets", path, nc, &testConfig{}, parseTestConfig, nil)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Close()

	// Ping fires but the file's content never actually changes (simulating
	// a kubelet sync that never lands within the bounded retry window).
	if err := nc.Publish(ConfigSubject("widgets"), nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	nc.Flush()

	time.Sleep(ReloadTimeout + 300*time.Millisecond)

	if got := w.Current(); len(got.Rules) != 1 || got.Rules[0] != "v1" {
		t.Errorf("expected the last-good value to survive a reload timeout, got %+v", got)
	}
}

func TestWatcher_ReloadRetriesMalformedContentThenSucceeds(t *testing.T) {
	url := startTestNATSServer(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	writeVersioned(t, path, 1, testConfig{Rules: []string{"v1"}})

	w, err := NewWatcher[testConfig]("widgets", path, nc, &testConfig{}, parseTestConfig, nil)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Close()

	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := nc.Publish(ConfigSubject("widgets"), nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	nc.Flush()

	time.Sleep(50 * time.Millisecond)
	writeVersioned(t, path, 2, testConfig{Rules: []string{"v2"}})

	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := w.Current(); len(got.Rules) == 1 && got.Rules[0] == "v2" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Watcher did not recover after a malformed intermediate write, current=%+v", w.Current())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWatcher_OnChangeFiresAtConstructionAndOnReload(t *testing.T) {
	url := startTestNATSServer(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	writeVersioned(t, path, 1, testConfig{Rules: []string{"v1"}})

	var mu sync.Mutex
	var seen []testConfig
	onChange := func(c *testConfig) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, *c)
	}

	w, err := NewWatcher[testConfig]("widgets", path, nc, &testConfig{}, parseTestConfig, onChange)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Close()

	mu.Lock()
	if len(seen) != 1 || seen[0].Rules[0] != "v1" {
		t.Errorf("expected onChange to fire once at construction with the loaded value, got %+v", seen)
	}
	mu.Unlock()

	writeVersioned(t, path, 2, testConfig{Rules: []string{"v2"}})
	nc.Publish(ConfigSubject("widgets"), nil)
	nc.Flush()

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got := len(seen)
		mu.Unlock()
		if got == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected onChange to fire again on reload, seen=%+v", seen)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// setPollInterval shortens PollInterval for one test and restores it at
// cleanup. Cleanups run LIFO, so register it BEFORE creating any Watcher
// whose Close is deferred: the Watcher is then fully quiesced (Close waits
// for its poll goroutine) before the package var is restored.
func setPollInterval(t *testing.T, d time.Duration) {
	t.Helper()
	old := PollInterval
	PollInterval = d
	t.Cleanup(func() { PollInterval = old })
}

func waitForRules(t *testing.T, w *Watcher[testConfig], want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if got := w.Current(); got != nil && len(got.Rules) > 0 && got.Rules[len(got.Rules)-1] == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Watcher never reached %q within %s, current=%+v", want, within, w.Current())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestWatcher_StaleAfterPingIsRecoveredByPoll reproduces the bug seen on a
// fresh deploy: one ping, the ping-driven reload applies the FIRST content
// the mounted file shows (the kubelet's stale v1) and stops, then the
// file's real update (v2) lands with no further ping. Without the poller
// the Watcher stayed on v1 forever.
func TestWatcher_StaleAfterPingIsRecoveredByPoll(t *testing.T) {
	setPollInterval(t, 50*time.Millisecond)
	url := startTestNATSServer(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()

	// No override file yet at construction (pod started before the
	// ConfigMap existed).
	path := filepath.Join(t.TempDir(), "config.json")
	w, err := NewWatcher[testConfig]("widgets", path, nc, &testConfig{Rules: []string{"initial"}}, parseTestConfig, nil)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Close()

	// The ping for v2 arrives; the kubelet first delivers its stale v1.
	if err := nc.Publish(ConfigSubject("widgets"), nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	writeVersioned(t, path, 1, testConfig{Rules: []string{"v1"}})
	waitForRules(t, w, "v1", 5*time.Second)

	// The real update reaches the file afterwards — no ping follows.
	writeVersioned(t, path, 2, testConfig{Rules: []string{"v2"}})
	waitForRules(t, w, "v2", 2*time.Second)
}

func TestWatcher_PollPicksUpChangeWithoutAnyPing(t *testing.T) {
	setPollInterval(t, 50*time.Millisecond)
	url := startTestNATSServer(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	writeVersioned(t, path, 1, testConfig{Rules: []string{"v1"}})
	w, err := NewWatcher[testConfig]("widgets", path, nc, &testConfig{}, parseTestConfig, nil)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Close()

	// e.g. the ConfigMap edited directly, or the ping lost during a NATS
	// reconnect.
	writeVersioned(t, path, 2, testConfig{Rules: []string{"v2"}})
	waitForRules(t, w, "v2", 2*time.Second)
}

func TestWatcher_CloseStopsPolling(t *testing.T) {
	setPollInterval(t, 20*time.Millisecond)
	url := startTestNATSServer(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	writeVersioned(t, path, 1, testConfig{Rules: []string{"v1"}})
	w, err := NewWatcher[testConfig]("widgets", path, nc, &testConfig{}, parseTestConfig, nil)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	done := make(chan struct{})
	go func() { _ = w.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return promptly")
	}

	writeVersioned(t, path, 2, testConfig{Rules: []string{"v2"}})
	time.Sleep(10 * PollInterval)
	if got := w.Current(); got.Rules[0] != "v1" {
		t.Errorf("expected no reload after Close, got %+v", got)
	}
}

func TestWatcher_NoPollWithoutNATS(t *testing.T) {
	setPollInterval(t, 20*time.Millisecond)
	path := filepath.Join(t.TempDir(), "config.json")
	writeVersioned(t, path, 1, testConfig{Rules: []string{"v1"}})
	w, err := NewWatcher[testConfig]("widgets", path, nil, &testConfig{}, parseTestConfig, nil)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Close()

	// nil nc keeps the documented contract: Current never changes after
	// construction.
	writeVersioned(t, path, 2, testConfig{Rules: []string{"v2"}})
	time.Sleep(10 * PollInterval)
	if got := w.Current(); got.Rules[0] != "v1" {
		t.Errorf("expected no reload without NATS, got %+v", got)
	}
}

func TestWatcher_VersionTracksTheLoadedOverrideFile(t *testing.T) {
	// No override file yet: still on the initial fallback, version 0.
	w, err := NewWatcher[testConfig]("widgets", "", nil, &testConfig{Rules: []string{"initial"}}, parseTestConfig, nil)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	if got := w.Version(); got != 0 {
		t.Errorf("initial fallback: expected version 0, got %d", got)
	}

	setPollInterval(t, 50*time.Millisecond)
	url := startTestNATSServer(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()

	path := filepath.Join(t.TempDir(), "config.json")
	writeVersioned(t, path, 3, testConfig{Rules: []string{"v3"}})
	w, err = NewWatcher[testConfig]("widgets", path, nc, &testConfig{}, parseTestConfig, nil)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Close()
	if got := w.Version(); got != 3 {
		t.Errorf("after construction: expected version 3, got %d", got)
	}

	writeVersioned(t, path, 4, testConfig{Rules: []string{"v4"}})
	waitForRules(t, w, "v4", 2*time.Second)
	if got := w.Version(); got != 4 {
		t.Errorf("after reload: expected version 4, got %d", got)
	}

	// A local admin update has no file behind it, so it leaves the version alone.
	w.SetLocal(&testConfig{Rules: []string{"local"}})
	if got := w.Version(); got != 4 {
		t.Errorf("after SetLocal: expected version to stay 4, got %d", got)
	}
}
