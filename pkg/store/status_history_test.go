package store

import (
	"path/filepath"
	"testing"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// TestPublicIncidentHistoryIncludesResolved covers the public timeline:
// both open and resolved incidents must show up, ordered by started_at desc.
func TestPublicIncidentHistoryIncludesResolved(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "test.db"), "", 0)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	uid, _, _ := s.Register("alice", "pw")
	s.BindNode("node-a", uid)

	// Open a warning incident via heartbeat pressure, then resolve by recovery.
	s.Ingest(&protocol.Heartbeat{NodeID: "node-a", Memory: protocol.MemoryStats{UsedPercent: 95.0}})
	open := s.GetActiveIncidents(uid)
	if len(open) != 1 {
		t.Fatalf("expected 1 open incident, got %d", len(open))
	}
	if err := s.ResolveIncident(open[0].ID, 1); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Add a fresh critical incident so we mix resolved + open.
	s.CreateIncident("node-a", "critical", "Container Stopped: web", "exited")

	items, err := s.GetPublicIncidentHistory(0, 50)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(items) < 2 {
		t.Fatalf("expected at least 2 history rows, got %d", len(items))
	}
	var sawResolved, sawOpen bool
	for _, it := range items {
		if it.Title == "High Memory Pressure" && it.Resolved {
			sawResolved = true
		}
		if it.Title == "Container Stopped: web" && !it.Resolved {
			sawOpen = true
		}
	}
	if !sawResolved {
		t.Fatalf("expected resolved memory-pressure row in history")
	}
	if !sawOpen {
		t.Fatalf("expected unresolved container-stop row in history")
	}
}

// TestPublicIncidentHistoryRespectsLimit ensures the limit clamp works and
// never blows past the public-facing ceiling.
func TestPublicIncidentHistoryRespectsLimit(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "test.db"), "", 0)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	for i := 0; i < 5; i++ {
		// distinct titles so dedup doesn't fold them into a single row
		s.CreateIncident("node-x", "warning", "Synthetic Event "+string(rune('a'+i)), "detail")
	}
	items, err := s.GetPublicIncidentHistory(0, 2)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected limit=2 to clamp rows, got %d", len(items))
	}
}
