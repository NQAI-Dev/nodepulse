package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// ResolveStaleIncidents only fires per heartbeat, so a node that has gone
// silent (no ingest for hours) otherwise leaves its container-stopped
// incident pinned open forever. ResolveAbandonedAcrossFleet must sweep
// such rows based on age alone.
func TestResolveAbandonedAcrossFleet_SilentNodeIncident(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "silent.db"), "", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	now := time.Now()
	hb := mkHeartbeat("silent-node", now)
	hb.Services = []protocol.ServiceStatus{
		{Name: "dead_container", Type: "docker", Active: false, Status: "Exited (1)"},
	}
	s.Ingest(&hb)

	// Rewind the incident past abandonedContainerTTL so the next sweep
	// closes it without us waiting 30 minutes of wall-clock.
	if openInc := s.GetActiveIncidents(1); len(openInc) == 1 {
		if _, err := s.db.Exec("UPDATE incidents SET started_at = ? WHERE id = ?",
			now.Add(-2*abandonedContainerTTL).Unix(), openInc[0].ID); err != nil {
			t.Fatalf("rewind: %v", err)
		}
	} else {
		t.Fatalf("expected 1 open incident, got %d", len(s.GetActiveIncidents(1)))
	}

	n := s.ResolveAbandonedAcrossFleet()
	if n != 1 {
		t.Fatalf("expected 1 abandoned incident auto-resolved, got %d", n)
	}
	if remaining := s.GetActiveIncidents(1); len(remaining) != 0 {
		t.Fatalf("expected no open incidents after sweep, got %+v", remaining)
	}
}

func TestResolveAbandonedAcrossFleet_FreshIncidentUnaffected(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "fresh2.db"), "", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	hb := mkHeartbeat("fresh-node", time.Now())
	hb.Services = []protocol.ServiceStatus{
		{Name: "recently_dead", Type: "docker", Active: false, Status: "Exited (1)"},
	}
	s.Ingest(&hb)

	if n := s.ResolveAbandonedAcrossFleet(); n != 0 {
		t.Fatalf("fresh incident must not be swept, got n=%d", n)
	}
	if remaining := s.GetActiveIncidents(1); len(remaining) != 1 {
		t.Fatalf("fresh incident must stay open, got %d", len(remaining))
	}
}
