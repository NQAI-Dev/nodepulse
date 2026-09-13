package policy

import (
	"testing"
	"time"
)

func TestClassifyByImage(t *testing.T) {
	cases := []struct {
		image string
		want  Class
	}{
		{"postgres:16-alpine", ClassDB},
		{"postgres:latest", ClassDB},
		{"mariadb:11", ClassDB},
		{"mysql:8.0", ClassDB},
		{"mongo:7", ClassDB},
		{"redis:7-alpine", ClassCache},
		{"valkey/valkey:7", ClassCache},
		{"memcached:1.6", ClassCache},
		{"nginx:1.27", ClassStateless},
		{"traefik:v3.0", ClassStateless},
		{"ghcr.io/me/myapp:v1.2.3", ClassDefault},
		{"", ClassDefault},
	}
	for _, c := range cases {
		got := Classify(c.image, "container-1", nil)
		if got != c.want {
			t.Errorf("image=%q: got %s, want %s", c.image, got, c.want)
		}
	}
}

func TestClassifyByNameFallback(t *testing.T) {
	// No image hint, but the name carries the signal.
	got := Classify("ghcr.io/me/custom-app:v1", "myapp-postgres-1", nil)
	if got != ClassDB {
		t.Fatalf("name fallback: got %s, want %s", got, ClassDB)
	}
	got = Classify("ghcr.io/me/custom-app:v1", "redis-cache-7", nil)
	if got != ClassCache {
		t.Fatalf("name fallback: got %s, want %s", got, ClassCache)
	}
}

func TestClassifyLabelOverrides(t *testing.T) {
	// Image says stateless, label says critical: label wins.
	labels := map[string]string{"policy.nodepulse/class": "critical"}
	got := Classify("nginx:1.27", "web-1", labels)
	if got != ClassCritical {
		t.Fatalf("label override failed: got %s", got)
	}

	// Unknown label value falls through to heuristic.
	labels = map[string]string{"policy.nodepulse/class": "exotic"}
	got = Classify("postgres:16", "db-1", labels)
	if got != ClassDB {
		t.Fatalf("unknown label should fall through to heuristic: got %s", got)
	}

	// Garbage value with default-class image still falls through to default.
	labels = map[string]string{"policy.nodepulse/class": "exotic"}
	got = Classify("ghcr.io/me/custom-app:v1", "worker-1", labels)
	if got != ClassDefault {
		t.Fatalf("unknown label with no heuristic: got %s, want %s", got, ClassDefault)
	}
}

func TestStrategyForUnknown(t *testing.T) {
	s := StrategyFor(Class("nope"))
	if s.Class != ClassDefault {
		t.Fatalf("unknown class should fall back to default, got %s", s.Class)
	}
}

func TestNextDelayExponential(t *testing.T) {
	s := StrategyFor(ClassStateless) // Cooldown = 15s
	cases := []struct {
		n    int
		want time.Duration
	}{
		{0, 0},
		{1, 15 * time.Second},
		{2, 30 * time.Second},
		{3, 60 * time.Second},
		{4, 120 * time.Second},
		{5, 120 * time.Second}, // capped
		{99, 120 * time.Second},
	}
	for _, c := range cases {
		got := NextDelay(s, c.n)
		if got != c.want {
			t.Errorf("n=%d: got %s, want %s", c.n, got, c.want)
		}
	}
}

func TestShouldEscalate(t *testing.T) {
	db := StrategyFor(ClassDB) // EscalateAfter=2
	if ShouldEscalate(db, 1) {
		t.Fatalf("DB should not escalate at 1 failure")
	}
	if !ShouldEscalate(db, 2) {
		t.Fatalf("DB should escalate at 2 failures")
	}
	if !ShouldEscalate(db, 5) {
		t.Fatalf("DB should still escalate at 5 failures")
	}

	stateless := StrategyFor(ClassStateless) // EscalateAfter=6
	if ShouldEscalate(stateless, 3) {
		t.Fatalf("stateless should not have escalated yet at 3 failures")
	}
	if !ShouldEscalate(stateless, 6) {
		t.Fatalf("stateless should escalate at 6 failures")
	}
}

func TestStrategyDBCooldownIsHighFloor(t *testing.T) {
	// Ponytail guard: a misconfigured DB cooldown would silently cause
	// WAL thrash. Pin the floor.
	db := StrategyFor(ClassDB)
	if db.Cooldown < 60*time.Second {
		t.Fatalf("DB cooldown must be >= 60s, got %s", db.Cooldown)
	}
	if db.BurstLimit != 3 {
		t.Fatalf("DB burst limit must be 3 (WAL replay cost), got %d", db.BurstLimit)
	}
}
