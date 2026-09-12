package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openJanitorStore(t *testing.T) *PersistentStore {
	t.Helper()
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "test.db"), "", 0)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

func TestJanitor_PrunesResolvedIncidentsOlderThanRetention(t *testing.T) {
	s := openJanitorStore(t)
	now := time.Now().Unix()
	old := now - int64(91*24*time.Hour/time.Second)
	recent := now - int64(2*24*time.Hour/time.Second)

	if _, err := s.db.Exec(
		`INSERT INTO incidents (node_id, severity, title, detail, started_at, resolved, resolved_at) VALUES (?, ?, ?, ?, ?, 1, ?)`,
		"node-old", "warning", "stale", "x", old, old,
	); err != nil {
		t.Fatalf("insert old: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO incidents (node_id, severity, title, detail, started_at, resolved, resolved_at) VALUES (?, ?, ?, ?, ?, 1, ?)`,
		"node-recent", "warning", "fresh", "x", recent, recent,
	); err != nil {
		t.Fatalf("insert recent: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO incidents (node_id, severity, title, detail, started_at, resolved, resolved_at) VALUES (?, ?, ?, ?, ?, 0, 0)`,
		"node-open", "critical", "still going", "x", old,
	); err != nil {
		t.Fatalf("insert open: %v", err)
	}

	s.runJanitorPass()

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM incidents WHERE node_id = 'node-old'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected old resolved incident pruned, got %d rows", n)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM incidents WHERE node_id = 'node-recent'`).Scan(&n); err != nil {
		t.Fatalf("count recent: %v", err)
	}
	if n != 1 {
		t.Fatalf("recent resolved incident must survive, got %d rows", n)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM incidents WHERE node_id = 'node-open'`).Scan(&n); err != nil {
		t.Fatalf("count open: %v", err)
	}
	if n != 1 {
		t.Fatalf("open incidents must never be pruned, got %d rows", n)
	}
}

func TestJanitor_ExpiresPastMaintenanceWindows(t *testing.T) {
	s := openJanitorStore(t)
	now := time.Now().Unix()
	past := now - 60
	future := now + 3600

	for _, w := range []struct {
		end  int64
		name string
	}{
		{past, "should-expire"},
		{future, "should-stay"},
		{0, "open-ended"},
	} {
		if _, err := s.db.Exec(
			`INSERT INTO maintenance_windows (user_id, scope, reason, node_ids, start_unix, end_unix, created_at, created_by) VALUES (1, 'user', ?, '', ?, ?, ?, 't')`,
			"x", past-3600, w.end, now,
		); err != nil {
			t.Fatalf("insert %s: %v", w.name, err)
		}
	}

	s.runJanitorPass()

	var expired int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM maintenance_windows WHERE end_unix > 0 AND end_unix < ?`, now).Scan(&expired); err != nil {
		t.Fatalf("count: %v", err)
	}
	if expired != 0 {
		t.Fatalf("expected 0 expired rows left, got %d", expired)
	}
	var survivors int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM maintenance_windows`).Scan(&survivors); err != nil {
		t.Fatalf("survivors count: %v", err)
	}
	if survivors != 2 {
		t.Fatalf("expected 2 surviving maintenance windows (future + open-ended), got %d", survivors)
	}
}

func TestRunJanitor_StopsOnContextCancel(t *testing.T) {
	s := openJanitorStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.RunJanitor(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunJanitor did not return after context cancel")
	}
}
