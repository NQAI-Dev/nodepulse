package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func TestAutoHealLogPersistence(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("init store: %v", err)
	}

	now := time.Now().Unix()
	events := []protocol.AutoHealLog{
		{Command: "restart_docker:web", Status: "ok", Ts: now},
		{Command: "restart_docker:web", Status: "skipped", Reason: "cooldown", Ts: now + 1},
		{Command: "restart_docker:web", Status: "skipped", Reason: "circuit_open", Ts: now + 2, RetrySec: 240},
		{Command: "restart_systemd:nginx", Status: "failed", Error: "unit not found", Ts: now + 3},
	}
	_ = s.RecordAutoHealLogs("node-1", 0, events)

	logs := s.RecentAutoHealLogs("node-1", 10)
	if len(logs) != 4 {
		t.Fatalf("expected 4 logs, got %d", len(logs))
	}
	if logs[0].Command != "restart_systemd:nginx" {
		t.Fatalf("expected newest-first ordering, got %q", logs[0].Command)
	}
	// RetrySec is not persisted (it's only relevant on the wire); just check
	// the round-trip keeps the reason and command.
	if logs[1].Reason != "circuit_open" || logs[1].Command != "restart_docker:web" {
		t.Fatalf("expected circuit_open row at index 1, got %+v", logs[1])
	}
}

func TestAutoHealLogPrune(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("init store: %v", err)
	}

	old := time.Now().Add(-48 * time.Hour).Unix()
	recent := time.Now().Unix()
	_ = s.RecordAutoHealLogs("node-prune", 0, []protocol.AutoHealLog{
		{Command: "restart_docker:alpha", Status: "ok", Ts: old},
		{Command: "restart_docker:alpha", Status: "ok", Ts: old - 100},
		{Command: "restart_docker:alpha", Status: "ok", Ts: recent},
	})

	logs := s.RecentAutoHealLogs("node-prune", 10)
	if len(logs) != 1 {
		t.Fatalf("expected only the recent log to survive pruning, got %d", len(logs))
	}
}
