package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func mkHeartbeat(id string, ts time.Time) protocol.Heartbeat {
	return protocol.Heartbeat{
		NodeID:    id,
		Timestamp: ts.Unix(),
		Node:      protocol.NodeInfo{Hostname: id, Arch: "amd64", OS: "linux"},
		CPU:       protocol.CPUStats{Cores: 4, Load1: 0.1},
		Memory:    protocol.MemoryStats{TotalBytes: 1 << 30, UsedPercent: 10},
	}
}

func TestFleetSummary_EmptyFleetIsOperational(t *testing.T) {
	s, err := NewPersistentStore(filepath.Join(t.TempDir(), "fleet.db"), "", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got := s.GetPublicFleetSummary()
	if got.Status != "operational" {
		t.Fatalf("empty fleet must be operational, got %q", got.Status)
	}
	if got.NodesTotal != 0 || got.NodesOnline != 0 || got.IncidentsOpen != 0 {
		t.Fatalf("empty fleet counters wrong: %+v", got)
	}
	if got.UpdatedAt <= 0 {
		t.Fatal("UpdatedAt must be set")
	}
}

func TestFleetSummary_OfflineNodesDropStatusToDegraded(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "fleet.db"), "", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now()
	hbOnline := mkHeartbeat("node-up", now)
	s.Ingest(&hbOnline)
	hbOld := mkHeartbeat("node-stale", now.Add(-2*time.Minute))
	s.Ingest(&hbOld)

	// Ingest stamps LastHeartbeat with time.Now(), so we rewind the stale
	// node's heartbeat directly to simulate a long-silent agent.
	s.mem.mu.Lock()
	if n, ok := s.mem.nodes["node-stale"]; ok {
		n.LastHeartbeat = now.Add(-2 * time.Minute)
	}
	s.mem.mu.Unlock()

	got := s.GetPublicFleetSummary()
	if got.NodesTotal != 2 {
		t.Fatalf("want 2 nodes, got %d", got.NodesTotal)
	}
	if got.NodesOnline != 1 {
		t.Fatalf("want 1 online, got %d", got.NodesOnline)
	}
	if got.NodesOffline != 1 {
		t.Fatalf("want 1 offline (stale heartbeat), got %d", got.NodesOffline)
	}
	if got.Status != "degraded" {
		t.Fatalf("offline node should drop status to degraded, got %q", got.Status)
	}
}

func TestFleetSummary_CriticalIncidentPromotesToOutage(t *testing.T) {
	s, _ := NewPersistentStore(filepath.Join(t.TempDir(), "fleet.db"), "", 0)
	now := time.Now().Unix()
	if _, err := s.db.Exec(
		`INSERT INTO incidents (node_id, severity, title, detail, started_at, resolved) VALUES (?, ?, ?, ?, ?, 0)`,
		"node-x", "critical", "db down", "x", now,
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got := s.GetPublicFleetSummary()
	if got.Status != "outage" {
		t.Fatalf("critical incident must mark outage, got %q", got.Status)
	}
	if got.IncidentsCrit != 1 || got.IncidentsOpen != 1 {
		t.Fatalf("incident counters wrong: %+v", got)
	}
}
