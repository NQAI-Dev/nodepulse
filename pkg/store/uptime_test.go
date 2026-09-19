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
	if now.UTC().Format("2006-01-02") != time.Now().UTC().Format("2006-01-02") {
		// Pin the seed inside the NodeUptime query window (today UTC).
		// The hardcoded date drifts the moment the calendar flips — bump
		// it forward so the cutoff filter keeps the rollup row visible.
		now = time.Now().UTC().Truncate(24 * time.Hour).Add(12 * time.Hour)
	}
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

	now := time.Now().UTC().Truncate(time.Second)
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

func TestNodeUptimeDailyFillsEmptyDays(t *testing.T) {
	st, err := NewPersistentStore(filepath.Join(t.TempDir(), "daily.db"), "", 0)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	// Pick two consecutive UTC days relative to today so the 3-day window
	// the helper builds around time.Now() always lands on seeded rows.
	today := time.Now().UTC().Truncate(24 * time.Hour)
	yesterday := today.AddDate(0, 0, -1)

	// Seed day T-1 (yesterday) and bridge into today so the rollup picks up
	// at least one bucket on both ends, leaving the middle day empty.
	if _, err := st.RecordHeartbeat("node-d", yesterday); err != nil {
		t.Fatalf("seed1: %v", err)
	}
	if _, err := st.RecordHeartbeat("node-d", yesterday.Add(10*time.Second)); err != nil {
		t.Fatalf("seed2: %v", err)
	}
	// big gap so the day between yesterday and today is unobserved; the
	// heartbeat on today restarts the credit window.
	if _, err := st.RecordHeartbeat("node-d", today.Add(24*time.Hour).Add(-5*time.Second)); err != nil {
		t.Fatalf("seed3: %v", err)
	}
	if _, err := st.RecordHeartbeat("node-d", today.Add(24*time.Hour)); err != nil {
		t.Fatalf("seed4: %v", err)
	}

	rows, err := st.NodeUptimeDaily("node-d", 3)
	if err != nil {
		t.Fatalf("NodeUptimeDaily: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 daily buckets, got %d", len(rows))
	}

	days := map[string]UptimeDayBucket{}
	for _, r := range rows {
		days[r.Day] = r
	}
	wantKeys := []string{
		today.AddDate(0, 0, -2).Format("2006-01-02"),
		yesterday.Format("2006-01-02"),
		today.Format("2006-01-02"),
	}
	for _, k := range wantKeys {
		if _, ok := days[k]; !ok {
			t.Errorf("missing day %s in daily buckets", k)
		}
	}
	if got := days[yesterday.Format("2006-01-02")]; got.TotalSecs != 10 {
		t.Errorf("yesterday should hold 10s credit, got total=%d up=%d", got.TotalSecs, got.UpSecs)
	}
}

func TestNodeUptimeDailyRejectsEmptyNode(t *testing.T) {
	st, err := NewPersistentStore(filepath.Join(t.TempDir(), "daily2.db"), "", 0)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if _, err := st.NodeUptimeDaily("", 7); err == nil {
		t.Fatal("expected error for empty node id")
	}
}

func TestFleetUptimeDailyAggregatesAcrossNodes(t *testing.T) {
	st, err := NewPersistentStore(filepath.Join(t.TempDir(), "fleet.db"), "", 0)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	if now.UTC().Format("2006-01-02") != time.Now().UTC().Format("2006-01-02") {
		// Same drift fix as TestUptimeRollupCreditsDelta: keep the seed
		// inside the fleet rollup window so the test stays valid past the
		// UTC calendar flip.
		now = time.Now().UTC().Truncate(24 * time.Hour).Add(12 * time.Hour)
	}
	for _, id := range []string{"alpha", "beta"} {
		st.RecordHeartbeat(id, now)
		st.RecordHeartbeat(id, now.Add(time.Second)) // 1s credit each
	}

	rows, err := st.FleetUptimeDaily(1)
	if err != nil {
		t.Fatalf("FleetUptimeDaily: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 bucket, got %d", len(rows))
	}
	if rows[0].UpSecs != 2 {
		t.Errorf("expected 2s fleet up, got %d", rows[0].UpSecs)
	}
	if rows[0].UptimePct < 99.9 {
		t.Errorf("expected ~100%% fleet pct, got %.2f", rows[0].UptimePct)
	}
}
