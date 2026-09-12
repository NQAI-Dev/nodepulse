package store

import (
	"testing"
	"time"
)

func TestGetIncidentHistory_RespectsFilters(t *testing.T) {
	p := newTestStore(t)
	p.BindNode("node-h", 1)

	// Seed: 2 critical, 1 warning, one outside the 24h window.
	now := time.Now().Unix()
	p.CreateIncident("node-h", "critical", "DB Down", "primary unreachable")
	p.CreateIncident("node-h", "warning", "High CPU", "load 5.2")
	p.CreateIncident("node-h", "critical", "Old Outage", "very old")

	// Backdate the old incident so it falls outside the 24h window.
	p.db.Exec("UPDATE incidents SET started_at = ? WHERE title = 'Old Outage'", now-48*3600)

	hist, err := p.GetIncidentHistory(1, "24h", "node-h", "", 50)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("want 2 inside 24h, got %d", len(hist))
	}
}

func TestGetIncidentHistory_SeverityFilter(t *testing.T) {
	p := newTestStore(t)
	p.BindNode("node-s", 1)
	p.CreateIncident("node-s", "critical", "X", "x")
	p.CreateIncident("node-s", "warning", "Y", "y")
	p.CreateIncident("node-s", "warning", "Z", "z")

	crits, _ := p.GetIncidentHistory(1, "7d", "node-s", "critical", 10)
	if len(crits) != 1 {
		t.Fatalf("want 1 critical, got %d", len(crits))
	}
	warns, _ := p.GetIncidentHistory(1, "7d", "node-s", "warning", 10)
	if len(warns) != 2 {
		t.Fatalf("want 2 warnings, got %d", len(warns))
	}
}

func TestGetIncidentHistory_LimitClamp(t *testing.T) {
	p := newTestStore(t)
	p.BindNode("node-l", 1)
	for i := 0; i < 10; i++ {
		// Unique titles so the per-(node,title) throttling doesn't merge them.
		p.CreateIncident("node-l", "warning", "Fill"+intToA(int64(i)), "x")
	}
	// Limit 0 should fall back to default 100.
	hist, _ := p.GetIncidentHistory(1, "7d", "node-l", "", 0)
	if len(hist) == 0 {
		t.Fatal("expected default-limit results, got 0")
	}
	// Explicit small limit.
	hist2, _ := p.GetIncidentHistory(1, "7d", "node-l", "", 3)
	if len(hist2) != 3 {
		t.Fatalf("limit=3 should clamp, got %d", len(hist2))
	}
}

func TestGetIncidentHistory_UnknownRangeFallsBack(t *testing.T) {
	p := newTestStore(t)
	p.BindNode("node-u", 1)
	p.CreateIncident("node-u", "warning", "x", "y")
	hist, _ := p.GetIncidentHistory(1, "bogus", "node-u", "", 10)
	if len(hist) != 1 {
		t.Fatalf("unknown range should still return data, got %d", len(hist))
	}
}

func TestGetIncidentHistory_IncludesResolved(t *testing.T) {
	p := newTestStore(t)
	p.BindNode("node-r", 1)
	p.CreateIncident("node-r", "critical", "Firesale", "details")
	id, _ := resolveFirst(p, "Firesale")
	if id == "" {
		t.Fatal("expected to find created incident id")
	}
	hist, _ := p.GetIncidentHistory(1, "7d", "node-r", "", 10)
	if len(hist) != 1 || hist[0].ResolvedAt == 0 {
		t.Fatalf("resolved incident missing resolved_at: %+v", hist)
	}
}

func TestGetIncidentStats_BucketLayout(t *testing.T) {
	p := newTestStore(t)
	p.BindNode("node-b", 1)
	for i := 0; i < 3; i++ {
		// Unique titles so each CreateIncident lands as a fresh row.
		p.CreateIncident("node-b", "warning", "W"+intToA(int64(i)), "x")
	}
	for i := 0; i < 2; i++ {
		p.CreateIncident("node-b", "critical", "C"+intToA(int64(i)), "x")
	}
	buckets, err := p.GetIncidentStats(1, "24h")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(buckets) == 0 {
		t.Fatal("want non-empty bucket timeline for 24h")
	}
	// Sum across buckets must equal total created (5).
	totalCritical := 0
	totalWarning := 0
	for _, b := range buckets {
		totalCritical += b.CriticalOpened
		totalWarning += b.WarningOpened
	}
	if totalCritical != 2 || totalWarning != 3 {
		t.Fatalf("stats totals wrong: crit=%d warn=%d", totalCritical, totalWarning)
	}
}

func TestGetIncidentStats_NetZeroAfterResolve(t *testing.T) {
	p := newTestStore(t)
	p.BindNode("node-net", 1)
	p.CreateIncident("node-net", "critical", "Resolved Later", "x")
	_, _ = resolveFirst(p, "Resolved Later")
	buckets, _ := p.GetIncidentStats(1, "24h")
	totalOpened := 0
	totalResolved := 0
	for _, b := range buckets {
		totalOpened += b.CriticalOpened
		totalResolved += b.CriticalResolved
	}
	if totalOpened != 1 {
		t.Fatalf("opened should still count before subtract, got %d", totalOpened)
	}
	if totalResolved != 1 {
		t.Fatalf("resolved should also count, got %d", totalResolved)
	}
}

// resolveFirst finds the first incident matching title and marks it
// resolved through the public API so the resolved_at column is set.
func resolveFirst(p *PersistentStore, title string) (string, error) {
	var id int64
	err := p.db.QueryRow("SELECT id FROM incidents WHERE title = ? ORDER BY id ASC LIMIT 1", title).Scan(&id)
	if err != nil {
		return "", err
	}
	pid := intToA(id)
	if err := p.ResolveIncident(pid, 1); err != nil {
		return "", err
	}
	return pid, nil
}

func intToA(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
