// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package ratelimit

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// Reason explains why a Check call was allowed or denied.
type Reason string

const (
	// ReasonUnlimited: the tool has no configured global limit at all.
	ReasonUnlimited Reason = "unlimited"
	// ReasonBelowGlobal: the tool's aggregate traffic is below its global
	// limit, so the call was admitted unconditionally regardless of role.
	ReasonBelowGlobal Reason = "below_global"
	// ReasonConfiguredPool: the tool is contended and the caller's role has
	// a configured percentage; admitted within its (possibly borrowed) share
	// of the configured pool.
	ReasonConfiguredPool Reason = "configured_pool"
	// ReasonConfiguredPoolExhausted: the tool is contended, the caller's
	// role has a configured percentage, but its share of the configured
	// pool is currently exhausted.
	ReasonConfiguredPoolExhausted Reason = "configured_pool_exhausted"
	// ReasonUnconfiguredPool: the tool is contended and the caller's roles
	// have no configured percentage; admitted within its (possibly
	// borrowed) equal share of the unconfigured pool.
	ReasonUnconfiguredPool Reason = "unconfigured_pool"
	// ReasonUnconfiguredPoolExhausted: the tool is contended, the caller's
	// roles have no configured percentage, and its equal share of the
	// unconfigured pool is currently exhausted (or the tool leaves no
	// unconfigured pool at all).
	ReasonUnconfiguredPoolExhausted Reason = "unconfigured_pool_exhausted"
	// ReasonQueueTimeout: Wait's caller gave up (ctx done) before ever
	// reaching the front of its per-role FIFO queue -- it never got to
	// attempt Check even once. Distinct from a Check-produced *Exhausted
	// reason, which means it DID reach the front and was denied there.
	ReasonQueueTimeout Reason = "queue_timeout"
)

// Decision is the outcome of a Check call.
type Decision struct {
	Allowed bool
	Reason  Reason
}

// Option configures a Limiter at construction time.
type Option func(*Limiter)

// WithClock overrides the Limiter's time source. Intended for tests; real
// callers should leave this unset, which defaults to time.Now.
func WithClock(now func() time.Time) Option {
	return func(l *Limiter) {
		l.now = now
	}
}

// defaultPollInterval is how often Wait rechecks admission once it's at the
// front of its queue. Overridable via WithPollInterval, mainly for tests.
const defaultPollInterval = 100 * time.Millisecond

// WithPollInterval overrides how often Wait rechecks admission once it's at
// the front of its per-role queue. Intended for tests (a short interval
// keeps FIFO/timeout tests fast); real callers should leave this unset.
func WithPollInterval(d time.Duration) Option {
	return func(l *Limiter) {
		l.pollInterval = d
	}
}

// Limiter enforces per-tool, per-role call-rate limits. Construct one with
// NewLimiter; the zero value is not usable.
type Limiter struct {
	now          func() time.Time
	pollInterval time.Duration

	mu  sync.RWMutex
	cfg *Config

	statesMu sync.Mutex
	states   map[string]*toolState
}

// NewLimiter builds a Limiter from cfg. A nil cfg is valid and means every
// tool is unlimited, mirroring the "absent section is a total no-op"
// convention used elsewhere for optional configuration in this codebase.
// Returns an error, and no Limiter, if cfg fails Validate.
func NewLimiter(cfg *Config, opts ...Option) (*Limiter, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	l := &Limiter{
		now:          time.Now,
		pollInterval: defaultPollInterval,
		cfg:          cfg,
		states:       make(map[string]*toolState),
	}
	for _, opt := range opts {
		opt(l)
	}
	return l, nil
}

// Update atomically replaces the Limiter's configuration. Returns an error,
// leaving the previous configuration in place, if cfg fails Validate.
// Existing per-tool usage state is preserved across an Update -- a
// configuration change doesn't reset anyone's recent-usage history.
func (l *Limiter) Update(cfg *Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	l.mu.Lock()
	l.cfg = cfg
	l.mu.Unlock()
	return nil
}

// Snapshot returns the Limiter's current configuration. The returned value
// must be treated as read-only by the caller.
func (l *Limiter) Snapshot() *Config {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.cfg
}

// Allow reports whether a call to tool by a caller holding roles should be
// admitted. It is a convenience wrapper around Check for callers that don't
// need the reason for the decision.
func (l *Limiter) Allow(tool string, roles []string) bool {
	return l.Check(tool, roles).Allowed
}

// admissionClass is the result of resolving a caller's roles against a
// tool's configured percentages -- shared by Check (which uses it to decide
// cross-role weighting) and Wait (which uses it to pick the right per-role
// FIFO queue), so the two can never disagree about which group a caller
// belongs to.
type admissionClass struct {
	role string  // bestRole, only meaningful when pct > 0
	pct  float64 // bestPct; 0 means the caller has no configured role here
	key  string  // canonicalRoleKey(roles); the grouping key for the
	// unconfigured pool, used regardless of pct (harmless to compute even
	// when the caller turns out to be configured)
}

// classify resolves the caller's effective role for tool: the max
// configured percentage among their roles, ties broken deterministically by
// lexicographically-first role name (sorting first, then only replacing on
// strict ">", achieves this -- a later, equal-percent role never displaces
// an earlier one).
func classify(toolCfg ToolConfig, roles []string) admissionClass {
	sortedRoles := append([]string(nil), roles...)
	sort.Strings(sortedRoles)

	bestRole, bestPct := "", 0.0
	for _, r := range sortedRoles {
		if pct, ok := toolCfg.RolePercent[r]; ok && pct > bestPct {
			bestRole, bestPct = r, pct
		}
	}
	return admissionClass{role: bestRole, pct: bestPct, key: canonicalRoleKey(sortedRoles)}
}

// Check decides whether a call to tool by a caller holding roles should be
// admitted, returning the reason for the decision alongside the verdict.
func (l *Limiter) Check(tool string, roles []string) Decision {
	toolCfg, ok := l.toolConfig(tool)
	if !ok || toolCfg.GlobalPerMinute == nil {
		return Decision{Allowed: true, Reason: ReasonUnlimited}
	}
	g := float64(*toolCfg.GlobalPerMinute)

	sumConfiguredPct := 0.0
	for _, pct := range toolCfg.RolePercent {
		sumConfiguredPct += pct
	}
	leftoverPct := 100 - sumConfiguredPct
	gConfigured := sumConfiguredPct / 100 * g
	gUnconfigured := leftoverPct / 100 * g

	class := classify(toolCfg, roles)
	bestRole, bestPct := class.role, class.pct
	unconfiguredKey := class.key

	st := l.stateFor(tool)
	st.mu.Lock()
	defer st.mu.Unlock()

	now := l.now()
	st.refill(now, g, gConfigured, toolCfg.RolePercent, gUnconfigured)

	if st.totalRate+1 <= g {
		st.totalRate++
		if bestPct > 0 {
			st.configuredRoleRate[bestRole]++
		} else {
			st.unconfiguredRoleRate[unconfiguredKey]++
		}
		return Decision{Allowed: true, Reason: ReasonBelowGlobal}
	}

	// Contended: the two pools below are strictly separate and never borrow
	// from each other.
	if bestPct > 0 {
		active := 0.0
		for role, pct := range toolCfg.RolePercent {
			if role == bestRole || st.configuredRoleRate[role] > 0 {
				active += pct
			}
		}
		share := (bestPct / active) * gConfigured
		if st.configuredRoleRate[bestRole] < share {
			st.configuredRoleRate[bestRole]++
			st.totalRate++
			return Decision{Allowed: true, Reason: ReasonConfiguredPool}
		}
		return Decision{Allowed: false, Reason: ReasonConfiguredPoolExhausted}
	}

	if gUnconfigured <= 0 {
		return Decision{Allowed: false, Reason: ReasonUnconfiguredPoolExhausted}
	}
	activeCount := 0
	currentIncluded := false
	for key, rate := range st.unconfiguredRoleRate {
		if rate > 0 {
			activeCount++
			if key == unconfiguredKey {
				currentIncluded = true
			}
		}
	}
	if !currentIncluded {
		activeCount++
	}
	share := gUnconfigured / float64(activeCount)
	if st.unconfiguredRoleRate[unconfiguredKey] < share {
		st.unconfiguredRoleRate[unconfiguredKey]++
		st.totalRate++
		return Decision{Allowed: true, Reason: ReasonUnconfiguredPool}
	}
	return Decision{Allowed: false, Reason: ReasonUnconfiguredPoolExhausted}
}

// Wait blocks until a call to tool by a caller holding roles is admitted, or
// ctx is done, whichever comes first. Unlike Check (a single, instantaneous
// decision with no ordering guarantee among simultaneous same-role callers
// -- see the ticketQueue doc comment), Wait guarantees strict FIFO order
// among callers competing for the same (tool, role) share: the
// longest-waiting caller in a group is always the next one admitted once
// that group's share reopens, regardless of how the other callers' poll
// timing happens to line up. Cross-role weighting (Check's own math) is
// completely unaffected -- Wait only arbitrates ordering *within* one
// role's queue, never between roles.
func (l *Limiter) Wait(ctx context.Context, tool string, roles []string) Decision {
	toolCfg, ok := l.toolConfig(tool)
	if !ok || toolCfg.GlobalPerMinute == nil {
		return Decision{Allowed: true, Reason: ReasonUnlimited}
	}

	class := classify(toolCfg, roles)
	configured := class.pct > 0
	key := class.key
	if configured {
		key = class.role
	}

	queue := l.stateFor(tool).queueFor(configured, key)
	turn, leave := queue.enqueue()
	defer leave()

	select {
	case <-turn:
	case <-ctx.Done():
		return Decision{Allowed: false, Reason: ReasonQueueTimeout}
	}

	// Now at the front of this role's queue: only this caller retries until
	// admitted or ctx is done. Nobody behind it in the queue can be checked
	// until it either succeeds or leave() (deferred) promotes the next one.
	for {
		d := l.Check(tool, roles)
		if d.Allowed {
			return d
		}
		select {
		case <-ctx.Done():
			return d
		case <-time.After(l.pollInterval):
		}
	}
}

func (l *Limiter) toolConfig(tool string) (ToolConfig, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.cfg == nil {
		return ToolConfig{}, false
	}
	tc, ok := l.cfg.Tools[tool]
	return tc, ok
}

func (l *Limiter) stateFor(tool string) *toolState {
	l.statesMu.Lock()
	defer l.statesMu.Unlock()
	st, ok := l.states[tool]
	if !ok {
		st = &toolState{
			configuredRoleRate:   make(map[string]float64),
			unconfiguredRoleRate: make(map[string]float64),
			configuredQueues:     make(map[string]*ticketQueue),
			unconfiguredQueues:   make(map[string]*ticketQueue),
		}
		l.states[tool] = st
	}
	return st
}

// toolState holds the live, continuously-decaying usage estimates for one
// tool: an aggregate rate capped conceptually at G, plus one estimate per
// configured role and one per distinct currently-active unconfigured-role
// grouping. queuesMu guards the two queue maps independently of mu (which
// guards only the decaying-rate maps) so a caller blocked waiting for its
// turn in a queue never holds a lock that Check needs.
type toolState struct {
	mu sync.Mutex

	lastUpdate time.Time
	totalRate  float64

	configuredRoleRate   map[string]float64
	unconfiguredRoleRate map[string]float64

	queuesMu           sync.Mutex
	configuredQueues   map[string]*ticketQueue
	unconfiguredQueues map[string]*ticketQueue
}

// queueFor returns the FIFO ticketQueue for the given role/key, creating it
// on first use. configured selects which of the two independent queue
// namespaces (configured-role vs. unconfigured-pool) key belongs to -- the
// same partition Check itself uses, so a queue never mixes callers from the
// two strictly-separate pools.
func (s *toolState) queueFor(configured bool, key string) *ticketQueue {
	s.queuesMu.Lock()
	defer s.queuesMu.Unlock()
	m := s.unconfiguredQueues
	if configured {
		m = s.configuredQueues
	}
	q, ok := m[key]
	if !ok {
		q = &ticketQueue{}
		m[key] = q
	}
	return q
}

// refill decays every tracked rate estimate by the time elapsed since the
// last call on this tool, then prunes unconfigured entries that have fully
// decayed away. Pruning matters only for the unconfigured map: the set of
// possible role combinations is unbounded, unlike the finite,
// config-declared set of configured role names, so leaving fully-decayed
// entries in place would grow the map without bound over time.
func (s *toolState) refill(now time.Time, g, gConfigured float64, rolePercent map[string]float64, gUnconfigured float64) {
	if s.lastUpdate.IsZero() {
		s.lastUpdate = now
		return
	}
	elapsed := now.Sub(s.lastUpdate).Seconds()
	if elapsed <= 0 {
		return // clock hasn't advanced (or went backwards): nothing to decay
	}
	s.lastUpdate = now

	s.totalRate = decay(s.totalRate, g, elapsed)
	for role, pct := range rolePercent {
		own := pct / 100 * gConfigured
		if cur, ok := s.configuredRoleRate[role]; ok {
			s.configuredRoleRate[role] = decay(cur, own, elapsed)
		}
	}
	for key, rate := range s.unconfiguredRoleRate {
		next := decay(rate, gUnconfigured, elapsed)
		if next <= 0 {
			delete(s.unconfiguredRoleRate, key)
			continue
		}
		s.unconfiguredRoleRate[key] = next
	}
}

// decay reduces value by capacity/60 units per elapsed second (the same
// continuous-refill math golang.org/x/time/rate uses internally), floored at
// zero.
func decay(value, capacity, elapsedSeconds float64) float64 {
	if capacity <= 0 {
		return 0
	}
	v := value - capacity/60*elapsedSeconds
	if v < 0 {
		return 0
	}
	return v
}

// canonicalRoleKey turns a caller's (already-sorted) role set into a single,
// deterministic identity used to group unconfigured callers for the
// equal-split pool: two callers presenting the same set of roles (in any
// order) are grouped together and share one bucket; callers with a different
// role set get their own.
func canonicalRoleKey(sortedRoles []string) string {
	return strings.Join(sortedRoles, ",")
}
