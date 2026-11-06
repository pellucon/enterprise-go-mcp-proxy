// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"
)

// --- ticketQueue: precise, deterministic (channel-based, no timing at all) --

func TestTicketQueue_SoloWaiterIsImmediatelyAtFront(t *testing.T) {
	q := &ticketQueue{}
	turn, leave := q.enqueue()
	defer leave()
	select {
	case <-turn:
	default:
		t.Fatal("expected a lone waiter to be immediately at the front")
	}
}

func TestTicketQueue_StrictFIFOOrder(t *testing.T) {
	q := &ticketQueue{}

	turn1, leave1 := q.enqueue()
	<-turn1

	turn2, leave2 := q.enqueue()
	turn3, leave3 := q.enqueue()
	turn4, leave4 := q.enqueue()
	defer leave4()

	assertNotYetTurn := func(name string, turn <-chan struct{}) {
		select {
		case <-turn:
			t.Fatalf("%s: expected not to be at the front yet", name)
		default:
		}
	}
	assertNotYetTurn("waiter 2", turn2)
	assertNotYetTurn("waiter 3", turn3)
	assertNotYetTurn("waiter 4", turn4)

	// Releasing 1 must promote 2, and ONLY 2 -- 3 and 4 still wait.
	leave1()
	select {
	case <-turn2:
	case <-time.After(time.Second):
		t.Fatal("expected waiter 2 to be promoted after waiter 1 left")
	}
	assertNotYetTurn("waiter 3", turn3)
	assertNotYetTurn("waiter 4", turn4)

	leave2()
	select {
	case <-turn3:
	case <-time.After(time.Second):
		t.Fatal("expected waiter 3 to be promoted after waiter 2 left")
	}
	assertNotYetTurn("waiter 4", turn4)

	leave3()
	select {
	case <-turn4:
	case <-time.After(time.Second):
		t.Fatal("expected waiter 4 to be promoted after waiter 3 left")
	}
}

func TestTicketQueue_AbandonmentDoesNotStallLaterWaiters(t *testing.T) {
	q := &ticketQueue{}

	turn1, leave1 := q.enqueue() // at the front
	<-turn1
	_, leave2 := q.enqueue() // queued behind 1
	turn3, leave3 := q.enqueue()
	defer leave3()

	// Waiter 2 gives up WITHOUT ever reaching the front (simulates a
	// caller's ctx being cancelled while still queued).
	leave2()

	// Waiter 3 must still be promoted once 1 leaves -- 2's abandonment
	// must not permanently orphan the queue.
	leave1()
	select {
	case <-turn3:
	case <-time.After(time.Second):
		t.Fatal("expected waiter 3 to be promoted despite waiter 2 abandoning before its turn")
	}
}

func TestTicketQueue_LeaveIsIdempotent(t *testing.T) {
	q := &ticketQueue{}
	_, leave := q.enqueue()
	leave()
	leave() // must not panic or double-promote
}

func TestTicketQueue_ConcurrentEnqueueLeaveNoRace(t *testing.T) {
	q := &ticketQueue{}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			turn, leave := q.enqueue()
			defer leave()
			select {
			case <-turn:
			case <-time.After(5 * time.Second):
			}
		}()
	}
	wg.Wait()
	// Run with -race; no explicit assertion beyond "didn't deadlock/panic".
}

// --- Limiter.Wait: integration-level, generous real-time margins for -------
// --- orchestration only (never for precision numeric thresholds)     -------

func TestWait_NoGlobalLimit_ReturnsImmediately(t *testing.T) {
	cfg := &Config{Tools: map[string]ToolConfig{"t": {}}}
	l := mustNewLimiter(t, cfg)
	d := l.Wait(context.Background(), "t", []string{"anyone"})
	if !d.Allowed || d.Reason != ReasonUnlimited {
		t.Fatalf("expected {true, ReasonUnlimited}, got %+v", d)
	}
}

func TestWait_AdmitsImmediatelyWhenBelowGlobal(t *testing.T) {
	cfg := &Config{Tools: map[string]ToolConfig{"t": {GlobalPerMinute: intPtr(10)}}}
	l := mustNewLimiter(t, cfg, WithPollInterval(5*time.Millisecond))
	d := l.Wait(context.Background(), "t", []string{"x"})
	if !d.Allowed || d.Reason != ReasonBelowGlobal {
		t.Fatalf("expected an immediate calm-phase allow, got %+v", d)
	}
}

func TestWait_AdmitsOnceCapacityDecaysBack(t *testing.T) {
	clock := newFakeClock()
	cfg := &Config{Tools: map[string]ToolConfig{"t": {GlobalPerMinute: intPtr(60)}}} // 1/sec refill
	l, err := NewLimiter(cfg, WithClock(clock.Now), WithPollInterval(5*time.Millisecond))
	if err != nil {
		t.Fatalf("NewLimiter: %v", err)
	}
	for i := 0; i < 60; i++ {
		l.Allow("t", []string{"x"})
	}
	if l.Allow("t", []string{"x"}) {
		t.Fatal("expected saturation before waiting")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan Decision, 1)
	go func() { done <- l.Wait(ctx, "t", []string{"x"}) }()

	time.Sleep(20 * time.Millisecond) // let the goroutine enqueue and start polling
	clock.Advance(2 * time.Second)    // frees up ~2 tokens

	select {
	case d := <-done:
		if !d.Allowed {
			t.Fatalf("expected Wait to admit once capacity decayed back, got %+v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait never returned after capacity freed up")
	}
}

// TestWait_SoleWaiterSeesRealCheckReasonOnTimeout proves ReasonQueueTimeout
// is specifically for giving up BEFORE reaching the front -- a lone waiter
// reaches the front immediately (empty queue), so a timeout here surfaces
// Check's own real exhausted reason instead.
func TestWait_SoleWaiterSeesRealCheckReasonOnTimeout(t *testing.T) {
	cfg := &Config{Tools: map[string]ToolConfig{"t": {GlobalPerMinute: intPtr(1)}}}
	l := mustNewLimiter(t, cfg, WithPollInterval(5*time.Millisecond)) // frozen clock: never decays
	l.Allow("t", []string{"x"})                                       // saturate

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	d := l.Wait(ctx, "t", []string{"x"})
	if d.Allowed {
		t.Fatal("expected denial: capacity never frees up on a frozen clock")
	}
	// Reached the front and retried Check (which returns a real Exhausted
	// reason) rather than timing out before ever reaching the front, since
	// it was the only waiter.
	if d.Reason != ReasonConfiguredPoolExhausted && d.Reason != ReasonUnconfiguredPoolExhausted {
		t.Errorf("expected a real Check-produced exhausted reason, got %v", d.Reason)
	}
}

// TestWait_QueuedWaiterTimesOutBeforeReachingFront proves ReasonQueueTimeout
// specifically: a second waiter, stuck behind a first who's still occupying
// the front (and won't leave for a long time), gives up before ever calling
// Check even once.
func TestWait_QueuedWaiterTimesOutBeforeReachingFront(t *testing.T) {
	cfg := &Config{Tools: map[string]ToolConfig{"t": {GlobalPerMinute: intPtr(1)}}}
	l := mustNewLimiter(t, cfg, WithPollInterval(5*time.Millisecond)) // frozen clock
	l.Allow("t", []string{"x"})                                       // saturate

	// Waiter 1: long ctx, takes the front and holds it (never admitted --
	// frozen clock never decays -- but won't give up for a long time).
	longCtx, longCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer longCancel()
	go func() { l.Wait(longCtx, "t", []string{"x"}) }()

	time.Sleep(10 * time.Millisecond) // let waiter 1 enqueue and reach the front first

	// Waiter 2: short ctx, enqueues behind waiter 1 and must time out
	// without ever getting a turn.
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer shortCancel()
	d := l.Wait(shortCtx, "t", []string{"x"})
	if d.Allowed || d.Reason != ReasonQueueTimeout {
		t.Fatalf("expected waiter 2 to time out with ReasonQueueTimeout (never reached the front), got %+v", d)
	}
}

func TestWait_AbandonedWaiterDoesNotBlockOthers(t *testing.T) {
	clock := newFakeClock()
	cfg := &Config{Tools: map[string]ToolConfig{"t": {GlobalPerMinute: intPtr(1)}}}
	l, err := NewLimiter(cfg, WithClock(clock.Now), WithPollInterval(5*time.Millisecond))
	if err != nil {
		t.Fatalf("NewLimiter: %v", err)
	}
	l.Allow("t", []string{"x"}) // saturate

	// Waiter A enqueues first and will time out quickly, abandoning its
	// place at the front of the queue.
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer shortCancel()
	doneA := make(chan Decision, 1)
	go func() { doneA <- l.Wait(shortCtx, "t", []string{"x"}) }()

	time.Sleep(5 * time.Millisecond) // let A enqueue and reach the front first

	// Waiter B enqueues behind A, with a long-enough deadline to survive
	// A's abandonment and still be served once capacity frees up.
	longCtx, longCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer longCancel()
	doneB := make(chan Decision, 1)
	go func() { doneB <- l.Wait(longCtx, "t", []string{"x"}) }()

	// A was the only waiter when it enqueued, so it reached the front
	// immediately and timed out while actively retrying Check -- its
	// reason is Check's own real exhausted reason, not ReasonQueueTimeout
	// (which is specific to giving up before ever reaching the front).
	dA := <-doneA
	if dA.Allowed {
		t.Fatalf("expected waiter A to be denied (capacity never freed before its timeout), got %+v", dA)
	}

	clock.Advance(90 * time.Second) // free up capacity now that A has abandoned

	select {
	case dB := <-doneB:
		if !dB.Allowed {
			t.Fatalf("expected waiter B to be admitted after A abandoned and capacity freed up, got %+v", dB)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiter B never completed -- A's abandonment appears to have stalled the queue")
	}
}

func TestWait_DifferentRolesUseIndependentQueues(t *testing.T) {
	// Role A fully saturated (and its own share exhausted); role B, a
	// completely fresh, never-touched sibling role, must be unaffected.
	cfg := &Config{Tools: map[string]ToolConfig{"t": {
		GlobalPerMinute: intPtr(100),
		RolePercent:     map[string]float64{"A": 50, "B": 50},
	}}}
	l := mustNewLimiter(t, cfg, WithPollInterval(5*time.Millisecond))
	for i := 0; i < 100; i++ {
		l.Allow("t", []string{"A"})
	}
	if l.Allow("t", []string{"A"}) {
		t.Fatal("expected role A to be saturated")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d := l.Wait(ctx, "t", []string{"B"})
	if !d.Allowed {
		t.Fatalf("expected role B's own queue/share to be completely unaffected by role A's saturation, got %+v", d)
	}
}
