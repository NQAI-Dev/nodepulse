package store

import (
	"os"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// mkWatchdogStore spins up a fresh SQLite-backed store for a single
// test. Returned cleanup deletes the db file.
func mkWatchdogStore(t *testing.T) (*PersistentStore, func()) {
	t.Helper()
	dbFile := "test_watchdog.db"
	_ = os.Remove(dbFile)
	s, err := NewPersistentStore(dbFile, "", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}
	return s, func() {
		_ = os.Remove(dbFile)
	}
}

func freshHeartbeat(nodeID string) *protocol.Heartbeat {
	return &protocol.Heartbeat{
		NodeID:    nodeID,
		Timestamp: time.Now().Unix(),
		Node:      protocol.NodeInfo{ID: nodeID, Hostname: nodeID, OS: "linux", Arch: "amd64", Version: "test"},
		CPU:       protocol.CPUStats{Load1: 0.1, Cores: 2},
		Memory:    protocol.MemoryStats{UsedPercent: 30.0, TotalBytes: 1 << 30, AvailableBytes: 1 << 29, UsedBytes: 1 << 29},
	}
}

// TestSilentWatchdog_RaisesIncidentOnSilentNode verifies that calling
// SilentNodeWatchdog for a node whose last heartbeat is older than
// silentNodeTTL opens exactly one "Node Silent" critical incident.
func TestSilentWatchdog_RaisesIncidentOnSilentNode(t *testing.T) {
	s, cleanup := mkWatchdogStore(t)
	defer cleanup()

	hb := freshHeartbeat("silent-node-1")
	hb.Memory.UsedPercent = 30.0
	s.Ingest(hb)

	// Rewind the in-memory last-heartbeat past the TTL window so the
	// watchdog sees the node as silent without us having to wait.
	s.mu.Lock()
	if st, ok := s.mem.nodes["silent-node-1"]; ok {
		st.LastHeartbeat = time.Now().Add(-10 * time.Minute)
	}
	s.mu.Unlock()

	res := s.SilentNodeWatchdog(time.Now())
	if res.IncidentsRaised != 1 {
		t.Fatalf("want 1 incident raised, got %d (raised=%v)", res.IncidentsRaised, res.RaisedNodeIDs)
	}
	if res.RaisedNodeIDs[0] != "silent-node-1" {
		t.Fatalf("unexpected node id %q", res.RaisedNodeIDs[0])
	}

	open, err := s.HasOpenSilentNodeIncident("silent-node-1")
	if err != nil {
		t.Fatalf("HasOpenSilentNodeIncident: %v", err)
	}
	if !open {
		t.Fatalf("expected open silent-node incident after watchdog pass")
	}
}

// TestSilentWatchdog_ResolvesOnHeartbeat verifies that once the agent
// resumes shipping heartbeats, the watchdog closes the previously
// raised "Node Silent" incident with the heartbeat_restored reason.
func TestSilentWatchdog_ResolvesOnHeartbeat(t *testing.T) {
	s, cleanup := mkWatchdogStore(t)
	defer cleanup()

	s.Ingest(freshHeartbeat("returned-node"))
	s.mu.Lock()
	if st, ok := s.mem.nodes["returned-node"]; ok {
		st.LastHeartbeat = time.Now().Add(-10 * time.Minute)
	}
	s.mu.Unlock()

	if r := s.SilentNodeWatchdog(time.Now()); r.IncidentsRaised != 1 {
		t.Fatalf("setup: expected 1 raised, got %d", r.IncidentsRaised)
	}

	// Simulate the agent coming back online. Fresh ingest stamps the
	// last-heartbeat to time.Now() inside the in-memory store, which
	// puts the node back above the watchdog threshold.
	s.Ingest(freshHeartbeat("returned-node"))

	res := s.SilentNodeWatchdog(time.Now())
	if res.IncidentsResolved != 1 {
		t.Fatalf("want 1 resolved, got %d", res.IncidentsResolved)
	}

	open, err := s.HasOpenSilentNodeIncident("returned-node")
	if err != nil {
		t.Fatalf("HasOpenSilentNodeIncident: %v", err)
	}
	if open {
		t.Fatalf("expected silent-node incident closed after heartbeat return")
	}

	// And the resolution_reason must be the watchdog tag so the audit
	// timeline distinguishes "agent came back" from "metric_recovered".
	var reason string
	err = s.db.QueryRow(
		"SELECT resolution_reason FROM incidents WHERE node_id = ? AND title = ? ORDER BY id DESC LIMIT 1",
		"returned-node", "Node Silent",
	).Scan(&reason)
	if err != nil {
		t.Fatalf("read resolution_reason: %v", err)
	}
	if reason != "auto:heartbeat_restored" {
		t.Fatalf("want auto:heartbeat_restored, got %q", reason)
	}
}

// TestSilentWatchdog_IdempotentOnLongOutage makes sure that two
// consecutive passes on a still-silent node only keep one open incident.
// Without this guard the public status page would show duplicate rows.
func TestSilentWatchdog_IdempotentOnLongOutage(t *testing.T) {
	s, cleanup := mkWatchdogStore(t)
	defer cleanup()

	s.Ingest(freshHeartbeat("dead-node"))
	s.mu.Lock()
	if st, ok := s.mem.nodes["dead-node"]; ok {
		st.LastHeartbeat = time.Now().Add(-time.Hour)
	}
	s.mu.Unlock()

	first := s.SilentNodeWatchdog(time.Now())
	second := s.SilentNodeWatchdog(time.Now())
	if first.IncidentsRaised != 1 {
		t.Fatalf("first pass: want 1 raised, got %d", first.IncidentsRaised)
	}
	if second.IncidentsRaised != 0 {
		t.Fatalf("second pass: want 0 raised (already open), got %d", second.IncidentsRaised)
	}

	var n int
	if err := s.db.QueryRow(
		"SELECT COUNT(*) FROM incidents WHERE node_id = ? AND title = ? AND resolved = 0",
		"dead-node", "Node Silent",
	).Scan(&n); err != nil {
		t.Fatalf("count open: %v", err)
	}
	if n != 1 {
		t.Fatalf("want exactly 1 open silent incident, got %d", n)
	}
}

// TestSilentWatchdog_IgnoresRecentlyActiveNodes is the regression guard
// against the watchdog spamming alerts on a healthy fleet.
func TestSilentWatchdog_IgnoresRecentlyActiveNodes(t *testing.T) {
	s, cleanup := mkWatchdogStore(t)
	defer cleanup()

	for _, id := range []string{"alive-a", "alive-b", "alive-c"} {
		s.Ingest(freshHeartbeat(id))
	}

	res := s.SilentNodeWatchdog(time.Now())
	if res.IncidentsRaised != 0 {
		t.Fatalf("healthy fleet should not raise incidents, got %d (%v)", res.IncidentsRaised, res.RaisedNodeIDs)
	}
}
