package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// seedDelivery inserts a delivery row directly so we don't depend on the
// dispatcher's HTTP behaviour to exercise RetryWebhookDelivery.
func seedDelivery(t *testing.T, s *PersistentStore, d protocol.WebhookDelivery) int64 {
	t.Helper()
	written, err := s.FlushWebhookDeliveries([]protocol.WebhookDelivery{d})
	if err != nil {
		t.Fatalf("seed: flush: %v", err)
	}
	if written != 1 {
		t.Fatalf("seed: expected 1 row written, got %d", written)
	}
	rows, err := s.WebhookDeliveries(d.UserID, 1)
	if err != nil || len(rows) == 0 {
		t.Fatalf("seed: cannot read back inserted row: %v", err)
	}
	return rows[0].ID
}

func TestRetryWebhookDelivery_NotFound(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "test.db"), "", 0)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	_, err = s.RetryWebhookDelivery(7, 99999)
	if err != ErrWebhookDeliveryNotFound {
		t.Fatalf("expected ErrWebhookDeliveryNotFound, got %v", err)
	}
}

func TestRetryWebhookDelivery_NoPayload(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "test.db"), "", 0)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	id := seedDelivery(t, s, protocol.WebhookDelivery{
		UserID:    7,
		Event:     "incident.created",
		URL:       "https://hook.example/x",
		Status:    502,
		OK:        false,
		Error:     "boom",
		Timestamp: time.Now().Unix(),
		// Payload intentionally empty
	})
	_, err = s.RetryWebhookDelivery(7, id)
	if err != ErrWebhookDeliveryNoPayload {
		t.Fatalf("expected ErrWebhookDeliveryNoPayload, got %v", err)
	}
}

func TestRetryWebhookDelivery_BadPayload(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "test.db"), "", 0)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	id := seedDelivery(t, s, protocol.WebhookDelivery{
		UserID:    7,
		Event:     "incident.created",
		URL:       "https://hook.example/x",
		Status:    502,
		OK:        false,
		Error:     "boom",
		Payload:   "not-json{",
		Timestamp: time.Now().Unix(),
	})
	_, err = s.RetryWebhookDelivery(7, id)
	if err != ErrWebhookDeliveryBadPayload {
		t.Fatalf("expected ErrWebhookDeliveryBadPayload, got %v", err)
	}
}

func TestRetryWebhookDelivery_OtherUserForbidden(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "test.db"), "", 0)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	// Row belongs to user 7; user 9 must not be able to retry it.
	id := seedDelivery(t, s, protocol.WebhookDelivery{
		UserID:    7,
		Event:     "incident.created",
		URL:       "https://hook.example/x",
		Status:    502,
		OK:        false,
		Error:     "boom",
		Payload:   `{"event":"incident.created","timestamp":1}`,
		Timestamp: time.Now().Unix(),
	})
	_, err = s.RetryWebhookDelivery(9, id)
	if err != ErrWebhookDeliveryNotFound {
		t.Fatalf("expected ErrWebhookDeliveryNotFound (cross-user isolation), got %v", err)
	}
}

// TestRetryWebhookDelivery_DispatchesAndAudits verifies the happy path:
// valid payload + URL pointing at a server we control. We spin up an
// in-process httptest.Server, point the audit row at it, and assert that
// the retry:
//   - performs a real HTTP POST,
//   - records a fresh audit row,
//   - returns the new row,
//   - leaves the original failed row intact for historical context.
func TestRetryWebhookDelivery_DispatchesAndAudits(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "test.db"), "", 0)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	var hits int
	srv := newWebhookCaptureServer(&hits, 200)
	defer srv.Close()

	payload := `{"event":"incident.created","timestamp` + `":42,"incident":{"id":"inc-1","severity":"warning","title":"t"}}`
	origID := seedDelivery(t, s, protocol.WebhookDelivery{
		UserID:        7,
		IncidentID:    "1",
		Event:         "incident.created",
		URL:           srv.URL,
		Attempts:      4,
		Status:        502,
		OK:            false,
		Error:         "webhook returned status 502",
		TotalLatencyMs: 1234,
		Timestamp:     time.Now().Unix() - 60,
		Payload:       payload,
	})

	newRow, err := s.RetryWebhookDelivery(7, origID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if newRow == nil {
		t.Fatal("retry returned nil row")
	}
	if newRow.ID == origID {
		t.Fatalf("retry wrote a new audit row; expected different ID, got %d", newRow.ID)
	}
	if !newRow.OK {
		t.Fatalf("expected retry to succeed; got ok=false error=%q status=%d", newRow.Error, newRow.Status)
	}
	if newRow.URL != srv.URL {
		t.Fatalf("retry used wrong URL: got %q", newRow.URL)
	}
	if hits != 1 {
		t.Fatalf("expected capture server to receive exactly 1 POST, got %d", hits)
	}

	// Original row must still be in the audit trail.
	orig, err := s.WebhookDeliveryByID(7, origID)
	if err != nil || orig == nil {
		t.Fatalf("original row disappeared after retry: %v", err)
	}
	if orig.OK {
		t.Fatal("original row was overwritten; should keep its ok=false history")
	}
}

func TestWebhookDeliveryByID_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "test.db"), "", 0)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	want := protocol.WebhookDelivery{
		UserID:        7,
		IncidentID:    "42",
		Event:         "incident.created",
		URL:           "https://hook.example/x",
		Attempts:      4,
		Status:        502,
		OK:            false,
		Error:         "boom",
		TotalLatencyMs: 999,
		Timestamp:     time.Now().Unix(),
		Payload:       `{"event":"incident.created"}`,
	}
	id := seedDelivery(t, s, want)

	got, err := s.WebhookDeliveryByID(7, id)
	if err != nil {
		t.Fatalf("by id: %v", err)
	}
	if got == nil {
		t.Fatal("expected row, got nil")
	}
	if got.Payload != want.Payload {
		t.Fatalf("payload not round-tripped: got %q want %q", got.Payload, want.Payload)
	}
	if got.URL != want.URL || got.Status != want.Status || got.Error != want.Error {
		t.Fatalf("row mismatch: %+v", got)
	}

	if _, err := s.WebhookDeliveryByID(7, 999999); err != nil {
		t.Fatalf("missing row should return nil err, got %v", err)
	}
	if _, err := s.WebhookDeliveryByID(9, id); err != nil {
		t.Fatalf("cross-user lookup should return nil err, got %v", err)
	}
}
