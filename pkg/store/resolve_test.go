package store

import (
	"path/filepath"
	"testing"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func TestResolveStaleIncidentsMemoryRecovers(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "test.db"), "", 0)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	uid, _, _ := s.Register("alice", "pw")
	s.BindNode("node-a", uid)

	// 1) Pressure → creates incident
	hb := &protocol.Heartbeat{
		NodeID: "node-a",
		Memory: protocol.MemoryStats{UsedPercent: 95.0},
	}
	s.Ingest(hb)
	if got := s.GetActiveIncidents(uid); len(got) != 1 {
		t.Fatalf("expected 1 open incident after pressure, got %d", len(got))
	}

	// 2) Recovery → auto-resolve
	hb2 := &protocol.Heartbeat{
		NodeID: "node-a",
		Memory: protocol.MemoryStats{UsedPercent: 60.0},
	}
	s.Ingest(hb2)
	if got := s.GetActiveIncidents(uid); len(got) != 0 {
		t.Fatalf("expected 0 open incidents after recovery, got %d", len(got))
	}
}

func TestResolveStaleIncidentsDockerRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "test.db"), "", 0)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	uid, _, _ := s.Register("bob", "pw")
	s.BindNode("node-b", uid)

	hb := &protocol.Heartbeat{
		NodeID:   "node-b",
		Memory:   protocol.MemoryStats{UsedPercent: 10.0},
		Services: []protocol.ServiceStatus{{Name: "nginx", Type: "docker", Active: false}},
	}
	s.Ingest(hb)
	if got := s.GetActiveIncidents(uid); len(got) != 1 {
		t.Fatalf("expected 1 incident for stopped container, got %d", len(got))
	}

	// Container came back up → incident should auto-resolve.
	hb2 := &protocol.Heartbeat{
		NodeID:   "node-b",
		Memory:   protocol.MemoryStats{UsedPercent: 10.0},
		Services: []protocol.ServiceStatus{{Name: "nginx", Type: "docker", Active: true}},
	}
	s.Ingest(hb2)
	if got := s.GetActiveIncidents(uid); len(got) != 0 {
		t.Fatalf("expected 0 incidents after container restart, got %d", len(got))
	}
}

func TestResolveStaleIncidentsDedupesResolution(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "test.db"), "", 0)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	uid, _, _ := s.Register("carol", "pw")
	s.BindNode("node-c", uid)

	hb := &protocol.Heartbeat{
		NodeID: "node-c",
		Memory: protocol.MemoryStats{UsedPercent: 95.0},
	}
	s.Ingest(hb)
	if got := s.GetActiveIncidents(uid); len(got) != 1 {
		t.Fatalf("setup: expected 1 open incident")
	}

	// Three consecutive normal heartbeats: must only resolve once (idempotent).
	for i := 0; i < 3; i++ {
		s.Ingest(&protocol.Heartbeat{
			NodeID: "node-c",
			Memory: protocol.MemoryStats{UsedPercent: 40.0},
		})
	}
	if got := s.GetActiveIncidents(uid); len(got) != 0 {
		t.Fatalf("expected 0 open incidents, got %d", len(got))
	}
}
