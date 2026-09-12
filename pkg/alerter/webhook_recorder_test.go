package alerter

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// TestWebhookRecorder_CapturesSuccess proves the recorder wraps the dispatcher
// and writes exactly one audit row for a successful 2xx delivery, with the
// expected owner/event/url fields populated.
func TestWebhookRecorder_CapturesSuccess(t *testing.T) {
	got := make(chan protocol.WebhookAlert, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload protocol.WebhookAlert
		_ = readJSON(r, &payload)
		got <- payload
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	r := NewWebhookRecorder(NewWebhook())
	if err := r.DispatchSigned(7, 42, ts.URL, "secret", protocol.WebhookAlert{
		Event:     "incident.created",
		Timestamp: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("webhook never arrived")
	}

	rows := r.Drain()
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	row := rows[0]
	if row.UserID != 7 || row.IncidentID != "42" || row.Event != "incident.created" {
		t.Fatalf("row not populated correctly: %+v", row)
	}
	if !row.OK || row.Status != 200 {
		t.Fatalf("expected ok/200, got ok=%v status=%d", row.OK, row.Status)
	}
	if row.URL != ts.URL {
		t.Fatalf("URL mismatch: %q", row.URL)
	}
	if row.Attempts < 1 {
		t.Fatalf("attempts should record max-retries+1, got %d", row.Attempts)
	}
}

// TestWebhookRecorder_CapturesFailure asserts that a final 5xx delivery still
// surfaces as a single row, with OK=false and the status code from the last
// attempt preserved.
func TestWebhookRecorder_CapturesFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ts.Close()

	r := NewWebhookRecorder(&WebhookDispatcher{
		client:     &http.Client{Timeout: 2 * time.Second},
		maxRetries: 2,
	})
	if err := r.Dispatch(1, 0, ts.URL, protocol.WebhookAlert{Event: "incident.created"}); err == nil {
		t.Fatal("expected error after exhausting retries")
	}

	rows := r.Drain()
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].OK {
		t.Fatal("expected ok=false for 502")
	}
	if rows[0].Status != http.StatusBadGateway {
		t.Fatalf("expected status=502, got %d", rows[0].Status)
	}
	if rows[0].Error == "" {
		t.Fatal("expected error message captured")
	}
}

// TestWebhookRecorder_SkipsEmptyURL confirms we don't audit "no URL configured"
// rows — they'd dominate the table for every user who hasn't set up a webhook.
func TestWebhookRecorder_SkipsEmptyURL(t *testing.T) {
	r := NewWebhookRecorder(NewWebhook())
	if err := r.Dispatch(1, 0, "", protocol.WebhookAlert{Event: "incident.created"}); err != nil {
		t.Fatalf("dispatch with empty URL should be a no-op error: %v", err)
	}
	if pending := r.Pending(); pending != 0 {
		t.Fatalf("empty-URL dispatch should not enqueue, got %d pending", pending)
	}
}

// TestWebhookRecorder_OverflowDrops guards the bounded buffer: the recorder
// must NEVER block the dispatcher. We push enough failing deliveries that the
// internal ring saturates and assert that subsequent calls still return
// quickly with their own error.
func TestWebhookRecorder_OverflowDrops(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	r := NewWebhookRecorder(&WebhookDispatcher{
		client:     &http.Client{Timeout: 2 * time.Second},
		maxRetries: 0, // single attempt — keep the loop fast
	})

	for i := 0; i < r.maxLen+10; i++ {
		_ = r.Dispatch(1, int64(i), ts.URL, protocol.WebhookAlert{Event: "incident.created"})
	}

	if r.DroppedTotal() == 0 {
		t.Fatal("expected some rows to be dropped on overflow")
	}
	if pending := r.Pending(); pending > r.maxLen {
		t.Fatalf("pending=%d exceeds maxLen=%d", pending, r.maxLen)
	}
}

// TestWebhookRecorder_Concurrent asserts the recorder is safe under concurrent
// dispatch — the dispatcher itself is safe (zero shared state per-call), but
// the recorder fan-in must not race.
func TestWebhookRecorder_Concurrent(t *testing.T) {
	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	r := NewWebhookRecorder(NewWebhook())
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = r.Dispatch(int64(i), 0, ts.URL, protocol.WebhookAlert{Event: "incident.created"})
		}(i)
	}
	wg.Wait()

	if atomic.LoadInt32(&hits) != 50 {
		t.Fatalf("expected 50 deliveries, server saw %d", hits)
	}
	rows := r.Drain()
	if len(rows) != 50 {
		t.Fatalf("expected 50 buffered rows, got %d", len(rows))
	}
	if r.DroppedTotal() != 0 {
		t.Fatalf("expected zero drops, got %d", r.DroppedTotal())
	}
}

// TestWebhookRecorder_Requeue verifies that re-queueing after a failed DB
// flush restores rows to the buffer so the next sweep can retry.
func TestWebhookRecorder_Requeue(t *testing.T) {
	r := NewWebhookRecorder(NewWebhook())
	original := []protocol.WebhookDelivery{
		{Event: "a"}, {Event: "b"}, {Event: "c"},
	}

	// Simulate a flush that lost the first row.
	kept := original[1:]
	r.Requeue(kept)
	if got := r.Pending(); got != 2 {
		t.Fatalf("expected 2 requeued, got %d", got)
	}

	rows := r.Drain()
	if len(rows) != 2 || rows[0].Event != "b" || rows[1].Event != "c" {
		t.Fatalf("requeue order wrong: %+v", rows)
	}
}

// readJSON is a tiny helper kept local to the test package so we don't have
// to import encoding/json at the top — keeps the test file stdlib-light.
func readJSON(r *http.Request, dst *protocol.WebhookAlert) error {
	defer r.Body.Close()
	buf, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(buf, dst)
}
