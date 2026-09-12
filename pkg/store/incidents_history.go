package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// IncidentWithTimestamps extends protocol.Incident with the resolution
// timestamp so the history view can render duration-to-resolve without a
// second round-trip per row.
type IncidentWithTimestamps struct {
	protocol.Incident
	ResolvedAt int64 `json:"resolved_at"`
}

// GetIncidentHistory returns the most recent incidents for a user, optionally
// filtered by node and severity. The view spans both active and resolved
// rows; for an "open only" feed use GetActiveIncidents instead.
//
// rangeKey clamps the window to 24h, 7d, or 30d; anything else falls back
// to 7d. limit is clamped to [1, 500].
//
// ponytail: a single JOIN on node_owners is enough today; if the history
// endpoint gets hot, add a covering index on (user_id, started_at DESC).
func (p *PersistentStore) GetIncidentHistory(userID int64, rangeKey, nodeID, severity string, limit int) ([]IncidentWithTimestamps, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var window int64
	switch rangeKey {
	case "24h":
		window = 24 * 3600
	case "7d":
		window = 7 * 24 * 3600
	case "30d":
		window = 30 * 24 * 3600
	default:
		window = 7 * 24 * 3600
		rangeKey = "7d"
	}
	cutoff := time.Now().Unix() - window

	query := `SELECT i.id, i.node_id, i.severity, i.title, i.detail,
	                 i.started_at, i.resolved, COALESCE(i.resolved_at, 0),
	                 COALESCE(i.acknowledged_at, 0), COALESCE(i.last_notified_at, 0)
	            FROM incidents i`
	args := []interface{}{}
	where := []string{"i.started_at >= ?"}
	args = append(args, cutoff)

	if userID > 1 {
		query += ` JOIN node_owners o ON o.node_id = i.node_id`
		where = append(where, "o.user_id = ?")
		args = append(args, userID)
	}
	if nodeID != "" {
		where = append(where, "i.node_id = ?")
		args = append(args, nodeID)
	}
	if severity == "critical" || severity == "warning" {
		where = append(where, "i.severity = ?")
		args = append(args, severity)
	}
	if len(where) > 0 {
		query += " WHERE " + joinWhere(where, " AND ")
	}
	query += " ORDER BY i.started_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := p.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]IncidentWithTimestamps, 0, 32)
	for rows.Next() {
		var i IncidentWithTimestamps
		var id int64
		var res int
		if err := rows.Scan(&id, &i.NodeID, &i.Severity, &i.Title, &i.Detail,
			&i.StartedAt, &res, &i.ResolvedAt,
			&i.Incident.AcknowledgedAt, &i.Incident.LastNotifiedAt); err != nil {
			continue
		}
		i.ID = fmt.Sprintf("%d", id)
		i.Resolved = res == 1
		out = append(out, i)
	}
	return out, rows.Err()
}

// IncidentStatsBucket is one bucket of an incident-count sparkline.
// Timestamp is the bucket-start in unix seconds; Counts holds per-severity
// totals inside the bucket.
type IncidentStatsBucket struct {
	Timestamp       int64 `json:"ts"`
	CriticalOpened  int   `json:"critical_opened"`
	WarningOpened   int   `json:"warning_opened"`
	CriticalResolved int   `json:"critical_resolved"`
	WarningResolved  int   `json:"warning_resolved"`
}

// GetIncidentStats returns bucketed counts (opened + resolved) for the
// supplied window. Bucket cadence: 1h for ≤24h windows, 6h for 7d,
// 1d for 30d. The dashboard uses this to render a "this week" timeline
// without shipping every incident to the browser.
func (p *PersistentStore) GetIncidentStats(userID int64, rangeKey string) ([]IncidentStatsBucket, error) {
	var window int64
	var bucketSec int64
	switch rangeKey {
	case "24h":
		window = 24 * 3600
		bucketSec = 3600
	case "7d":
		window = 7 * 24 * 3600
		bucketSec = 6 * 3600
	case "30d":
		window = 30 * 24 * 3600
		bucketSec = 24 * 3600
	default:
		window = 7 * 24 * 3600
		bucketSec = 6 * 3600
		rangeKey = "7d"
	}
	now := time.Now().Unix()
	from := now - window

	userFilter := ""
	args := []interface{}{from, now, bucketSec}
	if userID > 1 {
		userFilter = " AND EXISTS (SELECT 1 FROM node_owners o WHERE o.node_id = i.node_id AND o.user_id = ?)"
		args = append(args, userID)
	}
	_ = userFilter
	// Bucketed counts of opened incidents per severity.
	openedQ := `SELECT (started_at / ?) * ? AS bucket_ts,
	                  SUM(CASE WHEN severity = 'critical' THEN 1 ELSE 0 END),
	                  SUM(CASE WHEN severity = 'warning' THEN 1 ELSE 0 END)
	             FROM incidents i
	            WHERE started_at >= ? AND started_at <= ?
	              ` + userFilter + `
	         GROUP BY bucket_ts
	         ORDER BY bucket_ts ASC`
	argsOpened := []interface{}{bucketSec, bucketSec, from, now}
	if userID > 1 {
		argsOpened = append(argsOpened, userID)
	}
	openedRows, err := p.db.Query(openedQ, argsOpened...)
	if err != nil {
		return nil, err
	}
	defer openedRows.Close()

	type bucket struct {
		criticalOpened, warningOpened       int
		criticalResolved, warningResolved   int
	}
	buckets := map[int64]*bucket{}
	for openedRows.Next() {
		var ts int64
		var crit, warn sql.NullInt64
		if err := openedRows.Scan(&ts, &crit, &warn); err != nil {
			continue
		}
		buckets[ts] = &bucket{
			criticalOpened: int(crit.Int64),
			warningOpened:  int(warn.Int64),
		}
	}
	if err := openedRows.Err(); err != nil {
		return nil, err
	}

	resolvedQ := `SELECT (resolved_at / ?) * ? AS bucket_ts,
	                   SUM(CASE WHEN severity = 'critical' THEN 1 ELSE 0 END),
	                   SUM(CASE WHEN severity = 'warning' THEN 1 ELSE 0 END)
	              FROM incidents i
	             WHERE resolved_at > 0 AND resolved_at >= ? AND resolved_at <= ?
	               ` + userFilter + `
	          GROUP BY bucket_ts
	          ORDER BY bucket_ts ASC`
	argsResolved := []interface{}{bucketSec, bucketSec, from, now}
	if userID > 1 {
		argsResolved = append(argsResolved, userID)
	}
	resolvedRows, err := p.db.Query(resolvedQ, argsResolved...)
	if err != nil {
		return nil, err
	}
	defer resolvedRows.Close()
	for resolvedRows.Next() {
		var ts int64
		var crit, warn sql.NullInt64
		if err := resolvedRows.Scan(&ts, &crit, &warn); err != nil {
			continue
		}
		b, ok := buckets[ts]
		if !ok {
			b = &bucket{}
			buckets[ts] = b
		}
		b.criticalResolved += int(crit.Int64)
		b.warningResolved += int(warn.Int64)
	}

	// Materialize sorted buckets; emit zeros for empty intervals so the
	// chart x-axis stays regular.
	out := make([]IncidentStatsBucket, 0, 64)
	for ts := (from / bucketSec) * bucketSec; ts <= now; ts += bucketSec {
		b, ok := buckets[ts]
		if !ok {
			out = append(out, IncidentStatsBucket{Timestamp: ts})
			continue
		}
		out = append(out, IncidentStatsBucket{
			Timestamp:        ts,
			CriticalOpened:   b.criticalOpened,
			WarningOpened:    b.warningOpened,
			CriticalResolved: b.criticalResolved,
			WarningResolved:  b.warningResolved,
		})
	}
	return out, nil
}

func clampNonNeg(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// joinWhere is a tiny helper used by GetIncidentHistory. Inline string
// concat would work; this keeps the WHERE clause assembly readable.
func joinWhere(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}
