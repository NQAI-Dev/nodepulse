package store

import (
	"database/sql"
	"errors"
	"time"
)

// ErrSnoozeDurationInvalid is returned by SnoozeIncident when the duration
// is not one of the canonical keys (1h / 4h / 8h) or the underlying
// conversion to seconds fails. The callback handler maps this to 400 so the
// operator UI can echo "invalid duration" instead of "internal error".
var ErrSnoozeDurationInvalid = errors.New("snooze duration must be one of 1h/4h/8h")

// ErrSnoozeIncidentNotFound is returned when no matching open incident
// exists. Mirrors AcknowledgeIncident's not-found semantics so the callback
// handler can answer the Telegram spinner with "already resolved or not
// found" without needing to inspect the underlying error string.
var ErrSnoozeIncidentNotFound = errors.New("incident not found or already resolved")

// SnoozeIncident sets snoozed_until = now + seconds on an open incident and
// returns the timestamp that was persisted (unix seconds). Idempotent:
// re-snoozing an already-snoozed incident extends the window rather than
// resetting from now, so a sequence of clicks produces a single operator
// intent (the latest one wins) without surprising double-windows.
//
// seconds must be positive and <= 24h; longer windows are clamped to 24h
// so a fat-fingered button can't mute an incident for a week. Returns
// ErrSnoozeIncidentNotFound when the id is unknown or already resolved,
// and ErrSnoozeDurationInvalid when seconds is out of range.
func (p *PersistentStore) SnoozeIncident(id string, seconds int64) (int64, error) {
	if seconds <= 0 || seconds > 24*3600 {
		return 0, ErrSnoozeDurationInvalid
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	var resolved int
	err := p.db.QueryRow(
		"SELECT resolved FROM incidents WHERE id = ?", id,
	).Scan(&resolved)
	if err == sql.ErrNoRows {
		return 0, ErrSnoozeIncidentNotFound
	}
	if err != nil {
		return 0, err
	}
	if resolved == 1 {
		return 0, ErrSnoozeIncidentNotFound
	}

	until := time.Now().Unix() + seconds
	// last_notified_at is bumped too so the cooldown gate sees a recent
	// notification and stays quiet until snoozed_until expires.
	if _, err := p.db.Exec(
		"UPDATE incidents SET snoozed_until = ?, last_notified_at = ? WHERE id = ?",
		until, time.Now().Unix(), id,
	); err != nil {
		return 0, err
	}
	return until, nil
}

// IncidentSnoozedUntil returns the snoozed_until timestamp for an incident
// in unix seconds, or 0 when the row is missing / not snoozed. Read-only,
// safe for concurrent calls (uses COALESCE so legacy rows without the column
// return 0 even on an unmigrated DB).
//
// ponytail: if snoozes ever become per-user (different operators snoozing
// the same incident with different windows), promote this to a join on
// incident_snoozes.
func (p *PersistentStore) IncidentSnoozedUntil(id string) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	var until int64
	if err := p.db.QueryRow(
		"SELECT COALESCE(snoozed_until, 0) FROM incidents WHERE id = ?", id,
	).Scan(&until); err != nil {
		return 0
	}
	return until
}
