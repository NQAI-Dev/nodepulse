package store

import (
	"sync"
	"time"
)

// RateLimiter is an in-memory sliding-window counter per key. Intended for
// brute-force protection on /api/v1/auth/login and /api/v1/auth/register.
//
// Keys with `Limit` hits inside `Window` are denied until the oldest hit
// ages out. Single-process only — fine for a NodePulse server fleet of
// one box per tenant; for a multi-replica deployment swap for Redis or
// the SQLite hits table.
//
// ponytail: at >10k unique IPs/min or multi-replica deployments, move to
// a shared backend (Redis INCR with EXPIRE, or a hits table in SQLite).
type RateLimiter struct {
	mu       sync.Mutex
	hits     map[string][]time.Time
	Limit    int
	Window   time.Duration
	SweepEv  int // evict entries with no hits inside N windows; default = 3
	lastSweep time.Time
}

func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{
		hits:     make(map[string][]time.Time),
		Limit:    limit,
		Window:   window,
		SweepEv:  3,
		lastSweep: time.Now(),
	}
}

// Allow returns true when the call should proceed. False means the caller
// has hit the limit inside the current window.
func (r *RateLimiter) Allow(key string) bool {
	if r == nil || r.Limit <= 0 {
		return true
	}
	now := time.Now()
	cutoff := now.Add(-r.Window)

	r.mu.Lock()
	defer r.mu.Unlock()

	if now.Sub(r.lastSweep) > r.Window {
		evictCutoff := now.Add(-r.Window * time.Duration(r.SweepEv))
		for k, v := range r.hits {
			if len(v) == 0 || v[len(v)-1].Before(evictCutoff) {
				delete(r.hits, k)
			}
		}
		r.lastSweep = now
	}

	arr := r.hits[key]
	keep := arr[:0]
	for _, t := range arr {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	if len(keep) >= r.Limit {
		r.hits[key] = keep
		return false
	}
	keep = append(keep, now)
	r.hits[key] = keep
	return true
}

// Reset wipes a single key — useful after a successful login so a legit
// user doesn't carry their failed-attempt tail into the next hour.
func (r *RateLimiter) Reset(key string) {
	r.mu.Lock()
	delete(r.hits, key)
	r.mu.Unlock()
}
