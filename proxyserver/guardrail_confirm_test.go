// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWaitForConfigVersion(t *testing.T) {
	// Reports old versions for a few polls, then the target - like the
	// guardrail server before and after the kubelet syncs its ConfigMap.
	t.Run("confirms once the target version is reported", func(t *testing.T) {
		calls := 0
		fetch := func(context.Context) (int64, error) {
			calls++
			if calls < 3 {
				return 3, nil
			}
			return 4, nil
		}
		ok, err := waitForConfigVersion(context.Background(), 4, fetch, time.Second, time.Millisecond)
		if err != nil || !ok {
			t.Fatalf("expected confirmation, got ok=%v err=%v", ok, err)
		}
		if calls != 3 {
			t.Errorf("expected 3 polls, got %d", calls)
		}
	})

	t.Run("a newer version also confirms", func(t *testing.T) {
		fetch := func(context.Context) (int64, error) { return 7, nil }
		if ok, err := waitForConfigVersion(context.Background(), 4, fetch, time.Second, time.Millisecond); err != nil || !ok {
			t.Fatalf("expected confirmation, got ok=%v err=%v", ok, err)
		}
	})

	t.Run("gives up after the timeout", func(t *testing.T) {
		fetch := func(context.Context) (int64, error) { return 3, nil }
		start := time.Now()
		ok, err := waitForConfigVersion(context.Background(), 4, fetch, 50*time.Millisecond, 5*time.Millisecond)
		if err != nil || ok {
			t.Fatalf("expected an unconfirmed timeout, got ok=%v err=%v", ok, err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("timeout not honored: waited %s", elapsed)
		}
	})

	t.Run("stops when the context is done", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		fetch := func(context.Context) (int64, error) { return 3, nil }
		if ok, err := waitForConfigVersion(ctx, 4, fetch, time.Minute, 10*time.Millisecond); err != nil || ok {
			t.Fatalf("expected an unconfirmed return, got ok=%v err=%v", ok, err)
		}
	})

	t.Run("a fetch error is returned without retrying", func(t *testing.T) {
		calls := 0
		fetch := func(context.Context) (int64, error) { calls++; return 0, errors.New("unknown tool") }
		ok, err := waitForConfigVersion(context.Background(), 4, fetch, time.Second, time.Millisecond)
		if err == nil || ok {
			t.Fatalf("expected the fetch error, got ok=%v err=%v", ok, err)
		}
		if calls != 1 {
			t.Errorf("expected no retry, got %d calls", calls)
		}
	})
}
