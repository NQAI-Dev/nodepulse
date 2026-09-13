package store

import (
	"context"
	"log"
	"time"
)

const (
	// incidentRetentionDays is how long resolved incident rows are kept
	// before the janitor sweeps them. Open (unresolved) incidents are
	// never pruned.
	incidentRetentionDays = 90
	// webhookDeliveryRetentionDays bounds the webhook_deliveries audit
	// table. Operators care about the last few weeks for post-mortems; the
	// recorder guarantees nothing operational depends on older rows.
	webhookDeliveryRetentionDays = 30
	// janitorInterval is how often the background sweeper runs.
	janitorInterval = 1 * time.Hour
)

// IncidentsPrunedTotal is incremented whenever the janitor deletes a row.
// Exported for tests + operator metrics; safe to read without a lock.
var IncidentsPrunedTotal int64
var MaintenanceExpiredTotal int64
var WebhookDeliveriesPrunedTotal int64
var WebhookDeliveriesFlushedTotal int64

// RunJanitor blocks until ctx is cancelled, sweeping expired data every
// janitorInterval. Safe to call once at server start; the work is cheap
// (indexed DELETE) and idempotent.
//
// ponytail: if the dataset ever grows past ~10M rows, switch this to a
// paginated DELETE with LIMIT and a progress check so a single sweep
// can't pin the DB.
func (p *PersistentStore) RunJanitor(ctx context.Context) {
	t := time.NewTicker(janitorInterval)
	defer t.Stop()

	// Run once immediately so a fresh process catches up on backlog.
	p.runJanitorPass()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.runJanitorPass()
		}
	}
}

func (p *PersistentStore) runJanitorPass() {
	if p == nil || p.db == nil {
		return
	}

	// Sweep abandoned container-stopped incidents older than
	// abandonedContainerTTL even when the node is silent. Per-heartbeat
	// resolution only sees rows for live nodes, so without this a silent
	// node pins the public status at "outage" forever.
	if n := p.ResolveAbandonedAcrossFleet(); n > 0 {
		log.Printf("[janitor] auto-resolved %d abandoned incidents", n)
	}

	cutoff := time.Now().Add(-incidentRetentionDays * 24 * time.Hour).Unix()

	// Resolved incidents older than the retention window.
	if res, err := p.db.Exec(
		`DELETE FROM incidents WHERE resolved = 1 AND resolved_at > 0 AND resolved_at < ?`,
		cutoff,
	); err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			IncidentsPrunedTotal += n
			log.Printf("[janitor] pruned %d incidents older than %d days", n, incidentRetentionDays)
		}
	} else {
		log.Printf("[janitor] incidents prune error: %v", err)
	}

	// Maintenance windows whose end is already in the past are noise;
	// IsNodeSilenced filters them out at query time, but the table still
	// grows forever without this sweep.
	if res, err := p.db.Exec(
		`DELETE FROM maintenance_windows WHERE end_unix > 0 AND end_unix < ?`,
		time.Now().Unix(),
	); err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			MaintenanceExpiredTotal += n
			log.Printf("[janitor] expired %d maintenance windows", n)
		}
	} else {
		log.Printf("[janitor] maintenance prune error: %v", err)
	}

	// Drain the in-memory webhook audit buffer first so rows don't pile up
	// between sweeps; SQLite then handles the durable half. On DB error we
	// re-queue so the next pass (within janitorInterval) retries.
	if p.whRecorder != nil && p.whRecorder.Pending() > 0 {
		pending := p.whRecorder.Drain()
		if written, err := p.FlushWebhookDeliveries(pending); err != nil {
			log.Printf("[janitor] webhook flush error: %v (re-queueing %d rows)", err, len(pending))
			p.whRecorder.Requeue(pending)
		} else if written > 0 {
			WebhookDeliveriesFlushedTotal += int64(written)
			log.Printf("[janitor] flushed %d webhook deliveries", written)
		}
	}

	// Bound the audit trail at 30 days. Failed deliveries worth a closer
	// look typically get noticed (and screenshotted) within hours; older
	// rows are noise.
	if res, err := p.db.Exec(
		`DELETE FROM webhook_deliveries WHERE ts < ?`,
		time.Now().Add(-webhookDeliveryRetentionDays*24*time.Hour).Unix(),
	); err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			WebhookDeliveriesPrunedTotal += n
			log.Printf("[janitor] pruned %d webhook deliveries older than %d days", n, webhookDeliveryRetentionDays)
		}
	} else {
		log.Printf("[janitor] webhook deliveries prune error: %v", err)
	}
}
