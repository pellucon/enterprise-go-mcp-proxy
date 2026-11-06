// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package ratelimit

import (
	"sort"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually-advanced clock, so tests can control elapsed time
// deterministically instead of relying on real time.Sleep calls (or even on
// the small, real, wall-clock gaps between successive statements, which
// would make "zero elapsed time" tests only *practically* non-flaky rather
// than actually hermetic).
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// mustNewLimiter builds a Limiter on a frozen fake clock (never advances
// unless the test advances it explicitly), so every call in a test behaves
// as "zero elapsed time" (no decay) unless stated otherwise.
func mustNewLimiter(t *testing.T, cfg *Config, opts ...Option) *Limiter {
	t.Helper()
	clock := newFakeClock()
	allOpts := append([]Option{WithClock(clock.Now)}, opts...)
	l, err := NewLimiter(cfg, allOpts...)
	if err != nil {
		t.Fatalf("NewLimiter: unexpected error: %v", err)
	}
	return l
}

// mustNewLimiterWithClock is like mustNewLimiter but hands back the fake
// clock too, for tests that need to advance it.
func mustNewLimiterWithClock(t *testing.T, cfg *Config) (*Limiter, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	l, err := NewLimiter(cfg, WithClock(clock.Now))
	if err != nil {
		t.Fatalf("NewLimiter: unexpected error: %v", err)
	}
	return l, clock
}

// countAllowed drives n calls to tool with roles through l, all at whatever
// the (frozen, unless advanced by the test) clock currently reads, and
// returns how many were allowed.
func countAllowed(l *Limiter, tool string, roles []string, n int) int {
	allowed := 0
	for i := 0; i < n; i++ {
		if l.Allow(tool, roles) {
			allowed++
		}
	}
	return allowed
}

// --- 1. No global limit configured -----------------------------------------

func TestCheck_NoGlobalLimit_AlwaysAllowed(t *testing.T) {
	cfg := &Config{Tools: map[string]ToolConfig{"toolA": {}}}
	l := mustNewLimiter(t, cfg)

	for i := 0; i < 1000; i++ {
		d := l.Check("toolA", []string{"anyone"})
		if !d.Allowed || d.Reason != ReasonUnlimited {
			t.Fatalf("call %d: expected {true, ReasonUnlimited}, got %+v", i, d)
		}
	}
}

func TestCheck_ToolAbsentFromConfig_AlwaysAllowed(t *testing.T) {
	cfg := &Config{Tools: map[string]ToolConfig{
		"known-tool": {GlobalPerMinute: intPtr(1)},
	}}
	l := mustNewLimiter(t, cfg)

	for i := 0; i < 100; i++ {
		d := l.Check("unknown-tool", []string{"anyone"})
		if !d.Allowed || d.Reason != ReasonUnlimited {
			t.Fatalf("call %d: expected {true, ReasonUnlimited}, got %+v", i, d)
		}
	}
}

func TestCheck_NilConfig_AlwaysAllowed(t *testing.T) {
	l := mustNewLimiter(t, nil)
	for i := 0; i < 100; i++ {
		if !l.Allow("anything", []string{"anyone"}) {
			t.Fatalf("call %d: expected allowed with a nil config", i)
		}
	}
}

// --- 2 & 9. No role percentages configured: plain equal-split limiter ------

func TestCheck_NoRolePercent_EqualSplitAmongActiveRolesOnceContended(t *testing.T) {
	// G=99, no configured roles: the entire contended capacity is the
	// unconfigured pool, split equally among distinct active roles.
	cfg := &Config{Tools: map[string]ToolConfig{"t": {GlobalPerMinute: intPtr(99)}}}
	l := mustNewLimiter(t, cfg)

	roles := [][]string{{"x"}, {"y"}, {"z"}}
	allowedByRole := map[string]int{"x": 0, "y": 0, "z": 0}

	// 33 rounds of x,y,z: fills the calm phase exactly (33*3=99=G) with a
	// perfectly even split, since every call in this phase is unconditionally
	// admitted regardless of role.
	for round := 0; round < 33; round++ {
		for _, r := range roles {
			d := l.Check("t", r)
			if !d.Allowed || d.Reason != ReasonBelowGlobal {
				t.Fatalf("round %d role %v: expected calm-phase allow, got %+v", round, r, d)
			}
			allowedByRole[r[0]]++
		}
	}

	// The 100th call (any role): now contended. All three roles are exactly
	// at their fair share (33 each, summing to 99=G), so every one of them
	// should now be denied immediately.
	for _, r := range roles {
		d := l.Check("t", r)
		if d.Allowed {
			t.Fatalf("role %v: expected denial once all three are exactly at their fair share, got %+v", r, d)
		}
		if d.Reason != ReasonUnconfiguredPoolExhausted {
			t.Fatalf("role %v: expected ReasonUnconfiguredPoolExhausted, got %v", r, d.Reason)
		}
	}

	for role, n := range allowedByRole {
		if n != 33 {
			t.Fatalf("role %s: expected exactly 33 admits during the even split, got %d", role, n)
		}
	}
}

func TestCheck_NoRolePercent_NewEntrantJoinsExistingActiveRoles(t *testing.T) {
	// G=3: x alone consumes the entire calm phase, then y shows up for the
	// first time once already contended and must still get a fair share
	// (not zero, despite arriving late and despite x having hogged the calm
	// phase).
	cfg := &Config{Tools: map[string]ToolConfig{"t": {GlobalPerMinute: intPtr(3)}}}
	l := mustNewLimiter(t, cfg)

	for i := 0; i < 3; i++ {
		if !l.Allow("t", []string{"x"}) {
			t.Fatalf("calm-phase call %d for x: expected allow", i)
		}
	}
	// Contended now. x's own rate is 3; y is brand new (rate 0). Active
	// count = 2 (x has rate>0, y is the current caller), share = 3/2 = 1.5.
	d := l.Check("t", []string{"y"})
	if !d.Allowed || d.Reason != ReasonUnconfiguredPool {
		t.Fatalf("y's first contended call: expected allow via the unconfigured pool, got %+v", d)
	}
	d = l.Check("t", []string{"y"}) // y's rate now 1, still < 1.5
	if !d.Allowed {
		t.Fatalf("y's second contended call: expected allow (1 < 1.5), got %+v", d)
	}
	d = l.Check("t", []string{"y"}) // y's rate now 2, not < 1.5
	if d.Allowed {
		t.Fatalf("y's third contended call: expected denial (2 not < 1.5), got %+v", d)
	}
}

// --- 3. Below G, a single role can monopolize all traffic unrestricted -----

func TestCheck_BelowGlobal_SingleRoleUnrestricted(t *testing.T) {
	cfg := &Config{Tools: map[string]ToolConfig{"t": {
		GlobalPerMinute: intPtr(5),
		RolePercent:     map[string]float64{"tiny": 10}, // deliberately small
	}}}
	l := mustNewLimiter(t, cfg)

	// A role configured for only 10% of the pool should still be able to use
	// the FULL global limit while the tool isn't contended.
	for i := 0; i < 5; i++ {
		d := l.Check("t", []string{"tiny"})
		if !d.Allowed || d.Reason != ReasonBelowGlobal {
			t.Fatalf("call %d: expected calm-phase allow despite tiny role percent, got %+v", i, d)
		}
	}
}

// --- 5. Both configured roles maxed simultaneously: proportional split -----

func TestCheck_ConfiguredPool_ProportionalSplitWhenBothMaxed(t *testing.T) {
	// G=100, A=70%, B=20% (leftover=10%). gConfigured=90.
	cfg := &Config{Tools: map[string]ToolConfig{"t": {
		GlobalPerMinute: intPtr(100),
		RolePercent:     map[string]float64{"A": 70, "B": 20},
	}}}
	l := mustNewLimiter(t, cfg)

	// Fill the calm phase with a neutral, unconfigured role so A and B's own
	// counters start the contended phase at zero.
	for i := 0; i < 100; i++ {
		if !l.Allow("t", []string{"filler"}) {
			t.Fatalf("calm-phase filler call %d: expected allow", i)
		}
	}

	allowedA, allowedB := 0, 0
	// Interleave A and B so both become "active" immediately and stay that
	// way for the rest of the run (frozen clock -> no decay).
	for round := 0; round < 150; round++ {
		if l.Allow("t", []string{"A"}) {
			allowedA++
		}
		if l.Allow("t", []string{"B"}) {
			allowedB++
		}
	}

	if allowedA != 70 {
		t.Errorf("expected A admitted exactly 70 times (its own percentage of G), got %d", allowedA)
	}
	if allowedB != 20 {
		t.Errorf("expected B admitted exactly 20 times (its own percentage of G), got %d", allowedB)
	}
	if allowedA+allowedB != 90 {
		t.Errorf("expected total configured-pool admits to equal gConfigured=90 exactly, got %d", allowedA+allowedB)
	}
}

// --- 6. Oversubscription within a pool is structurally impossible ----------

func TestCheck_ConfiguredPool_NeverExceedsItsCapacity_AnyActiveSubset(t *testing.T) {
	// G=100, P=30%, Q=30%, R=30% (leftover=10%). gConfigured=90.
	cfg := &Config{Tools: map[string]ToolConfig{"t": {
		GlobalPerMinute: intPtr(100),
		RolePercent:     map[string]float64{"P": 30, "Q": 30, "R": 30},
	}}}

	t.Run("two of three roles active", func(t *testing.T) {
		l := mustNewLimiter(t, cfg)
		for i := 0; i < 100; i++ {
			l.Allow("t", []string{"filler"})
		}
		allowedP, allowedQ := 0, 0
		for round := 0; round < 150; round++ {
			if l.Allow("t", []string{"P"}) {
				allowedP++
			}
			if l.Allow("t", []string{"Q"}) {
				allowedQ++
			}
		}
		if total := allowedP + allowedQ; total != 90 {
			t.Fatalf("expected P+Q to total exactly gConfigured=90 (never more), got %d (P=%d, Q=%d)", total, allowedP, allowedQ)
		}
		if allowedP != 45 || allowedQ != 45 {
			t.Fatalf("expected an even 45/45 split between the two active roles, got P=%d Q=%d", allowedP, allowedQ)
		}
	})

	t.Run("all three roles active", func(t *testing.T) {
		l := mustNewLimiter(t, cfg)
		for i := 0; i < 100; i++ {
			l.Allow("t", []string{"filler"})
		}
		allowed := map[string]int{"P": 0, "Q": 0, "R": 0}
		for round := 0; round < 150; round++ {
			for _, r := range []string{"P", "Q", "R"} {
				if l.Allow("t", []string{r}) {
					allowed[r]++
				}
			}
		}
		total := allowed["P"] + allowed["Q"] + allowed["R"]
		if total != 90 {
			t.Fatalf("expected total to equal exactly gConfigured=90 (never more), got %d: %v", total, allowed)
		}
		if allowed["P"] != 30 || allowed["Q"] != 30 || allowed["R"] != 30 {
			t.Fatalf("expected an even 30/30/30 split, got %v", allowed)
		}
	})
}

// --- 7. Borrowing while idle, configured pool -------------------------------

func TestCheck_ConfiguredPool_BorrowsIdleSiblingShare(t *testing.T) {
	// G=100, A=70%, B=20% (leftover=10%), but B never calls at all.
	cfg := &Config{Tools: map[string]ToolConfig{"t": {
		GlobalPerMinute: intPtr(100),
		RolePercent:     map[string]float64{"A": 70, "B": 20},
	}}}
	l := mustNewLimiter(t, cfg)

	for i := 0; i < 100; i++ {
		l.Allow("t", []string{"filler"})
	}

	// A alone, contended: active = {A} only, share = (70/70)*90 = 90, i.e.
	// the FULL configured pool, borrowing B's entirely unused share.
	allowed := countAllowed(l, "t", []string{"A"}, 200)
	if allowed != 90 {
		t.Fatalf("expected A to be admitted exactly 90 times (the full gConfigured, borrowing B's idle share), got %d", allowed)
	}
}

// --- 8. Unconfigured pool gets a real, guaranteed floor (when headroom exists) --

func TestCheck_UnconfiguredPool_GetsGuaranteedFloor(t *testing.T) {
	// G=100, A=70%, B=20% (leftover=10% -> gUnconfigured=10).
	cfg := &Config{Tools: map[string]ToolConfig{"t": {
		GlobalPerMinute: intPtr(100),
		RolePercent:     map[string]float64{"A": 70, "B": 20},
	}}}
	l := mustNewLimiter(t, cfg)

	for i := 0; i < 100; i++ {
		l.Allow("t", []string{"A"}) // saturate via a configured role
	}

	allowed := 0
	var lastDenyReason Reason
	for i := 0; i < 20; i++ {
		d := l.Check("t", []string{"guest"})
		if d.Allowed {
			allowed++
		} else {
			lastDenyReason = d.Reason
		}
	}
	if allowed != 10 {
		t.Fatalf("expected the unconfigured caller to be admitted exactly gUnconfigured=10 times, got %d", allowed)
	}
	if lastDenyReason != ReasonUnconfiguredPoolExhausted {
		t.Fatalf("expected subsequent denials to report ReasonUnconfiguredPoolExhausted, got %v", lastDenyReason)
	}
}

// TestCheck_UnconfiguredPool_StarvesWhenSumIsExactly100 is the v2 behavior
// change (per rate-limiting.md's update): a config whose percentages sum to
// exactly 100 is now valid (see config_test.go), and leaves ZERO floor for
// unconfigured roles -- they are flatly, indefinitely denied once contended,
// same as v1's original (pre-percentage) starvation behavior. This is a
// deliberate, accepted regression against the earlier "always keep a
// nonzero floor" guarantee, not a bug.
func TestCheck_UnconfiguredPool_StarvesWhenSumIsExactly100(t *testing.T) {
	cfg := &Config{Tools: map[string]ToolConfig{"t": {
		GlobalPerMinute: intPtr(100),
		RolePercent:     map[string]float64{"A": 60, "B": 40}, // sum == 100, no leftover
	}}}
	l := mustNewLimiter(t, cfg)

	for i := 0; i < 100; i++ {
		l.Allow("t", []string{"A"})
	}

	for i := 0; i < 20; i++ {
		d := l.Check("t", []string{"guest"})
		if d.Allowed {
			t.Fatalf("call %d: expected the unconfigured caller to be flatly denied when sum==100 (no floor left), got %+v", i, d)
		}
		if d.Reason != ReasonUnconfiguredPoolExhausted {
			t.Fatalf("call %d: expected ReasonUnconfiguredPoolExhausted, got %v", i, d.Reason)
		}
	}
}

// --- 10. The two pools never cross ------------------------------------------

func TestCheck_PoolsNeverCross(t *testing.T) {
	cfg := &Config{Tools: map[string]ToolConfig{"t": {
		GlobalPerMinute: intPtr(100),
		RolePercent:     map[string]float64{"A": 70, "B": 20},
	}}}

	t.Run("saturated configured pool does not restrict a fresh unconfigured caller", func(t *testing.T) {
		l := mustNewLimiter(t, cfg)
		for i := 0; i < 100; i++ {
			l.Allow("t", []string{"A"})
		}
		countAllowed(l, "t", []string{"A"}, 200) // fully exhaust A's own share too

		// The unconfigured pool was never touched -- a brand new
		// unconfigured caller should get its full share (10), unaffected by
		// how saturated the configured pool is.
		allowed := countAllowed(l, "t", []string{"guest"}, 20)
		if allowed != 10 {
			t.Fatalf("expected the untouched unconfigured pool to grant its full share (10), got %d", allowed)
		}
	})

	t.Run("saturated unconfigured pool does not restrict a fresh configured caller", func(t *testing.T) {
		l := mustNewLimiter(t, cfg)
		for i := 0; i < 100; i++ {
			l.Allow("t", []string{"guest"})
		}
		countAllowed(l, "t", []string{"guest"}, 20) // fully exhaust the unconfigured pool's share

		// The configured pool was never touched -- role A calling for the
		// first time should get the full gConfigured (90), borrowing B's
		// unused share too, unaffected by the saturated unconfigured pool.
		allowed := countAllowed(l, "t", []string{"A"}, 200)
		if allowed != 90 {
			t.Fatalf("expected the untouched configured pool to grant A the full gConfigured (90), got %d", allowed)
		}
	})
}

// --- 11. Recovery via decay -------------------------------------------------

func TestCheck_RecoversAfterClockAdvances(t *testing.T) {
	cfg := &Config{Tools: map[string]ToolConfig{"t": {GlobalPerMinute: intPtr(10)}}}
	l, clock := mustNewLimiterWithClock(t, cfg)

	for i := 0; i < 10; i++ {
		if !l.Allow("t", []string{"x"}) {
			t.Fatalf("calm-phase call %d: expected allow", i)
		}
	}
	if l.Allow("t", []string{"x"}) {
		t.Fatal("expected the 11th call (same role) to be denied while fully saturated")
	}

	// Advance well past a full minute: totalRate (and x's own bucket) should
	// decay all the way back to 0.
	clock.Advance(90 * time.Second)

	d := l.Check("t", []string{"x"})
	if !d.Allowed || d.Reason != ReasonBelowGlobal {
		t.Fatalf("after the clock advanced 90s, expected a fresh calm-phase allow, got %+v", d)
	}
}

func TestCheck_PartialDecayGraduallyFreesUpCapacity(t *testing.T) {
	cfg := &Config{Tools: map[string]ToolConfig{"t": {GlobalPerMinute: intPtr(60)}}}
	l, clock := mustNewLimiterWithClock(t, cfg)

	for i := 0; i < 60; i++ {
		l.Allow("t", []string{"x"})
	}
	if l.Allow("t", []string{"x"}) {
		t.Fatal("expected denial immediately after fully saturating a 60/min limit")
	}

	// Refill rate is limit/60 per second = 1/sec here. Advancing 10 seconds
	// should free up exactly 10 units of capacity.
	clock.Advance(10 * time.Second)
	allowed := countAllowed(l, "t", []string{"x"}, 20)
	if allowed != 10 {
		t.Fatalf("expected exactly 10 calls freed up after a 10s decay (rate=1/sec), got %d", allowed)
	}
}

// --- Tie-break and canonical-key grouping -----------------------------------

func TestCheck_TieBreak_LexicographicallyFirstRoleWins(t *testing.T) {
	// G=100, zzz=40%, aaa=40% (tie), leftover=20%. gConfigured=80.
	cfg := &Config{Tools: map[string]ToolConfig{"t": {
		GlobalPerMinute: intPtr(100),
		RolePercent:     map[string]float64{"zzz": 40, "aaa": 40},
	}}}
	l := mustNewLimiter(t, cfg)
	for i := 0; i < 100; i++ {
		l.Allow("t", []string{"filler"}) // fill calm phase without touching aaa/zzz
	}

	// A caller presenting both tied roles should always be attributed to
	// "aaa" (lexicographically first), deterministically. Since "zzz" is
	// never independently incremented by these calls, "aaa" should be able
	// to claim the ENTIRE gConfigured (80), borrowing "zzz"'s completely
	// untouched share -- if the tie-break were nondeterministic, some calls
	// would instead land on "zzz" and this total would come out lower.
	dualRoleAllowed := countAllowed(l, "t", []string{"aaa", "zzz"}, 200)
	if dualRoleAllowed != 80 {
		t.Fatalf("expected the dual-role caller (tie broken to 'aaa') to claim the full gConfigured=80, got %d", dualRoleAllowed)
	}

	// "zzz" alone, afterward: "aaa"'s bucket (now at 80, fully exhausted)
	// still counts as "active" (it has nonzero recorded usage, even though
	// it's itself now fully spent), so "zzz" is diluted to its own dynamic
	// share of gConfigured (40) rather than getting the full pool -- this
	// asymmetry (aaa reaches 80, zzz is capped at 40) is what actually
	// proves the tie consistently broke towards "aaa", not a coin flip.
	zzzAllowed := countAllowed(l, "t", []string{"zzz"}, 200)
	if zzzAllowed != 40 {
		t.Fatalf("expected zzz's own bucket (diluted by aaa's still-nonzero usage) to admit exactly 40, got %d", zzzAllowed)
	}
}

func TestCanonicalRoleKey_OrderIndependentGroupingAndDistinctSets(t *testing.T) {
	a := []string{"b", "a", "c"}
	sort.Strings(a)
	b := []string{"c", "b", "a"}
	sort.Strings(b)
	if canonicalRoleKey(a) != canonicalRoleKey(b) {
		t.Fatalf("expected the same role set (different input order) to produce the same key: %q vs %q", canonicalRoleKey(a), canonicalRoleKey(b))
	}

	c := []string{"a", "b"}
	sort.Strings(c)
	if canonicalRoleKey(a) == canonicalRoleKey(c) {
		t.Fatalf("expected different role sets to produce different keys, both got %q", canonicalRoleKey(a))
	}
}

func TestCheck_UnconfiguredPool_DistinctRoleSetsGetSeparateShares(t *testing.T) {
	// G=100, A=70% (leftover=30% -> gUnconfigured=30). Two DIFFERENT
	// unconfigured role sets should each be tracked independently, sharing
	// the pool equally once both are simultaneously active.
	cfg := &Config{Tools: map[string]ToolConfig{"t": {
		GlobalPerMinute: intPtr(100),
		RolePercent:     map[string]float64{"A": 70},
	}}}
	l := mustNewLimiter(t, cfg)
	for i := 0; i < 100; i++ {
		l.Allow("t", []string{"A"})
	}

	// guest1 and guest2 interleaved: both become active, equal split of 30.
	allowed1, allowed2 := 0, 0
	for round := 0; round < 50; round++ {
		if l.Allow("t", []string{"guest1"}) {
			allowed1++
		}
		if l.Allow("t", []string{"guest2"}) {
			allowed2++
		}
	}
	if allowed1 != 15 || allowed2 != 15 {
		t.Fatalf("expected an even 15/15 split of gUnconfigured=30 between two distinct active unconfigured role sets, got guest1=%d guest2=%d", allowed1, allowed2)
	}
}

func TestCheck_UnconfiguredPool_PruningDoesNotAffectFreshRoleSets(t *testing.T) {
	// G=10, no configured roles. "temp" calls once, then is never seen again
	// long enough for its entry to fully decay and be pruned; a later
	// "temp2" role set should still get a completely fresh, undiluted share
	// (proving pruning happened rather than leaving a stale zero-but-present
	// entry that could otherwise be mistaken for still "active").
	cfg := &Config{Tools: map[string]ToolConfig{"t": {GlobalPerMinute: intPtr(10)}}}
	l, clock := mustNewLimiterWithClock(t, cfg)

	for i := 0; i < 10; i++ {
		l.Allow("t", []string{"temp"})
	}
	if l.Allow("t", []string{"temp"}) {
		t.Fatal("expected denial once temp has used the entire pool")
	}

	clock.Advance(60 * time.Second) // fully decays temp's entry back to 0 and prunes it

	// temp2 (a different role set) should now get the FULL pool (10) to
	// itself, exactly as if it were the very first caller ever -- if temp's
	// stale entry had lingered un-pruned at some nonzero value, or if
	// pruning were broken, this could come out wrong.
	allowed := countAllowed(l, "t", []string{"temp2"}, 20)
	if allowed != 10 {
		t.Fatalf("expected temp2 to get the full pool (10) after temp's entry decayed away, got %d", allowed)
	}
}

// --- Concurrency -------------------------------------------------------------

func TestCheck_ConcurrentCallers_NoRace(t *testing.T) {
	cfg := &Config{Tools: map[string]ToolConfig{"t": {
		GlobalPerMinute: intPtr(1000),
		RolePercent:     map[string]float64{"A": 50, "B": 30},
	}}}
	l, err := NewLimiter(cfg) // real clock: this test only checks race-safety
	if err != nil {
		t.Fatalf("NewLimiter: unexpected error: %v", err)
	}

	var wg sync.WaitGroup
	roleSets := [][]string{{"A"}, {"B"}, {"guest1"}, {"guest2"}}
	const goroutines = 50
	const callsPerGoroutine = 200

	for i := 0; i < goroutines; i++ {
		roles := roleSets[i%len(roleSets)]
		wg.Add(1)
		go func(roles []string) {
			defer wg.Done()
			for j := 0; j < callsPerGoroutine; j++ {
				l.Allow("t", roles)
			}
		}(roles)
	}
	wg.Wait()
	// No assertion beyond "did not race/deadlock/panic" -- run this test
	// with `go test -race` to get the actual guarantee.
}

func TestCheck_ConcurrentUpdateAndCheck_NoRace(t *testing.T) {
	cfg := &Config{Tools: map[string]ToolConfig{"t": {GlobalPerMinute: intPtr(1000)}}}
	l, err := NewLimiter(cfg)
	if err != nil {
		t.Fatalf("NewLimiter: unexpected error: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = l.Update(&Config{Tools: map[string]ToolConfig{
				"t": {GlobalPerMinute: intPtr(1000 + i%10)},
			}})
		}
	}()

	for i := 0; i < 500; i++ {
		l.Allow("t", []string{"x"})
	}
	close(stop)
	wg.Wait()
}

func TestUpdate_RejectsInvalidConfig_KeepsPreviousOneInPlace(t *testing.T) {
	cfg := &Config{Tools: map[string]ToolConfig{"t": {GlobalPerMinute: intPtr(5)}}}
	l := mustNewLimiter(t, cfg)

	bad := &Config{Tools: map[string]ToolConfig{"t": {GlobalPerMinute: intPtr(-1)}}}
	if err := l.Update(bad); err == nil {
		t.Fatal("expected Update to reject an invalid config")
	}
	got := l.Snapshot()
	if *got.Tools["t"].GlobalPerMinute != 5 {
		t.Fatalf("expected the previous valid config to remain in place, got %+v", got.Tools["t"])
	}
}

func TestUpdate_PreservesUsageStateAcrossConfigChange(t *testing.T) {
	cfg := &Config{Tools: map[string]ToolConfig{"t": {GlobalPerMinute: intPtr(5)}}}
	l := mustNewLimiter(t, cfg)

	for i := 0; i < 5; i++ {
		l.Allow("t", []string{"x"})
	}
	if l.Allow("t", []string{"x"}) {
		t.Fatal("expected saturation before the config update")
	}

	// Update to a config with a much higher limit -- same tool name, so its
	// existing usage state carries over. Since totalRate (5) is still <
	// the new limit (100), the very next call should be freely admitted.
	if err := l.Update(&Config{Tools: map[string]ToolConfig{"t": {GlobalPerMinute: intPtr(100)}}}); err != nil {
		t.Fatalf("Update: unexpected error: %v", err)
	}
	if !l.Allow("t", []string{"x"}) {
		t.Fatal("expected the call to be admitted once the limit was raised")
	}
}

func TestNewLimiter_RejectsInvalidConfig(t *testing.T) {
	bad := &Config{Tools: map[string]ToolConfig{"t": {GlobalPerMinute: intPtr(-1)}}}
	if _, err := NewLimiter(bad); err == nil {
		t.Fatal("expected NewLimiter to reject an invalid config")
	}
}
