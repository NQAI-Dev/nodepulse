package store

import (
	"database/sql"
	"time"
)

// AcknowledgeIncident records an operator acknowledgement on the incident
// row without resolving it. Idempotent: a second call leaves acknowledged_at
// unchanged and reports false. Returns false when no matching row exists or
// the incident is already resolved.
//
// The SELECT-then-UPDATE pattern is intentional: SQLite's RowsAffected()
// counts WHERE matches rather than mutated rows for the COALESCE form, so a
// single UPDATE cannot tell a fresh ack from a no-op ack. The pre-read is
// safe under p.mu.
//
// ponytail: kept separate from ResolveIncident because the alert workflow
// is two-step (ack → resolve), and ack-only carries different semantics for
// on-call paging. If we ever drop ack-only (always-go-resolve), fold this
// into ResolveIncident with an extra ResolvedBy field.
func (p *PersistentStore) AcknowledgeIncident(id string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var existing int64
	err := p.db.QueryRow(
		"SELECT COALESCE(acknowledged_at, 0) FROM incidents WHERE id = ? AND resolved = 0",
		id,
	).Scan(&existing)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if existing != 0 {
		return false, nil
	}
	if _, err := p.db.Exec("UPDATE incidents SET acknowledged_at = ? WHERE id = ?", time.Now().Unix(), id); err != nil {
		return false, err
	}
	return true, nil
}

// IncidentOwner returns the user that owns the node this incident is
// attached to, or 0 when unowned. Used to scope callback handling and
// silence cross-tenant acknowledgement attempts (any owner can act today,
// but the path lets us tighten later).
func (p *PersistentStore) IncidentOwner(id string) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var owner int64
	err := p.db.QueryRow(`
		SELECT o.user_id FROM incidents i
		JOIN node_owners o ON o.node_id = i.node_id
		WHERE i.id = ?`, id).Scan(&owner)
	if err != nil {
		return 0, err
	}
	return owner, nil
}
