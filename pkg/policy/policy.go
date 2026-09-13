// Package policy maps a service (image + name + labels) to a remediation
// strategy. The autoheal breaker used to treat every target identically:
// 60s cooldown, 5 attempts/10m before opening. Real fleets don't behave
// that way — a Postgres pod that flap-restarts can corrupt WAL before
// anyone notices, while a stateless nginx can be hammered half a dozen
// times without drama. Classifying lets us put expensive floors under
// expensive targets and keep cheap ones snappy.
//
// Classification is heuristic. It reads the image (e.g. "postgres:16",
// "redis:7-alpine"), the container/service name, and its Docker labels.
// Anything we can't classify falls back to Default. That keeps false
// positives cheap and explicit: if ops wants a custom rule, they put it
// in labels (policy.nodepulse/class=db) and we honour it without code.
package policy

import (
	"strings"
	"time"
)

// Class is the remediation class of a service. Each class carries its own
// cooldown, attempt budget and escalation behaviour.
type Class string

const (
	ClassDefault  Class = "default"  // unknown / generic process
	ClassStateless Class = "stateless" // nginx, caddy, app servers — cheap restarts
	ClassCache     Class = "cache"     // redis, memcached, keydb — stateful but in-memory
	ClassDB        Class = "db"        // postgres, mysql, mariadb, mongo — durable state
	ClassCritical  Class = "critical"  // operators marked this with a label; never restart blindly
)

// Strategy is the remediation posture for a target. InitialBackoff is
// applied to the autoheal breaker for the first attempt; NextDelay walks
// the curve for subsequent attempts inside the same burst window.
// MaxAttempts is a soft ceiling — the breaker still opens at its own
// circuit limit if a strategy says "try 10 times" but crashes keep coming.
type Strategy struct {
	Class          Class
	Cooldown       time.Duration
	BurstLimit     int           // breaker bursts before circuit opens
	BurstWindow    time.Duration
	OpenFor        time.Duration // how long the circuit stays open once tripped
	MaxAttempts    int           // soft cap across one burst window
	EscalateAfter  int           // after N failures, surface a "human needed" hint
}

// Standard strategies. They are not magic numbers — they reflect how the
// underlying workload reacts to a restart:
//   - Stateless: 15s cooldown. Restart is cheap; quick iteration helps.
//   - Cache: 30s. State is in-memory but rebuilds fast (LUA loads, RDB
//     warm-up). One bad restart is fine; six in a row is operator time.
//   - DB: 120s. WAL replay, fsync, replica catch-up. We want a much
//     longer floor before trying again, and we escalate to a human after
//     two failures because the third will likely be worse.
//   - Critical: 5m cooldown, never auto-restart without an explicit
//     operator ack. The breaker still records the crash; it just blocks
//     execution. This is the safe default for anything tagged sensitive.
var strategies = map[Class]Strategy{
	ClassDefault: {
		Class:         ClassDefault,
		Cooldown:      60 * time.Second,
		BurstLimit:    5,
		BurstWindow:   10 * time.Minute,
		OpenFor:       5 * time.Minute,
		MaxAttempts:   8,
		EscalateAfter: 4,
	},
	ClassStateless: {
		Class:         ClassStateless,
		Cooldown:      15 * time.Second,
		BurstLimit:    6,
		BurstWindow:   10 * time.Minute,
		OpenFor:       3 * time.Minute,
		MaxAttempts:   10,
		EscalateAfter: 6,
	},
	ClassCache: {
		Class:         ClassCache,
		Cooldown:      30 * time.Second,
		BurstLimit:    4,
		BurstWindow:   10 * time.Minute,
		OpenFor:       5 * time.Minute,
		MaxAttempts:   6,
		EscalateAfter: 3,
	},
	ClassDB: {
		Class:         ClassDB,
		Cooldown:      120 * time.Second,
		BurstLimit:    3,
		BurstWindow:   15 * time.Minute,
		OpenFor:       15 * time.Minute,
		MaxAttempts:   4,
		EscalateAfter: 2,
	},
	ClassCritical: {
		Class:         ClassCritical,
		Cooldown:      5 * time.Minute,
		BurstLimit:    2,
		BurstWindow:   30 * time.Minute,
		OpenFor:       30 * time.Minute,
		MaxAttempts:   2,
		EscalateAfter: 1,
	},
}

// imageHints: ordered list of (substring in image, class). First match
// wins. Substrings are lowercase; we lowercase the image before matching.
// We deliberately don't match "mysql" inside "postgres" — substring
// containment is sharp enough for image names.
var imageHints = []struct {
	substr string
	class  Class
}{
	{"postgres", ClassDB},
	{"postgresql", ClassDB},
	{"mariadb", ClassDB},
	{"mysql", ClassDB},
	{"mongo", ClassDB},
	{"cockroach", ClassDB},
	{"tidb", ClassDB},
	{"yugabyte", ClassDB},
	{"clickhouse", ClassDB},
	{"redis", ClassCache},
	{"keydb", ClassCache},
	{"memcached", ClassCache},
	{"dragonfly", ClassCache},
	{"valkey", ClassCache},
	{"nginx", ClassStateless},
	{"caddy", ClassStateless},
	{"traefik", ClassStateless},
	{"envoy", ClassStateless},
	{"haproxy", ClassStateless},
	{"caddy", ClassStateless},
}

// nameHints: container name substrings, applied when the image doesn't
// match. Common naming conventions: "myapp-postgres-1", "cache-redis".
var nameHints = []struct {
	substr string
	class  Class
}{
	{"postgres", ClassDB},
	{"postgresql", ClassDB},
	{"mariadb", ClassDB},
	{"mysql", ClassDB},
	{"mongo", ClassDB},
	{"redis", ClassCache},
	{"memcached", ClassCache},
	{"keydb", ClassCache},
	{"valkey", ClassCache},
	{"nginx", ClassStateless},
	{"caddy", ClassStateless},
	{"traefik", ClassStateless},
}

// Classify inspects the docker image, container name, and labels to pick
// a Class. An explicit label "policy.nodepulse/class=db" always wins —
// that's the escape hatch for operators who disagree with our heuristic.
// Otherwise we walk image hints, then name hints, then fall back.
func Classify(image, name string, labels map[string]string) Class {
	if labels != nil {
		if v := strings.ToLower(strings.TrimSpace(labels["policy.nodepulse/class"])); v != "" {
			switch v {
			case "default", "stateless", "cache", "db", "critical":
				return Class(v)
			}
			// Unknown label value: ignore and fall through to heuristic.
		}
	}

	img := strings.ToLower(image)
	for _, h := range imageHints {
		if strings.Contains(img, h.substr) {
			return h.class
		}
	}
	n := strings.ToLower(name)
	for _, h := range nameHints {
		if strings.Contains(n, h.substr) {
			return h.class
		}
	}
	return ClassDefault
}

// StrategyFor returns the configured Strategy for a class, defaulting
// to Default if the class is unknown. Callers should treat the returned
// value as read-only.
func StrategyFor(class Class) Strategy {
	if s, ok := strategies[class]; ok {
		return s
	}
	return strategies[ClassDefault]
}

// NextDelay returns the recommended delay before the n-th retry (n=1 is
// the first retry, n=0 returns 0). It walks an exponential curve capped
// at 8× the strategy's base cooldown so a DB doesn't get hammered any
// faster than its WAL can settle. The shape is:
//
//	n=1 → Cooldown
//	n=2 → Cooldown * 2
//	n=3 → Cooldown * 4
//	n=4 → Cooldown * 8
//	n≥5 → Cooldown * 8  (capped)
//
// ponytail: pure exponential, no jitter. Add ±20% jitter when several
// targets of the same class flap in lockstep (e.g. a load balancer
// restart takes out 6 pods at once).
func NextDelay(s Strategy, n int) time.Duration {
	if n <= 0 {
		return 0
	}
	if n > 4 {
		return s.Cooldown * 8
	}
	mult := time.Duration(1) << uint(n-1) // 1, 2, 4, 8
	return s.Cooldown * mult
}

// ShouldEscalate reports whether the strategy suggests paging a human
// after the given number of consecutive failures inside the burst window.
// It is a hint, not a stop sign: the breaker still opens the circuit.
// Autoheal uses it to flip an incident's escalation flag so the on-call
// gets a louder notification.
func ShouldEscalate(s Strategy, failures int) bool {
	return failures >= s.EscalateAfter && s.EscalateAfter > 0
}
