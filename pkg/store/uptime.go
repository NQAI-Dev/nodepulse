package store

import (
	"database/sql"
	"fmt"
	"sync"
	"time"
)

// UptimeRollup is the per-node daily uptime bucket.
type UptimeRollup struct {
	NodeID    string `json:"node_id"`
	Day       string `json:"day"`        // YYYY-MM-DD (UTC)
	TotalSecs int64  `json:"total_secs"` // seconds observed this day
	UpSecs    int64  `json:"up_secs"`    // seconds where node was online
}

// UptimeSummary is the aggregated uptime for a node over a window.
type UptimeSummary struct {
	NodeID     string  `json:"node_id"`
	Days       int     `json:"days"`
	TotalSecs  int64   `json:"total_secs"`
	UpSecs     int64   `json:"up_secs"`
	UptimePct  float64 `json:"uptime_pct"`
}

// uptimeTracker remembers the last time we credited a node so we can charge
// the wall-clock delta into the daily rollup on the next heartbeat.
//
// The rollup is wall-clock-based: "we observed node X alive for Δ seconds
// between heartbeats". If the gap between heartbeats exceeds the "online"
// window (default 35s — see Store.GetAll), we credit zero seconds for that
// gap because the node was effectively offline.
type uptimeTracker struct {
	mu       sync.Mutex
	lastSeen map[string]time.Time
}

func newUptimeTracker() *uptimeTracker {
	return &uptimeTracker{lastSeen: make(map[string]time.Time)}
}

// onlineWindow is the gap beyond which we treat the node as offline.
// Must mirror the 35s threshold used in store.GetAll().
//
// ponytail: keep this aligned with the in-memory status threshold; if you
// change one, change the other. A 60s default leaves headroom for slow
// heartbeats without flapping the daily uptime numbers.
const onlineWindow = 35 * time.Second

// RecordHeartbeat credits elapsed wall-clock seconds into the current UTC
// day's rollup for nodeID. Safe to call from the ingest path on every
// heartbeat. Returns the delta that was credited so callers can log it.
func (p *PersistentStore) RecordHeartbeat(nodeID string, now time.Time) (time.Duration, error) {
	if p == nil || p.db == nil {
		return 0, nil
	}

	p.uptime.mu.Lock()
	last, ok := p.uptime.lastSeen[nodeID]
	p.uptime.lastSeen[nodeID] = now
	p.uptime.mu.Unlock()

	if !ok {
		// First heartbeat we've ever seen for this node — no delta yet.
		return 0, nil
	}

	delta := now.Sub(last)
	if delta <= 0 {
		return 0, nil
	}
	if delta > onlineWindow {
		// Node was effectively offline across the gap; credit nothing.
		return 0, nil
	}

	day := now.UTC().Format("2006-01-02")
	// Round up to at least 1s when a positive delta exists: if a heartbeat
	// arrived, the node was alive for at least a fraction of a second, and
	// floor-to-zero would zero out fast-cadence fleets entirely.
	secs := int64(delta.Seconds())
	if secs < 1 {
		secs = 1
	}
	if err := p.upsertUptime(nodeID, day, secs, secs); err != nil {
		return 0, err
	}
	return delta, nil
}

// upsertUptime bumps total_secs and up_secs for (node, day) by the supplied
// delta. Uses a transaction so the two columns can't drift apart.
func (p *PersistentStore) upsertUptime(nodeID, day string, totalDelta, upDelta int64) error {
	tx, err := p.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		INSERT INTO node_uptime_daily (node_id, day, total_secs, up_secs)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(node_id, day) DO UPDATE SET
			total_secs = total_secs + excluded.total_secs,
			up_secs    = up_secs + excluded.up_secs
	`, nodeID, day, totalDelta, upDelta); err != nil {
		return err
	}
	return tx.Commit()
}

// NodeUptime returns aggregated uptime for nodeID over the last `days` days
// (inclusive of today). days is clamped to [1, 90].
func (p *PersistentStore) NodeUptime(nodeID string, days int) (UptimeSummary, error) {
	if days < 1 {
		days = 1
	}
	if days > 90 {
		days = 90
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days+1).Format("2006-01-02")

	var totalSecs, upSecs sql.NullInt64
	err := p.db.QueryRow(`
		SELECT COALESCE(SUM(total_secs), 0), COALESCE(SUM(up_secs), 0)
		FROM node_uptime_daily
		WHERE node_id = ? AND day >= ?
	`, nodeID, cutoff).Scan(&totalSecs, &upSecs)
	if err != nil {
		return UptimeSummary{}, err
	}

	summary := UptimeSummary{
		NodeID:    nodeID,
		Days:      days,
		TotalSecs: totalSecs.Int64,
		UpSecs:    upSecs.Int64,
	}
	if summary.TotalSecs > 0 {
		summary.UptimePct = float64(summary.UpSecs) / float64(summary.TotalSecs) * 100.0
	}
	return summary, nil
}

// AllNodesUptime returns uptime summaries for every node that has any
// rollup rows, ordered by node_id. Used by the public status page.
func (p *PersistentStore) AllNodesUptime(days int) ([]UptimeSummary, error) {
	if days < 1 {
		days = 1
	}
	if days > 90 {
		days = 90
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days+1).Format("2006-01-02")

	rows, err := p.db.Query(`
		SELECT node_id,
		       COALESCE(SUM(total_secs), 0),
		       COALESCE(SUM(up_secs), 0)
		FROM node_uptime_daily
		WHERE day >= ?
		GROUP BY node_id
		ORDER BY node_id
	`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]UptimeSummary, 0, 8)
	for rows.Next() {
		var s UptimeSummary
		s.Days = days
		if err := rows.Scan(&s.NodeID, &s.TotalSecs, &s.UpSecs); err != nil {
			return nil, err
		}
		if s.TotalSecs > 0 {
			s.UptimePct = float64(s.UpSecs) / float64(s.TotalSecs) * 100.0
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UptimeDayBucket is one UTC day's rollup row for a single node. Emitted as
// part of the per-node uptime drill-down so the status widget can render a
// bar-chart of daily availability without re-aggregating client-side.
type UptimeDayBucket struct {
	Day       string  `json:"day"`        // YYYY-MM-DD (UTC)
	TotalSecs int64   `json:"total_secs"` // seconds observed this day
	UpSecs    int64   `json:"up_secs"`    // seconds where node was online
	UptimePct float64 `json:"uptime_pct"` // 0..100; 0 when TotalSecs == 0
}

// NodeUptimeDaily returns one UptimeDayBucket per UTC day for nodeID over
// the last `days` days (inclusive of today). Days with no rollup rows are
// emitted as zero-secs buckets so the widget timeline stays continuous.
//
// ponytail: returns up to 90 rows; for a single page render that's fine.
// If callers ever ask for years of history, pre-aggregate in SQL with a
// weekly CTE.
func (p *PersistentStore) NodeUptimeDaily(nodeID string, days int) ([]UptimeDayBucket, error) {
	if nodeID == "" {
		return nil, fmt.Errorf("node_id required")
	}
	if days < 1 {
		days = 1
	}
	if days > 90 {
		days = 90
	}

	today := time.Now().UTC()
	todayKey := today.Format("2006-01-02")
	cutoff := today.AddDate(0, 0, -days+1).Format("2006-01-02")

	rows, err := p.db.Query(`
		SELECT day, total_secs, up_secs
		  FROM node_uptime_daily
		 WHERE node_id = ? AND day >= ?
		 ORDER BY day ASC
	`, nodeID, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byDay := make(map[string]struct {
		total, up int64
	}, days)
	for rows.Next() {
		var day string
		var total, up int64
		if err := rows.Scan(&day, &total, &up); err != nil {
			return nil, err
		}
		byDay[day] = struct {
			total, up int64
		}{total, up}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]UptimeDayBucket, 0, days)
	for i := 0; i < days; i++ {
		ts := today.AddDate(0, 0, -days+1+i)
		key := ts.Format("2006-01-02")
		b := UptimeDayBucket{Day: key}
		if v, ok := byDay[key]; ok {
			b.TotalSecs = v.total
			b.UpSecs = v.up
			if b.TotalSecs > 0 {
				b.UptimePct = float64(b.UpSecs) / float64(b.TotalSecs) * 100.0
			}
		}
		out = append(out, b)
	}
	_ = todayKey
	return out, nil
}

// FleetUptimeDaily returns the fleet-wide daily rollup: each row aggregates
// every node's up_secs/total_secs into a single per-day bucket. Days with
// no activity are emitted as zero-secs buckets. Mirrors NodeUptimeDaily
// shape so the status widget can plot fleet vs single-node with the same
// parser.
func (p *PersistentStore) FleetUptimeDaily(days int) ([]UptimeDayBucket, error) {
	if days < 1 {
		days = 1
	}
	if days > 90 {
		days = 90
	}

	today := time.Now().UTC()
	todayKey := today.Format("2006-01-02")
	cutoff := today.AddDate(0, 0, -days+1).Format("2006-01-02")

	rows, err := p.db.Query(`
		SELECT day, SUM(total_secs), SUM(up_secs)
		  FROM node_uptime_daily
		 WHERE day >= ?
		 GROUP BY day
		 ORDER BY day ASC
	`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byDay := make(map[string]struct {
		total, up int64
	}, days)
	for rows.Next() {
		var day string
		var total, up sql.NullInt64
		if err := rows.Scan(&day, &total, &up); err != nil {
			return nil, err
		}
		byDay[day] = struct {
			total, up int64
		}{total.Int64, up.Int64}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]UptimeDayBucket, 0, days)
	for i := 0; i < days; i++ {
		ts := today.AddDate(0, 0, -days+1+i)
		key := ts.Format("2006-01-02")
		b := UptimeDayBucket{Day: key}
		if v, ok := byDay[key]; ok {
			b.TotalSecs = v.total
			b.UpSecs = v.up
			if b.TotalSecs > 0 {
				b.UptimePct = float64(b.UpSecs) / float64(b.TotalSecs) * 100.0
			}
		}
		out = append(out, b)
	}
	_ = todayKey
	return out, nil
}

// formatUptimePct renders the percentage with a single decimal, suitable for
// display. 100.00 collapses to 100.0; 99.95 stays distinct.
func formatUptimePct(pct float64) string {
	if pct > 100.0 {
		pct = 100.0
	}
	if pct < 0.0 {
		pct = 0.0
	}
	return fmt.Sprintf("%.1f%%", pct)
}
