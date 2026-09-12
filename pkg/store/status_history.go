package store

import (
	"strconv"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// GetPublicIncidentHistory returns the most recent incidents across the
// whole fleet. Powers the public status timeline at /api/v1/public/incidents
// and the Atom/RSS status feed. Caller controls the range via `sinceUnix`;
// results are capped at `limit` (default 200, hard ceiling 500) to keep the
// public payload bounded.
//
// ponytail: today the query scans idx_incidents_history (started_at, resolved).
// If the table grows past ~1M rows, add a partial index on started_at WHERE
// resolved = 1 to keep resolved-row scans cheap.
func (p *PersistentStore) GetPublicIncidentHistory(sinceUnix int64, limit int) ([]protocol.PublicIncidentHistory, error) {
	if p == nil || p.db == nil {
		return nil, nil
	}
	if sinceUnix <= 0 {
		sinceUnix = time.Now().Unix() - int64(90*24*3600)
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}

	rows, err := p.db.Query(
		`SELECT id, node_id, severity, title, started_at, resolved, COALESCE(resolved_at, 0)
		   FROM incidents
		  WHERE started_at >= ?
		  ORDER BY started_at DESC
		  LIMIT ?`,
		sinceUnix, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]protocol.PublicIncidentHistory, 0, 64)
	for rows.Next() {
		var id int64
		var e protocol.PublicIncidentHistory
		var res int
		if err := rows.Scan(&id, &e.NodeID, &e.Severity, &e.Title, &e.StartedAt, &res, &e.ResolvedAt); err != nil {
			continue
		}
		e.ID = strconv.FormatInt(id, 10)
		e.Resolved = res == 1
		if e.Resolved && e.ResolvedAt == 0 {
			e.ResolvedAt = e.StartedAt
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

