package store

import (
	"testing"
	"time"
)

func TestRateLimiter_AllowsUpToLimit(t *testing.T) {
	rl := NewRateLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !rl.Allow("ip:1.2.3.4") {
			t.Fatalf("call %d should be allowed", i+1)
		}
	}
	if rl.Allow("ip:1.2.3.4") {
		t.Fatal("4th call inside window must be denied")
	}
}

func TestRateLimiter_KeysAreIndependent(t *testing.T) {
	rl := NewRateLimiter(2, time.Minute)
	rl.Allow("a")
	rl.Allow("a")
	if rl.Allow("a") {
		t.Fatal("a must be locked")
	}
	if !rl.Allow("b") {
		t.Fatal("b should still be allowed")
	}
}

func TestRateLimiter_WindowExpiryUnblocks(t *testing.T) {
	rl := NewRateLimiter(1, 30*time.Millisecond)
	if !rl.Allow("k") {
		t.Fatal("first call allowed")
	}
	if rl.Allow("k") {
		t.Fatal("second call denied")
	}
	time.Sleep(60 * time.Millisecond)
	if !rl.Allow("k") {
		t.Fatal("after window expiry key must reset")
	}
}

func TestRateLimiter_ResetClearsKey(t *testing.T) {
	rl := NewRateLimiter(1, time.Minute)
	rl.Allow("k")
	if rl.Allow("k") {
		t.Fatal("locked before reset")
	}
	rl.Reset("k")
	if !rl.Allow("k") {
		t.Fatal("after reset must allow again")
	}
}

func TestRateLimiter_NilSafe(t *testing.T) {
	var rl *RateLimiter
	if !rl.Allow("anything") {
		t.Fatal("nil limiter must allow all calls")
	}
}
