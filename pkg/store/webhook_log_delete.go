package store

import "database/sql"

// DeleteWebhookDelivery removes a single webhook audit row scoped to the
// calling user. Returns true when a row was deleted, false when no matching
// row was found (HTTP layer reports `deleted: N` so the operator UI can
// treat "already gone" as success without a 404 round-trip).
//
// ponytail: we delete instead of soft-marking because the audit table is
// firehose-shaped and retention is operator-driven. If compliance ever
// demands a paper trail, add a `deleted_at` column and a janitor sweep
// instead of changing this function.
func (p *PersistentStore) DeleteWebhookDelivery(userID, id int64) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	res, err := p.db.Exec(`DELETE FROM webhook_deliveries WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
