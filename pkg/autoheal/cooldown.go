// Package autoheal provides a per-target cooldown and circuit breaker that
// gates remediation actions on the agent. Without it, a flapping container
// or service triggers a restart every heartbeat (~10s) and we hammer the
// underlying runtime.
//
// Design:
//   - Per-target (the action:target string) cooldown: minimum interval
//     between two attempts. Default 60s.
//   - Per-target circuit breaker: when N attempts happen inside W minutes
//     and they all fail (or repeat), the breaker opens and subsequent
//     attempts are short-circuited for a cool-down window. Default N=5,
//     W=10m, open for 5m.
//
// The breaker is keyed by action:target but its parameters (cooldown,
// burst limit, open window) come from a per-class Strategy supplied at
// construction. The autoheal pipeline picks the strategy by classifying
// the target (db / cache / stateless / default / critical) via the
// policy package. A pure-Default Breaker (NewBreaker) keeps the legacy
// behaviour for callers that haven't migrated.
//
// ponytail: limit of N attempts in W minutes follows a classic N-strikes
// model. Replace with rolling-window EMR-based detection if/when false
// positives from multi-attempt boot loops (where several restarts followed
// by green is expected) become operationally annoying.
package autoheal

import (
	"strings"
	"sync"
	"time"
)

// Strategy describes the per-class breaker parameters. It is intentionally
// a tiny mirror of policy.Strategy — keeping the breaker package free of
// a dependency on policy/ — so unit tests can build Strategies inline
// without importing the classifier.
type Strategy struct {
	Cooldown    time.Duration
	BurstLimit  int
	BurstWindow time.Duration
	OpenFor     time.Duration
}

// DefaultStrategy reproduces the original NewBreaker behaviour: 60s
// cooldown, 5 failures inside 10m opens the circuit for 5m.
var DefaultStrategy = Strategy{
	Cooldown:    60 * time.Second,
	BurstLimit:  5,
	BurstWindow: 10 * time.Minute,
	OpenFor:     5 * time.Minute,
}

type Breaker struct {
	mu sync.Mutex

	cooldown    time.Duration
	burstWindow time.Duration
	burstLimit  int
	openFor     time.Duration

	lastAttempt map[string]time.Time
	attempts    map[string][]time.Time // recent attempts within burstWindow
	failures    map[string][]time.Time // recent failures within burstWindow
	crashes     map[string][]time.Time // recent crash observations within burstWindow (post-restart exited)
	opensAt     map[string]time.Time   // circuit-open-until timestamp
}

// NewBreaker returns a breaker with the legacy default parameters
// (60s/5/10m/5m). Use NewBreakerWithStrategy to drive the breaker from a
// classifier output.
func NewBreaker() *Breaker {
	return NewBreakerWithStrategy(DefaultStrategy)
}

// NewBreakerWithStrategy returns a breaker configured by s. A zero-value
// Strategy falls back to DefaultStrategy so callers can wire a config
// struct straight through without nil-guarding.
func NewBreakerWithStrategy(s Strategy) *Breaker {
	if s.Cooldown <= 0 || s.BurstLimit <= 0 || s.BurstWindow <= 0 || s.OpenFor <= 0 {
		s = DefaultStrategy
	}
	return &Breaker{
		cooldown:    s.Cooldown,
		burstWindow: s.BurstWindow,
		burstLimit:  s.BurstLimit,
		openFor:     s.OpenFor,
		lastAttempt: make(map[string]time.Time),
		attempts:    make(map[string][]time.Time),
		failures:    make(map[string][]time.Time),
		crashes:     make(map[string][]time.Time),
		opensAt:     make(map[string]time.Time),
	}
}

// Strategy returns the strategy parameters the breaker was constructed
// with. It is a copy — mutating it does not change the breaker's
// behaviour. Useful for telemetry surfaces that report the active floor.
func (b *Breaker) Strategy() Strategy {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Strategy{
		Cooldown:    b.cooldown,
		BurstLimit:  b.burstLimit,
		BurstWindow: b.burstWindow,
		OpenFor:     b.openFor,
	}
}

// Result describes the decision of Allow().
type Result struct {
	Allowed    bool      // whether to execute the command now
	Target     string    // normalized target key (action:target)
	Reason     string    // human-readable explanation
	OpenedAt   time.Time // when the breaker opened (zero if not open)
	RetryAfter time.Duration
}

// Allow consults the breaker for `key` and reports whether the caller may
// proceed. The caller MUST call Record() after the action runs to feed the
// outcome into the breaker.
func (b *Breaker) Allow(key string) Result {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.purgeLocked(key)

	now := time.Now()

	if until, ok := b.opensAt[key]; ok {
		if now.Before(until) {
			return Result{
				Allowed:    false,
				Target:     key,
				Reason:     "circuit_open",
				OpenedAt:   until.Add(-b.openFor),
				RetryAfter: time.Until(until),
			}
		}
		delete(b.opensAt, key)
	}

	if last, ok := b.lastAttempt[key]; ok {
		if remaining := b.cooldown - now.Sub(last); remaining > 0 {
			return Result{
				Allowed:    false,
				Target:     key,
				Reason:     "cooldown",
				RetryAfter: remaining,
			}
		}
	}

	b.lastAttempt[key] = now
	return Result{
		Allowed: true,
		Target:  key,
		Reason:  "ok",
	}
}

// RecordCrash feeds a post-restart observation that the target is still
// down (e.g. Docker container exited again within seconds of `docker
// restart`). Distinct from Record() because the restart itself usually
// succeeds — only the post-check reveals the crash-loop. Each crash counts
// toward burstLimit in its own window; at the threshold the breaker opens
// immediately.
func (b *Breaker) RecordCrash(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()

	window := b.crashes[key]
	cutoff := now.Add(-b.burstWindow)
	out := window[:0]
	for _, t := range window {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	window = append(out, now)
	b.crashes[key] = window

	if len(window) >= b.burstLimit {
		b.opensAt[key] = now.Add(b.openFor)
		b.crashes[key] = nil
	}
}

// Record feeds the outcome (success: err == nil) into the breaker. Failures
// count toward the burst-limit; successes are still recorded as attempts so
// the cooldown gate works.
func (b *Breaker) Record(key string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.lastAttempt[key] = now

	failures := b.failures[key]
	if err != nil {
		failures = append(failures, now)
	}
	cutoff := now.Add(-b.burstWindow)
	out := failures[:0]
	for _, t := range failures {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	b.failures[key] = out

	window := b.attempts[key]
	purged := window[:0]
	for _, t := range window {
		if t.After(cutoff) {
			purged = append(purged, t)
		}
	}
	window = append(purged, now)
	b.attempts[key] = window

	if len(out) >= b.burstLimit {
		b.opensAt[key] = now.Add(b.openFor)
		b.attempts[key] = nil
		b.failures[key] = nil
	}
}

// purgeLocked trims attempts that fell out of the burst window.
func (b *Breaker) purgeLocked(key string) {
	now := time.Now()
	cutoff := now.Add(-b.burstWindow)
	purge := func(window []time.Time) []time.Time {
		out := window[:0]
		for _, t := range window {
			if t.After(cutoff) {
				out = append(out, t)
			}
		}
		return out
	}
	window := b.attempts[key]
	out := window[:0]
	for _, t := range window {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	b.attempts[key] = out
	b.failures[key] = purge(b.failures[key])
	b.crashes[key] = purge(b.crashes[key])
}

// Key canonicalizes a command like "restart_docker:foo bar" into a
// stable target key. Spaces are tolerated because container names may
// legitimately contain them; we only collapse control chars.
func Key(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	cmd = strings.ReplaceAll(cmd, "\r", "")
	cmd = strings.ReplaceAll(cmd, "\n", " ")
	cmd = strings.ReplaceAll(cmd, "\t", " ")
	return cmd
}
