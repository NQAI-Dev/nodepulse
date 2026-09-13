package store

import (
	"database/sql"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// FlushWebhookDeliveries persists every record handed in inside one
// transaction. On error no rows are written; the caller is expected to
// re-queue via recorder.Requeue so we don't lose audit rows on a transient
// SQLite hiccup. Returns the number of rows written.
//
// ponytail: a single INSERT ... VALUES (?), (?), ... is faster than a
// per-row prepared statement under load, but bloat the SQL for very large
// batches. Cap batches at 256 in the recorder; if you ever raise it, split
// into chunked transactions.
func (p *PersistentStore) FlushWebhookDeliveries(items []protocol.WebhookDelivery) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	tx, err := p.db.Begin()
	if err != nil {
		return 0, err
	}
	stmt, err := tx.Prepare(`INSERT INTO webhook_deliveries
		(user_id, incident_id, event, url, attempts, status, ok, error, latency_ms, total_latency_ms, ts, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}

	written := 0
	for _, d := range items {
		ok := 0
		if d.OK {
			ok = 1
		}
		if _, err := stmt.Exec(
			d.UserID,
			d.IncidentID,
			d.Event,
			d.URL,
			d.Attempts,
			d.Status,
			ok,
			d.Error,
			d.LatencyMs,
			d.TotalLatencyMs,
			d.Timestamp,
			d.Payload,
		); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return written, err
		}
		written++
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		return written, err
	}
	if err := tx.Commit(); err != nil {
		return written, err
	}
	return written, nil
}

// WebhookDeliveries returns the most recent deliveries for a user across all
// their webhook URLs. limit clamps to [1, 500] for sane operator pages.
func (p *PersistentStore) WebhookDeliveries(userID int64, limit int) ([]protocol.WebhookDelivery, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := p.db.Query(`SELECT id, user_id, incident_id, event, url, attempts,
		status, ok, COALESCE(error, ''), latency_ms, total_latency_ms, ts, COALESCE(payload, '')
		FROM webhook_deliveries WHERE user_id = ?
		ORDER BY id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWebhookDeliveries(rows)
}

// WebhookDeliveryByID returns a single delivery scoped to userID. Returns
// (nil, nil) if no row matches — callers map that to 404 so the operator UI
// can distinguish "missing" from "belongs to someone else".
func (p *PersistentStore) WebhookDeliveryByID(userID, id int64) (*protocol.WebhookDelivery, error) {
	rows, err := p.db.Query(`SELECT id, user_id, incident_id, event, url, attempts,
		status, ok, COALESCE(error, ''), latency_ms, total_latency_ms, ts, COALESCE(payload, '')
		FROM webhook_deliveries WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items, err := scanWebhookDeliveries(rows)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	return &items[0], nil
}

// WebhookDeliveryStats returns aggregate counts over the last `seconds`
// seconds for the operator dashboard: total attempts, successful deliveries,
// failures, and the count of distinct URLs that fired.
func (p *PersistentStore) WebhookDeliveryStats(userID int64, seconds int64) (map[string]interface{}, error) {
	if seconds <= 0 {
		seconds = 86400
	}
	if seconds > 7*24*3600 {
		seconds = 7 * 24 * 3600
	}
	cutoff := time.Now().Unix() - seconds

	var total, ok int64
	if err := p.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(ok), 0)
		FROM webhook_deliveries WHERE user_id = ? AND ts >= ?`, userID, cutoff).Scan(&total, &ok); err != nil {
		return nil, err
	}
	var urls int64
	if err := p.db.QueryRow(`SELECT COUNT(DISTINCT url) FROM webhook_deliveries
		WHERE user_id = ? AND ts >= ?`, userID, cutoff).Scan(&urls); err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"window_seconds": seconds,
		"total":          total,
		"ok":             ok,
		"failed":         total - ok,
		"distinct_urls":  urls,
	}, nil
}

func scanWebhookDeliveries(rows *sql.Rows) ([]protocol.WebhookDelivery, error) {
	var out []protocol.WebhookDelivery
	for rows.Next() {
		d := protocol.WebhookDelivery{}
		var ok int
		if err := rows.Scan(
			&d.ID, &d.UserID, &d.IncidentID, &d.Event, &d.URL, &d.Attempts,
			&d.Status, &ok, &d.Error, &d.LatencyMs, &d.TotalLatencyMs, &d.Timestamp, &d.Payload,
		); err != nil {
			return nil, err
		}
		d.OK = ok != 0
		out = append(out, d)
	}
	return out, rows.Err()
}
