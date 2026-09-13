package autoheal

import (
	"testing"
	"time"
)

func TestKeyNormalization(t *testing.T) {
	if Key("  restart_docker:valkey  ") != "restart_docker:valkey" {
		t.Fatalf("trim failed: %q", Key("  restart_docker:valkey  "))
	}
	if Key("restart_docker:foo\nbar") != "restart_docker:foo bar" {
		t.Fatalf("control char not stripped: %q", Key("restart_docker:foo\nbar"))
	}
	if Key("restart_systemd:nginx\t web") != "restart_systemd:nginx  web" {
		t.Fatalf("tab handling wrong: %q", Key("restart_systemd:nginx\t web"))
	}
}

func TestCooldownGatesWithin(t *testing.T) {
	b := NewBreaker()
	if !b.Allow("restart_docker:web").Allowed {
		t.Fatalf("first call must be allowed")
	}
	if got := b.Allow("restart_docker:web"); got.Allowed {
		t.Fatalf("second call within cooldown must be blocked, got %+v", got)
	} else if got.Reason != "cooldown" {
		t.Fatalf("expected cooldown reason, got %q", got.Reason)
	}
}

func TestCooldownExpiresAfter(t *testing.T) {
	b := NewBreaker()
	b.cooldown = 20 * time.Millisecond
	key := "restart_docker:alpha"
	if !b.Allow(key).Allowed {
		t.Fatalf("first call")
	}
	b.Record(key, nil)
	time.Sleep(40 * time.Millisecond)
	if !b.Allow(key).Allowed {
		t.Fatalf("after cooldown the call must be allowed again")
	}
}

func TestCircuitBreakerOpensAfterRepeatedFailures(t *testing.T) {
	b := NewBreaker()
	b.burstLimit = 3
	b.openFor = 200 * time.Millisecond
	b.cooldown = 0 // let the breaker logic dominate; no cooldown gate
	key := "restart_docker:looper"

	for i := 0; i < 3; i++ {
		if !b.Allow(key).Allowed {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
		b.Record(key, errStub("boom"))
	}

	got := b.Allow(key)
	if got.Allowed {
		t.Fatalf("circuit should be open after 3 failures")
	}
	if got.Reason != "circuit_open" {
		t.Fatalf("expected circuit_open, got %q", got.Reason)
	}
	if got.RetryAfter <= 0 {
		t.Fatalf("retry-after must be positive: %v", got.RetryAfter)
	}

	time.Sleep(220 * time.Millisecond)
	if !b.Allow(key).Allowed {
		t.Fatalf("breaker should close after cool-down window")
	}
}

func TestSuccessDoesNotTripBreaker(t *testing.T) {
	b := NewBreaker()
	b.burstLimit = 3
	key := "restart_docker:happy"
	for i := 0; i < 5; i++ {
		// Reset cooldown artificially so each Allow passes.
		b.mu.Lock()
		delete(b.lastAttempt, key)
		b.mu.Unlock()

		if !b.Allow(key).Allowed {
			t.Fatalf("call %d must be allowed", i+1)
		}
		b.Record(key, nil)
	}

	if len(b.opensAt) != 0 {
		t.Fatalf("breaker should remain closed for successes, opensAt=%v", b.opensAt)
	}
}

func TestRecordCrashOpensBreakerAfterRepeatedCrashes(t *testing.T) {
	b := NewBreaker()
	b.burstLimit = 3
	b.openFor = 200 * time.Millisecond
	b.cooldown = 0 // let the breaker logic dominate; no cooldown gate
	key := "restart_docker:crashloop"

	for i := 0; i < 3; i++ {
		if !b.Allow(key).Allowed {
			t.Fatalf("attempt %d must be allowed (cooldown gate off)", i+1)
		}
		b.Record(key, nil) // docker start succeeded
		b.RecordCrash(key) // but the container exited again
	}

	got := b.Allow(key)
	if got.Allowed {
		t.Fatalf("breaker should open after %d crash observations", 3)
	}
	if got.Reason != "circuit_open" {
		t.Fatalf("expected circuit_open, got %q", got.Reason)
	}
}

func TestMixedSuccessFailureBreakerIgnoresSuccesses(t *testing.T) {
	// Regression: a healthy container that occasionally fails should NOT
	// trip the breaker. Only failures count toward burstLimit; successes
	// only reset cooldown. Previously the breaker counted every attempt
	// (success + failure), which tripped it on noisy healthy hosts.
	b := NewBreaker()
	b.burstLimit = 3
	b.cooldown = 0
	key := "restart_docker:flappy"

	// 5 successes interleaved with 2 failures (under threshold).
	for i := 0; i < 5; i++ {
		b.Record(key, nil)
	}
	b.Record(key, errStub("boom1"))
	b.Record(key, nil)
	b.Record(key, nil)
	b.Record(key, errStub("boom2"))

	if len(b.opensAt) != 0 {
		t.Fatalf("breaker must stay closed for 2 failures + many successes, opensAt=%v", b.opensAt)
	}

	// Two more failures cross the threshold and open the breaker.
	b.Record(key, errStub("boom3"))
	b.Record(key, errStub("boom4"))
	got := b.Allow(key)
	if got.Allowed {
		t.Fatalf("breaker should open after 4 failures within window, got %+v", got)
	}
	if got.Reason != "circuit_open" {
		t.Fatalf("expected circuit_open, got %q", got.Reason)
	}
}

func TestRecordCrashRespectsCooldownGate(t *testing.T) {
	b := NewBreaker()
	b.burstLimit = 3
	key := "restart_docker:quiet"
	b.RecordCrash(key)
	b.RecordCrash(key) // still under threshold

	// Cooldown gate must still trip on Allow.
	if !b.Allow(key).Allowed {
		t.Fatalf("first Allow after crashes must pass cooldown")
	}
	if b.Allow(key).Allowed {
		t.Fatalf("second Allow within cooldown must be blocked")
	}
}

type stubErr string

func (e stubErr) Error() string { return string(e) }

func errStub(s string) error { return stubErr(s) }
