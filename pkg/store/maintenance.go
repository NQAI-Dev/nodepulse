package store

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// CreateMaintenanceWindow inserts a new silence window. The combination of
// (start_unix, end_unix, scope) is what gates alerting — see IsNodeSilenced.
//
// ponytail: if/when we let operators hand in cron expressions ("every Sunday
// 02:00-04:00") replace this struct with a ScheduleRow that resolves to a
// concrete []MaintenanceWindow for the next N hours.
func (p *PersistentStore) CreateMaintenanceWindow(userID int64, req protocol.MaintenanceWindowRequest, createdBy string) (protocol.MaintenanceWindow, error) {
	if req.Scope == "" {
		req.Scope = "user"
	}
	if req.Scope != "user" && req.Scope != "node" {
		return protocol.MaintenanceWindow{}, errors.New("scope must be 'user' or 'node'")
	}
	if req.Scope == "node" && len(req.NodeIDs) == 0 {
		return protocol.MaintenanceWindow{}, errors.New("node_ids required for scope=node")
	}
	if req.EndUnix != 0 && req.EndUnix < req.StartUnix {
		return protocol.MaintenanceWindow{}, errors.New("end_unix before start_unix")
	}

	now := time.Now().Unix()
	nodeCSV := strings.Join(req.NodeIDs, ",")

	p.mu.Lock()
	defer p.mu.Unlock()

	res, err := p.db.Exec(`INSERT INTO maintenance_windows
		(user_id, scope, reason, node_ids, start_unix, end_unix, created_at, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, req.Scope, req.Reason, nodeCSV, req.StartUnix, req.EndUnix, now, createdBy)
	if err != nil {
		return protocol.MaintenanceWindow{}, err
	}
	id, _ := res.LastInsertId()
	return protocol.MaintenanceWindow{
		ID:        id,
		UserID:    userID,
		NodeIDs:   req.NodeIDs,
		Reason:    req.Reason,
		StartUnix: req.StartUnix,
		EndUnix:   req.EndUnix,
		Scope:     req.Scope,
		CreatedAt: now,
		CreatedBy: createdBy,
	}, nil
}

// ListMaintenanceWindows returns every window owned by userID (admin gets
// all rows). Returned rows include closed ones — the UI uses that to render
// the recent-deployments history. Filter closedOnly=true if you want only
// still-open windows (end_unix=0 or in the future).
func (p *PersistentStore) ListMaintenanceWindows(userID int64, closedOnly bool) ([]protocol.MaintenanceWindow, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now().Unix()
	args := []interface{}{}
	q := `SELECT id, user_id, scope, reason, node_ids, start_unix, end_unix, created_at, COALESCE(created_by, '')
	      FROM maintenance_windows`
	where := []string{}
	if userID > 1 {
		where = append(where, "user_id = ?")
		args = append(args, userID)
	}
	if !closedOnly {
		// "active" means either open-ended (end_unix=0) or in the future.
		where = append(where, "(end_unix = 0 OR end_unix > ?)")
		args = append(args, now)
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY start_unix DESC LIMIT 200"

	rows, err := p.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []protocol.MaintenanceWindow{}
	for rows.Next() {
		var w protocol.MaintenanceWindow
		var nodes, scope string
		var endUnix int64
		if err := rows.Scan(&w.ID, &w.UserID, &scope, &w.Reason, &nodes, &w.StartUnix, &endUnix, &w.CreatedAt, &w.CreatedBy); err != nil {
			continue
		}
		w.Scope = scope
		w.EndUnix = endUnix
		if nodes != "" {
			w.NodeIDs = strings.Split(nodes, ",")
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// DeleteMaintenanceWindow removes a window by id. The owning user can only
// delete their own rows; admin (uid<=1) can delete anything. Returns the
// number of rows actually deleted so the caller can return 404 instead of
// silently 200 on a stale id.
func (p *PersistentStore) DeleteMaintenanceWindow(userID, id int64) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if userID <= 1 {
		res, err := p.db.Exec("DELETE FROM maintenance_windows WHERE id = ?", id)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}
	res, err := p.db.Exec("DELETE FROM maintenance_windows WHERE id = ? AND user_id = ?", id, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// IsNodeSilenced reports whether an incident for nodeID owned by userID
// should NOT be dispatched right now. Active window semantics:
//
//   - start_unix <= now (the window has begun; future windows are ignored
//     so operators can pre-stage them without immediately silencing)
//   - end_unix == 0  OR  end_unix > now (still open or scheduled to close later)
//   - matching scope: scope="user" matches every node the user owns;
//     scope="node" matches when nodeID is in the explicit list
//
// Cheap query path: the maintenance_windows table is bounded by the number
// of deployments an operator schedules (typically <100) and the covering
// index keeps the lookup fast. Called on every incident-create, so don't
// add joins or heavy logic here.
func (p *PersistentStore) IsNodeSilenced(userID int64, nodeID string, now int64) (bool, error) {
	if userID <= 0 {
		return false, nil
	}
	if now == 0 {
		now = time.Now().Unix()
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	return p.isNodeSilencedLocked(userID, nodeID, now)
}

// isNodeSilencedLocked is the same lookup without taking the mutex —
// callers that already hold p.mu (notably notifyAfterCreate which runs
// under CreateIncident's lock) MUST use this variant or they will
// deadlock against themselves.
func (p *PersistentStore) isNodeSilencedLocked(userID int64, nodeID string, now int64) (bool, error) {
	if userID <= 0 {
		return false, nil
	}
	if now == 0 {
		now = time.Now().Unix()
	}

	rows, err := p.db.Query(`SELECT scope, node_ids FROM maintenance_windows
		WHERE user_id = ? AND start_unix <= ? AND (end_unix = 0 OR end_unix > ?)`,
		userID, now, now)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var scope, nodes string
		if err := rows.Scan(&scope, &nodes); err != nil {
			continue
		}
		if scope == "user" {
			return true, nil
		}
		if scope == "node" {
			for _, n := range strings.Split(nodes, ",") {
				if n == nodeID {
					return true, nil
				}
			}
		}
	}
	return false, rows.Err()
}

// AutoCloseIncidentsInMaintenance marks any open incident for the given
// (userID, nodeID) as resolved if it's currently inside an active window.
// Reason: while the user has said "we're deploying", an incident that fires
// and then clears during the same window shouldn't sit on the dashboard as
// "open" — it would also block the start-of-next-window re-alert.
//
// Returns the number of incidents that were auto-resolved (purely
// informational for logs/tests).
func (p *PersistentStore) AutoCloseIncidentsInMaintenance(userID int64, nodeID string, now int64) (int64, error) {
	if userID <= 0 {
		return 0, nil
	}
	if now == 0 {
		now = time.Now().Unix()
	}

	silenced, err := p.IsNodeSilenced(userID, nodeID, now)
	if err != nil || !silenced {
		return 0, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Only auto-close *open* incidents. Resolved ones stay resolved.
	res, err := p.db.Exec(`UPDATE incidents SET resolved = 1, resolved_at = ?
		WHERE resolved = 0 AND node_id = ?`,
		now, nodeID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Helper for HTTP layer: parse "id" query param into int64.
func parseIDParam(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}
