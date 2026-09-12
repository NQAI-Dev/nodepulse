package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func makeHb(id string) *protocol.Heartbeat {
	return &protocol.Heartbeat{
		NodeID:    id,
		Timestamp: time.Now().Unix(),
		Node:      protocol.NodeInfo{Hostname: id, Arch: "amd64", OS: "linux"},
		CPU:       protocol.CPUStats{Cores: 4, Load1: 0.1},
		Memory:    protocol.MemoryStats{TotalBytes: 1 << 30, UsedPercent: 10},
	}
}

func TestResolveIncident_OwnerOnlyEnforced(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "r.db"), "", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// user 1 is the seeded admin; user 2/3 are fresh.
	uidAlice, _, err := s.Register("alice", "alice-pw-123")
	if err != nil {
		t.Fatalf("register alice: %v", err)
	}
	uidBob, _, err := s.Register("bob", "bob-pw-123")
	if err != nil {
		t.Fatalf("register bob: %v", err)
	}

	// alice owns node-a; an incident lands on node-a.
	s.Ingest(makeHb("node-a"))
	res, err := s.db.Exec(`INSERT INTO node_owners (node_id, user_id) VALUES (?, ?)`, "node-a", uidAlice)
	if err != nil {
		t.Fatal(err)
	}
	_ = res
	res, err = s.db.Exec(
		`INSERT INTO incidents (node_id, severity, title, detail, started_at, resolved) VALUES (?, ?, ?, ?, ?, 0)`,
		"node-a", "warning", "mem pressure", "x", time.Now().Unix(),
	)
	if err != nil {
		t.Fatal(err)
	}
	incID, _ := res.LastInsertId()
	idStr := intToA(incID)

	// bob (different user, no node_owners row) must NOT be able to resolve alice's incident.
	if err := s.ResolveIncident(idStr, uidBob); err != ErrIncidentForbidden {
		t.Fatalf("bob resolve alice's incident should be forbidden, got %v", err)
	}

	// alice (owner) resolves successfully.
	if err := s.ResolveIncident(idStr, uidAlice); err != nil {
		t.Fatalf("alice resolve her own incident: %v", err)
	}

	// unknown id -> not-found
	if err := s.ResolveIncident(intToA(999999), uidAlice); err != ErrIncidentNotFound {
		t.Fatalf("missing id should be not-found, got %v", err)
	}
}

func TestResolveIncident_AdminUser1BypassesOwnership(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "r.db"), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	s.Ingest(makeHb("lone-node"))
	res, err := s.db.Exec(
		`INSERT INTO incidents (node_id, severity, title, detail, started_at, resolved) VALUES (?, ?, ?, ?, ?, 0)`,
		"lone-node", "warning", "mem pressure", "x", time.Now().Unix(),
	)
	if err != nil {
		t.Fatal(err)
	}
	incID, _ := res.LastInsertId()
	if err := s.ResolveIncident(intToA(incID), 1); err != nil {
		t.Fatalf("user 1 (admin) must always resolve, got %v", err)
	}
}
