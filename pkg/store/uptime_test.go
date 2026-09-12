package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func TestUptimeRollupCreditsDelta(t *testing.T) {
	st, err := NewPersistentStore(filepath.Join(t.TempDir(), "uptime.db"), "", 0)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	if _, err := st.RecordHeartbeat("node-a", now); err != nil {
		t.Fatalf("first heartbeat: %v", err)
	}

	// 10s later -> 10s of credited uptime
	if _, err := st.RecordHeartbeat("node-a", now.Add(10*time.Second)); err != nil {
		t.Fatalf("second heartbeat: %v", err)
	}

	summary, err := st.NodeUptime("node-a", 1)
	if err != nil {
		t.Fatalf("NodeUptime: %v", err)
	}
	if summary.TotalSecs != 10 {
		t.Errorf("expected 10s total, got %d", summary.TotalSecs)
	}
	if summary.UpSecs != 10 {
		t.Errorf("expected 10s up, got %d", summary.UpSecs)
	}
	if summary.UptimePct < 99.9 || summary.UptimePct > 100.1 {
		t.Errorf("expected ~100%% uptime, got %.2f", summary.UptimePct)
	}
}

func TestUptimeRollupSkipsOfflineGap(t *testing.T) {
	st, err := NewPersistentStore(filepath.Join(t.TempDir(), "uptime.db"), "", 0)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	st.RecordHeartbeat("node-b", now)
	// Gap of 5 minutes exceeds onlineWindow (35s) — should credit nothing.
	st.RecordHeartbeat("node-b", now.Add(5*time.Minute))

	summary, err := st.NodeUptime("node-b", 1)
	if err != nil {
		t.Fatalf("NodeUptime: %v", err)
	}
	if summary.TotalSecs != 0 || summary.UpSecs != 0 {
		t.Errorf("expected zero delta, got total=%d up=%d", summary.TotalSecs, summary.UpSecs)
	}
}

func TestUptimeRollupAggregatesAcrossDays(t *testing.T) {
	st, err := NewPersistentStore(filepath.Join(t.TempDir(), "uptime.db"), "", 0)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	d1 := time.Date(2026, 9, 10, 23, 59, 50, 0, time.UTC)
	d2 := time.Date(2026, 9, 11, 0, 0, 5, 0, time.UTC)

	st.RecordHeartbeat("node-c", d1)
	st.RecordHeartbeat("node-c", d1.Add(5*time.Second))   // day 1: 5s
	st.RecordHeartbeat("node-c", d2)                      // cross-midnight: 10s into day 2
	st.RecordHeartbeat("node-c", d2.Add(10*time.Second))  // day 2: 10s

	summary, err := st.NodeUptime("node-c", 30)
	if err != nil {
		t.Fatalf("NodeUptime: %v", err)
	}
	if summary.TotalSecs != 25 || summary.UpSecs != 25 {
		t.Errorf("expected 25s aggregated (5 + 10 + 10), got total=%d up=%d", summary.TotalSecs, summary.UpSecs)
	}
}

func TestAllNodesUptimeListsEachNode(t *testing.T) {
	st, err := NewPersistentStore(filepath.Join(t.TempDir(), "uptime.db"), "", 0)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"alpha", "beta", "gamma"} {
		st.RecordHeartbeat(id, now)
		st.RecordHeartbeat(id, now.Add(7*time.Second))
	}

	all, err := st.AllNodesUptime(7)
	if err != nil {
		t.Fatalf("AllNodesUptime: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 nodes, got %d", len(all))
	}
	seen := map[string]bool{}
	for _, s := range all {
		seen[s.NodeID] = true
		if s.TotalSecs != 7 || s.UpSecs != 7 {
			t.Errorf("%s: expected 7s/7s, got %d/%d", s.NodeID, s.UpSecs, s.TotalSecs)
		}
	}
	for _, id := range []string{"alpha", "beta", "gamma"} {
		if !seen[id] {
			t.Errorf("missing uptime summary for %s", id)
		}
	}
}

func TestIngestTriggersUptimeRollup(t *testing.T) {
	st, err := NewPersistentStore(filepath.Join(t.TempDir(), "uptime.db"), "", 0)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	hb := &protocol.Heartbeat{
		NodeID:    "ingest-node",
		Timestamp: time.Now().Unix(),
		Node:      protocol.NodeInfo{Hostname: "h"},
	}
	st.Ingest(hb)
	st.Ingest(hb) // second ingest within onlineWindow should accumulate

	summary, err := st.NodeUptime("ingest-node", 1)
	if err != nil {
		t.Fatalf("NodeUptime: %v", err)
	}
	if summary.TotalSecs == 0 {
		t.Errorf("expected non-zero total_secs after Ingest, got %d", summary.TotalSecs)
	}
}
