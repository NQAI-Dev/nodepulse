package store

import (
	"path/filepath"
	"testing"
	"time"
)

// TestIncidentThrottleWithinCooldown ensures repeated CreateIncident calls for
// the same (node, title) inside the cooldown window do not produce duplicate
// rows. With botToken="" + tg chatID=0 the dispatcher is a no-op so we assert
// against the database directly.
func TestIncidentThrottleWithinCooldown(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	s, err := NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("init store: %v", err)
	}

	for i := 0; i < 5; i++ {
		_ = s.CreateIncident("node-throttle", "warning", "High Memory Pressure", "98.2% RAM")
	}

	inc := s.GetActiveIncidents(1)
	if len(inc) != 1 {
		t.Fatalf("expected exactly 1 active incident, got %d", len(inc))
	}
	if inc[0].Detail != "98.2% RAM" {
		t.Fatalf("expected detail to track the latest snapshot, got %q", inc[0].Detail)
	}
}

// TestIncidentThrottleReFiresAfterCooldown simulates the cooldown expiring by
// rewinding last_notified_at directly through SQL. We don't shrink the
// cooldown constant because that's a behavior guarantee we want to hold.
func TestIncidentThrottleReFiresAfterCooldown(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	s, err := NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("init store: %v", err)
	}

	_ = s.CreateIncident("node-x", "critical", "Container Stopped: web", "exited")

	// Force last_notified_at deep into the past so the next call passes the
	// cooldown gate. We can update only the existing row, no second row.
	old := time.Now().Add(-2 * cooldownFor("critical")).Unix()
	if _, err := s.db.Exec("UPDATE incidents SET last_notified_at = ? WHERE node_id = ?", old, "node-x"); err != nil {
		t.Fatalf("rewind last_notified_at: %v", err)
	}

	// New crossing detail -- re-fires:
	_ = s.CreateIncident("node-x", "critical", "Container Stopped: web", "exited again")

	inc := s.GetActiveIncidents(1)
	if len(inc) != 1 {
		t.Fatalf("expected 1 incident (collapsed), got %d", len(inc))
	}
	if inc[0].Detail != "exited again" {
		t.Fatalf("expected detail to update on re-fire, got %q", inc[0].Detail)
	}
}

// TestIncidentResolveAllowsRefire closes the loop: a recovered incident must
// re-arm CreateIncident so future crossings create fresh records instead of
// being absorbed by the already-resolved row.
func TestIncidentResolveAllowsRefire(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	s, err := NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("init store: %v", err)
	}

	_ = s.CreateIncident("node-y", "warning", "High Memory Pressure", "96.0% RAM")
	if len(s.GetActiveIncidents(1)) != 1 {
		t.Fatalf("setup: expected 1 active")
	}

	inc := s.GetActiveIncidents(1)[0]
	if err := s.ResolveIncident(inc.ID, 1); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(s.GetActiveIncidents(1)) != 0 {
		t.Fatalf("expected 0 active after resolve")
	}

	_ = s.CreateIncident("node-y", "warning", "High Memory Pressure", "97.0% RAM")
	if len(s.GetActiveIncidents(1)) != 1 {
		t.Fatalf("expected a fresh incident after resolve+new crossing, got %d", len(s.GetActiveIncidents(1)))
	}
}
