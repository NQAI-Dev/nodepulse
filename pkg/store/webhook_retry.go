package store

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// ErrWebhookDeliveryNotFound is returned by RetryWebhookDelivery when no row
// matches the requested id for the calling user. The HTTP layer maps this to
// 404 so operator UIs can distinguish "missing" from other failures.
var ErrWebhookDeliveryNotFound = errors.New("webhook delivery not found")

// ErrWebhookDeliveryNoPayload is returned when the stored delivery has no
// captured payload (legacy rows predating the snapshot column). Operators
// can't replay these without re-triggering the originating event.
var ErrWebhookDeliveryNoPayload = errors.New("webhook delivery has no captured payload")

// ErrWebhookDeliveryBadPayload is returned when the captured payload cannot
// be parsed back into a WebhookAlert. Corrupt rows should be visible in the
// audit list and retryable after manual cleanup.
var ErrWebhookDeliveryBadPayload = errors.New("webhook delivery has an unreadable payload")

// RetryWebhookDelivery re-sends the captured payload of a prior webhook
// delivery to its recorded URL and writes the result as a new audit row.
//
// ponytail: signed_payload_optional — the original HMAC signature can't be
// re-derived without the secret used at dispatch time, so the retry signs
// with the *current* webhook_secret from user_settings. If operators ever
// demand bit-identical replays (forensic debug), capture the secret too.
func (p *PersistentStore) RetryWebhookDelivery(userID, deliveryID int64) (*protocol.WebhookDelivery, error) {
	existing, err := p.WebhookDeliveryByID(userID, deliveryID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, ErrWebhookDeliveryNotFound
	}
	if existing.Payload == "" {
		return nil, ErrWebhookDeliveryNoPayload
	}

	var event protocol.WebhookAlert
	if err := json.Unmarshal([]byte(existing.Payload), &event); err != nil {
		return nil, ErrWebhookDeliveryBadPayload
	}

	// Use the current webhook secret for signing — if the operator rotated
	// it after the failed delivery, this is the secret their endpoint now
	// expects. The URL stays the same as the original (recorded in the row).
	settings, err := p.getSettingsLocked(userID)
	if err != nil {
		return nil, err
	}
	incidentID, _ := strconv.ParseInt(existing.IncidentID, 10, 64)

	rec := p.Recorder()
	if rec == nil {
		return nil, errors.New("webhook recorder unavailable")
	}
	// Errors from the dispatcher itself are already captured in the audit row
	// (recorder records success/failure outcome), so we don't propagate it.
	_ = rec.DispatchSigned(userID, incidentID, existing.URL, settings.WebhookSecret, event)

	// The recorder buffers audit rows in memory until the janitor sweeps
	// them. For a manual retry the operator expects to see the outcome
	// immediately, so drain the recorder and flush the freshly written
	// entries straight to SQLite.
	if pending := rec.Drain(); len(pending) > 0 {
		if _, err := p.FlushWebhookDeliveries(pending); err != nil {
			// Flush failure is non-fatal: the recorder will requeue on its
			// next janitor pass. Continue so we still surface what we have.
		}
	}

	// Return the most recent delivery row for this user so the operator UI
	// can show the new outcome without a second round-trip.
	rows, err := p.WebhookDeliveries(userID, 1)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}
