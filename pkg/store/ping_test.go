package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// TestPingOpenStore confirms Ping roundtrips on a fresh store. The
// /api/v1/ready handler relies on this succeeding in well under its 2s
// budget; SQLite open + PingContext should complete in microseconds on
// any sane filesystem. We don't pin a specific duration — only that it
// returns nil and finishes before the test's generous deadline.
func TestPingOpenStore(t *testing.T) {
	dbFile := "test_ping.db"
	defer os.Remove(dbFile)

	s, err := NewPersistentStore(dbFile, "", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	if err := s.Ping(ctx); err != nil {
		t.Fatalf("Ping on open store: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 1*time.Second {
		t.Errorf("Ping took %v, expected sub-second on a healthy local SQLite", elapsed)
	}
}

// TestPingAlreadyClosed confirms Ping returns a non-nil error once the
// underlying handle is unusable. The /api/v1/ready handler translates
// any error into 503 — this test pins that contract by closing the
// store first and asserting Ping reports a failure (specific text is
// driver-defined; just checking non-nil).
func TestPingAlreadyClosed(t *testing.T) {
	dbFile := "test_ping_closed.db"
	defer os.Remove(dbFile)

	s, err := NewPersistentStore(dbFile, "", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}
	if err := s.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := s.Ping(ctx); err == nil {
		t.Fatalf("Ping on closed store: expected error, got nil")
	}
}

// TestPingNilDB pins the defensive nil-check. The store constructor
// always populates p.db today, but the field is exported-style and a
// future test helper that bypasses NewPersistentStore could leave it
// nil. Returning a clear error beats a nil-pointer panic during a
// readiness probe.
func TestPingNilDB(t *testing.T) {
	s := &PersistentStore{}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	err := s.Ping(ctx)
	if err == nil {
		t.Fatalf("Ping on nil db: expected error, got nil")
	}
	if !errors.Is(err, err) {
		// sanity: just make sure we got a real error value
		t.Fatalf("Ping returned unexpected: %v", err)
	}
}
