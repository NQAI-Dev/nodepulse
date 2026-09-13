package alerter

import (
	"encoding/json"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// WebhookRecorder persists the outcome of every WebhookDispatcher delivery
// into an in-memory ring buffer that the persistent store drains into SQLite.
// The recorder never blocks the dispatcher: writes append under a mutex and
// get coalesced into a single INSERT batch when the store sweeps. Drops are
// counted (not errors) so a misbehaving DB can never wedge live notification
// dispatch — the audit trail is a convenience, not a source of truth.
//
// ponytail: the in-memory buffer + janitor sweep is the cheapest design that
// still surfaces deliveries within minutes. If operators ever demand
// sub-second persistence under sustained incident storms (>10k/min) swap
// this for a bounded worker pool with disk-backed spillover.
type WebhookRecorder struct {
	inner *WebhookDispatcher

	mu      sync.Mutex
	records []protocol.WebhookDelivery // pending writes awaiting drain
	maxLen  int

	droppedTotal atomic.Uint64 // buffer overflows (audit dropped)
	flushedTotal atomic.Uint64 // rows successfully handed to store
}

// NewWebhookRecorder wraps an existing dispatcher. The dispatcher reference
// is preserved so existing call sites that hold a *WebhookDispatcher keep
// working without rewiring.
func NewWebhookRecorder(inner *WebhookDispatcher) *WebhookRecorder {
	return &WebhookRecorder{
		inner:   inner,
		records: make([]protocol.WebhookDelivery, 0, 32),
		maxLen:  1024,
	}
}

// Dispatcher exposes the wrapped dispatcher so the persistent store can call
// it directly for legacy code paths that pre-date the recorder.
func (r *WebhookRecorder) Dispatcher() *WebhookDispatcher { return r.inner }

// DroppedTotal returns the count of audit rows dropped on buffer overflow.
// Exposed for /healthz operators dashboards.
func (r *WebhookRecorder) DroppedTotal() uint64 { return r.droppedTotal.Load() }

// FlushedTotal returns the count of audit rows handed off to the store.
func (r *WebhookRecorder) FlushedTotal() uint64 { return r.flushedTotal.Load() }

// record appends a delivery outcome. Drops silently if the buffer is full.
func (r *WebhookRecorder) record(d protocol.WebhookDelivery) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.records) >= r.maxLen {
		r.droppedTotal.Add(1)
		return
	}
	r.records = append(r.records, d)
}

// Drain returns pending records and clears the buffer. Returns nil if empty.
// The caller is expected to insert them in one transaction; on failure the
// store re-queues them via Requeue so a transient DB hiccup doesn't lose
// audit rows.
func (r *WebhookRecorder) Drain() []protocol.WebhookDelivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.records) == 0 {
		return nil
	}
	out := r.records
	r.records = make([]protocol.WebhookDelivery, 0, 32)
	r.flushedTotal.Add(uint64(len(out)))
	return out
}

// Requeue prepends records back onto the buffer when the store failed to
// persist them. Best-effort: if the buffer is saturated it increments the
// dropped counter so operators at least know rows went missing.
func (r *WebhookRecorder) Requeue(items []protocol.WebhookDelivery) {
	if len(items) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	room := r.maxLen - len(r.records)
	if room <= 0 {
		r.droppedTotal.Add(uint64(len(items)))
		return
	}
	if room < len(items) {
		// Keep only the most recent — older rows are less useful for debugging.
		items = items[len(items)-room:]
		r.droppedTotal.Add(uint64(len(items) - room))
	}
	r.records = append(items, r.records...)
}

// Pending returns the current backlog length.
func (r *WebhookRecorder) Pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.records)
}

// DispatchSigned records the dispatched URL alongside the event payload so the
// audit row carries enough context to answer "which incident triggered this
// webhook?" without joining against another table.
func (r *WebhookRecorder) DispatchSigned(userID, incidentID int64, url, secret string, event protocol.WebhookAlert) error {
	return r.dispatchWithMetadata(userID, incidentID, url, secret, event)
}

// Dispatch is the unsigned convenience wrapper that mirrors WebhookDispatcher.
func (r *WebhookRecorder) Dispatch(userID, incidentID int64, url string, event protocol.WebhookAlert) error {
	return r.dispatchWithMetadata(userID, incidentID, url, "", event)
}

// SendTestWebhook signs and delivers a clearly-labelled test payload. The
// audit row gets a synthetic incident_id "test" so operators can spot it in
// the deliveries dashboard without confusing it with a real incident.
func (r *WebhookRecorder) SendTestWebhook(userID int64, url, secret string) error {
	event := protocol.WebhookAlert{
		Event:     "test",
		Timestamp: time.Now().Unix(),
		Incident: &protocol.Incident{
			Severity:  "info",
			NodeID:    "test",
			Title:     "NodePulse test webhook",
			Detail:    "This is a test delivery — your webhook endpoint works.",
			StartedAt: time.Now().Unix(),
		},
	}
	return r.dispatchWithMetadata(userID, 0, url, secret, event)
}

func (r *WebhookRecorder) dispatchWithMetadata(userID, incidentID int64, url, secret string, event protocol.WebhookAlert) error {
	if url == "" {
		// Empty URL is a config miss, not a delivery. Skip the audit row so
		// the table doesn't fill up with "deliveries to no URL".
		return r.inner.DispatchSigned(url, secret, event)
	}

	startedAt := time.Now()
	err := r.inner.DispatchSigned(url, secret, event)

	// Snapshot the JSON payload alongside the audit row so a manual retry
	// can replay the exact bytes even after the incident is gone or the
	// webhook secret has been rotated. Failure to marshal is non-fatal —
	// we just skip the snapshot and the retry endpoint will 400.
	var payloadJSON string
	if raw, mErr := json.Marshal(event); mErr == nil {
		payloadJSON = string(raw)
	}

	rec := protocol.WebhookDelivery{
		UserID:         userID,
		IncidentID:     strconv.FormatInt(incidentID, 10),
		Event:          event.Event,
		URL:            url,
		Attempts:       r.inner.maxRetries + 1,
		Timestamp:      startedAt.Unix(),
		TotalLatencyMs: time.Since(startedAt).Milliseconds(),
		Payload:        payloadJSON,
	}
	if err == nil {
		rec.OK = true
		rec.Status = 200
	} else {
		rec.Error = err.Error()
		if s, ok := err.(*webhookStatusError); ok {
			rec.Status = s.status
		}
	}
	r.record(rec)
	return err
}
