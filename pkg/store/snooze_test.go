package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func newSnoozeTestStore(t *testing.T) *PersistentStore {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	p, err := NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}
	t.Cleanup(func() { p.db.Close() })
	return p
}

func TestSnoozeIncident_Basic(t *testing.T) {
	p := newSnoozeTestStore(t)
	_ = p.CreateIncident("node-a", "warning", "Container Stopped: web", "")

	id := mustOpenIncidentID(t, p, "node-a")
	if id == "" {
		t.Fatalf("expected an open incident to be created")
	}

	until, err := p.SnoozeIncident(id, 3600)
	if err != nil {
		t.Fatalf("SnoozeIncident: %v", err)
	}
	if until <= time.Now().Unix() {
		t.Fatalf("snoozed_until should be in the future, got %d (now=%d)", until, time.Now().Unix())
	}
	if got := p.IncidentSnoozedUntil(id); got != until {
		t.Fatalf("IncidentSnoozedUntil mismatch: got %d want %d", got, until)
	}
}

func TestSnoozeIncident_RejectsBadDuration(t *testing.T) {
	p := newSnoozeTestStore(t)
	_ = p.CreateIncident("node-a", "warning", "Container Stopped: web", "")
	id := mustOpenIncidentID(t, p, "node-a")

	for _, bad := range []int64{0, -1, 25 * 3600, 7 * 24 * 3600} {
		if _, err := p.SnoozeIncident(id, bad); err != ErrSnoozeDurationInvalid {
			t.Fatalf("seconds=%d expected ErrSnoozeDurationInvalid, got %v", bad, err)
		}
	}
}

func TestSnoozeIncident_UnknownOrResolved(t *testing.T) {
	p := newSnoozeTestStore(t)
	_ = p.CreateIncident("node-a", "warning", "Container Stopped: web", "")
	id := mustOpenIncidentID(t, p, "node-a")

	if _, err := p.SnoozeIncident("99999", 3600); err != ErrSnoozeIncidentNotFound {
		t.Fatalf("unknown id: expected ErrSnoozeIncidentNotFound, got %v", err)
	}

	// Resolve it, then try snoozing again.
	if err := p.ResolveIncident(id, 1); err != nil {
		t.Fatalf("ResolveIncident: %v", err)
	}
	if _, err := p.SnoozeIncident(id, 3600); err != ErrSnoozeIncidentNotFound {
		t.Fatalf("resolved id: expected ErrSnoozeIncidentNotFound, got %v", err)
	}
}

func TestSnoozeIncident_SuppressesReNotify(t *testing.T) {
	// Regression: when snoozed_until is in the future, CreateIncident must
	// NOT re-notify even if the cooldown has expired.
	p := newSnoozeTestStore(t)
	rec := &alerterRecordingNotifier{}
	p.SetNotifier(rec)

	_ = p.CreateIncident("node-a", "critical", "Container Stopped: web", "")
	if len(rec.Incidents) != 1 {
		t.Fatalf("first CreateIncident must notify, got %d", len(rec.Incidents))
	}
	id := mustOpenIncidentID(t, p, "node-a")

	if _, err := p.SnoozeIncident(id, 4*3600); err != nil {
		t.Fatalf("SnoozeIncident: %v", err)
	}

	// Re-fire: cooldown for critical is 60s, but snooze must override it.
	_ = p.CreateIncident("node-a", "critical", "Container Stopped: web", "stale detail")
	if len(rec.Incidents) != 1 {
		t.Fatalf("snoozed CreateIncident must NOT re-notify, got %d calls", len(rec.Incidents))
	}
}

func TestSnoozeIncident_ExpiresAndReNotifies(t *testing.T) {
	// After snoozed_until is in the past, the snooze gate opens but the
	// cooldown gate still applies (SnoozeIncident bumps last_notified_at
	// to suppress the immediate re-notify). Re-notifications resume only
	// after BOTH gates release.
	p := newSnoozeTestStore(t)
	rec := &alerterRecordingNotifier{}
	p.SetNotifier(rec)

	_ = p.CreateIncident("node-a", "critical", "Container Stopped: web", "")
	id := mustOpenIncidentID(t, p, "node-a")

	if _, err := p.SnoozeIncident(id, 1); err != nil {
		t.Fatalf("SnoozeIncident: %v", err)
	}
	// Force-expire the snooze AND clear last_notified_at so the cooldown
	// gate releases at the same time — that mimics the post-snooze steady
	// state once the cooldown window has elapsed.
	if _, err := p.db.Exec(
		"UPDATE incidents SET snoozed_until = ?, last_notified_at = 0 WHERE id = ?",
		time.Now().Unix()-1, id,
	); err != nil {
		t.Fatalf("expire snooze: %v", err)
	}

	_ = p.CreateIncident("node-a", "critical", "Container Stopped: web", "after snooze")
	if len(rec.Incidents) != 2 {
		t.Fatalf("expected re-notify after snooze expired, got %d", len(rec.Incidents))
	}
}

// mustOpenIncidentID returns the id (base-10 string) of the single open
// incident for nodeID, or "" if none. Fails the test when >1 row is found —
// the storage layer should never produce duplicate open rows for the same
// (node, title), so a duplicate here means a regression in CreateIncident.
func mustOpenIncidentID(t *testing.T, p *PersistentStore, nodeID string) string {
	t.Helper()
	incs := p.GetActiveIncidents(1) // userID=1 sees everything (admin)
	var found []string
	for _, inc := range incs {
		if inc.NodeID == nodeID {
			found = append(found, inc.ID)
		}
	}
	if len(found) == 0 {
		return ""
	}
	if len(found) > 1 {
		t.Fatalf("expected ≤1 open incident for %s, got %d", nodeID, len(found))
	}
	return found[0]
}

// Compile-time guard: keep protocol imported so the test compiles even when
// no other type from the package is referenced.
var _ = protocol.Heartbeat{}
