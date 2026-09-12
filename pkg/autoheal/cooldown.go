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

type Breaker struct {
	mu sync.Mutex

	cooldown    time.Duration
	burstWindow time.Duration
	burstLimit  int
	openFor     time.Duration

	lastAttempt map[string]time.Time
	attempts    map[string][]time.Time // recent attempts within burstWindow
	crashes     map[string][]time.Time // recent crash observations within burstWindow (post-restart exited)
	opensAt     map[string]time.Time   // circuit-open-until timestamp
}

func NewBreaker() *Breaker {
	return &Breaker{
		cooldown:    60 * time.Second,
		burstWindow: 10 * time.Minute,
		burstLimit:  5,
		openFor:     5 * time.Minute,
		lastAttempt: make(map[string]time.Time),
		attempts:    make(map[string][]time.Time),
		crashes:     make(map[string][]time.Time),
		opensAt:     make(map[string]time.Time),
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

	window := b.attempts[key]
	cutoff := now.Add(-b.burstWindow)
	out := window[:0]
	for _, t := range window {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	window = append(out, now)
	b.attempts[key] = window

	if err != nil {
		failures := 0
		for _, t := range window {
			_ = t
			failures++
		}
		if failures >= b.burstLimit {
			b.opensAt[key] = now.Add(b.openFor)
			b.attempts[key] = nil
		}
	}
}

// purgeLocked trims attempts that fell out of the burst window.
func (b *Breaker) purgeLocked(key string) {
	now := time.Now()
	cutoff := now.Add(-b.burstWindow)
	window := b.attempts[key]
	out := window[:0]
	for _, t := range window {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	b.attempts[key] = out
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
