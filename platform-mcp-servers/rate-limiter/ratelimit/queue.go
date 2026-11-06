// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package ratelimit

import "sync"

// ticketQueue is a FIFO of waiters for one (tool, role) admission group,
// used by Limiter.Wait to guarantee strict arrival order among callers
// competing for the same role's share -- something Check alone can't
// provide, since Check has no notion of "who asked first" (see its own doc
// comment). A waiter enqueues once and is handed a channel that closes the
// instant it reaches the front; it may also leave early (e.g. its own ctx
// was cancelled) without blocking whoever's behind it -- leave always
// promotes the new front, so an abandoned ticket can never permanently
// stall the queue.
//
// Deliberately not tied to toolState.mu: a waiter blocks on its turn
// channel, potentially for a long time, and must never hold a lock Check
// needs to make progress for other callers/roles.
type ticketQueue struct {
	mu      sync.Mutex
	waiters []chan struct{}
}

// enqueue adds a new waiter to the back of the queue and returns a channel
// that's closed once it's this waiter's turn (already closed if the queue
// was empty), plus a leave function the caller must call exactly once
// (typically via defer) when it's done waiting, whether it was admitted,
// gave up, or anything else -- this is what advances the queue.
func (q *ticketQueue) enqueue() (turn <-chan struct{}, leave func()) {
	q.mu.Lock()
	defer q.mu.Unlock()

	ch := make(chan struct{})
	q.waiters = append(q.waiters, ch)
	if len(q.waiters) == 1 {
		close(ch) // queue was empty: already at the front
	}

	left := false
	leave = func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		if left {
			return // idempotent, in case a caller ever calls it twice
		}
		left = true
		for i, w := range q.waiters {
			if w == ch {
				q.waiters = append(q.waiters[:i], q.waiters[i+1:]...)
				break
			}
		}
		if len(q.waiters) == 0 {
			return
		}
		select {
		case <-q.waiters[0]:
			// front was already signaled (it was already at the front
			// before this waiter left; nothing to do)
		default:
			close(q.waiters[0])
		}
	}
	return ch, leave
}
