package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// Regression: container-stopped incidents used to stay open forever because
// the agent keeps shipping the container in `services` with active=false,
// so the simple "container absent from services" resolution path never
// fired. After abandonedContainerTTL the incident must auto-resolve so the
// fleet status page doesn't sit at "outage" forever for a long-dead container.
func TestAbandonedContainerIncidentAutoResolves(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "abandoned.db"), "", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	now := time.Now()
	hb := mkHeartbeat("node-x", now)
	hb.Services = []protocol.ServiceStatus{
		{Name: "happy_khorana", Type: "docker", Active: false, Status: "Exited (1) 6 seconds ago"},
	}
	s.Ingest(&hb)

	// Right after opening the incident is still active.
	openInc := s.GetActiveIncidents(1)
	if len(openInc) != 1 {
		t.Fatalf("expected 1 open incident right after open, got %d", len(openInc))
	}

	// Simulate the incident having been open for >abandonedContainerTTL by
	// rewinding started_at on the row.
	if _, err := s.db.Exec("UPDATE incidents SET started_at = ? WHERE id = ?",
		now.Add(-2*abandonedContainerTTL).Unix(), openInc[0].ID); err != nil {
		t.Fatalf("rewind started_at: %v", err)
	}

	// Drive another heartbeat with the same stopped container; this triggers
	// ResolveStaleIncidents which must now auto-resolve it.
	hb2 := mkHeartbeat("node-x", now.Add(time.Microsecond))
	hb2.Services = hb.Services
	s.Ingest(&hb2)

	remaining := s.GetActiveIncidents(1)
	if len(remaining) != 0 {
		t.Fatalf("abandoned container incident should have auto-resolved, still open: %+v", remaining)
	}
}

// Counter-test: a freshly opened container-stopped incident must NOT
// auto-resolve on the next heartbeat — only after abandonedContainerTTL.
func TestFreshContainerIncidentStaysOpen(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "fresh.db"), "", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	now := time.Now()
	hb := mkHeartbeat("node-y", now)
	hb.Services = []protocol.ServiceStatus{
		{Name: "fresh_container", Type: "docker", Active: false, Status: "Exited (1)"},
	}
	s.Ingest(&hb)

	hb2 := mkHeartbeat("node-y", now.Add(time.Microsecond))
	hb2.Services = hb.Services
	s.Ingest(&hb2)

	openInc := s.GetActiveIncidents(1)
	if len(openInc) != 1 {
		t.Fatalf("fresh container incident must still be open, got %d open", len(openInc))
	}
}
