package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
	"github.com/NQAI-Dev/nodepulse/pkg/store"
)

// newTestBillingWebhookStore spins up a PersistentStore with in-memory
// SQLite + billing schema + a registered user, then seeds an
// "already-issued" invoice via SaveInvoice so the handler has a row
// to mark paid. Mirrors newTestBillingStore from billing_handlers_test.go
// (kept separate because the assertions here care about the full
// users.plan / users.pro_until side-effect, not just the invoices row).
//
// Returns (store, uid, token, seeded-invoice-id). The seeded invoice
// mirrors the create-invoice path: a user requested an upgrade, the
// handler saved the row, then they completed payment via CryptoBot and
// the webhook is now arriving.
func newTestBillingWebhookStore(t *testing.T) (*store.PersistentStore, int64, string, string) {
	t.Helper()
	s, err := store.NewPersistentStore(":memory:", "test-bot-token", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}
	if err := s.InitBillingSchema(); err != nil {
		t.Fatalf("InitBillingSchema: %v", err)
	}
	uid, tok, err := s.Register("webhooktest", "hunter22pass")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	const seeded = "999777"
	if err := s.SaveInvoice(seeded, uid, "pro", "5.00", "https://t.me/CryptoBot?start=stub-invoice-999777"); err != nil {
		t.Fatalf("SaveInvoice seed: %v", err)
	}
	return s, uid, tok, seeded
}

// paidPayload builds a valid CryptoBotWebhook-shaped body for the given
// invoice_id. The handler only inspects Status + InvoiceID (Payload
// field is the user_id, Amount/Asset unused on the webhook path —
// CryptoBot doesn't echo them in a way that affects our behavior), so
// a minimal struct is enough.
func paidPayload(invoiceID int64) *protocol.CryptoBotWebhook {
	hook := &protocol.CryptoBotWebhook{UpdateID: 12345}
	hook.Payload.InvoiceID = invoiceID
	hook.Payload.Status = "paid"
	hook.Payload.Payload = "2"
	hook.Payload.Amount = "5.00"
	hook.Payload.Asset = "USDT"
	return hook
}

// TestBillingWebhook_HappyPath pins the success path: a paid webhook
// for a previously-issued invoice → 200 + `{ok:true}` + user upgraded
// to pro + pro_until set ~30 days out.
//
// Without this test, a future refactor that breaks the response shape
// (e.g. returns the wrong JSON key) would silently break the payment
// pipeline — CryptoBot would see something other than {ok:true} and
// either retry forever or report a failure to the user.
func TestBillingWebhook_HappyPath(t *testing.T) {
	s, uid, _, seeded := newTestBillingWebhookStore(t)

	body, _ := json.Marshal(paidPayload(999777))
	req := httptest.NewRequest("POST", "/api/v1/billing/webhook", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	handleBillingWebhook(w, req, s)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("expected ok:true marker, got %q", w.Body.String())
	}

	// Verify the user upgraded to pro.
	var plan string
	var proUntil string
	if err := s.DB().QueryRow("SELECT plan, COALESCE(pro_until, '') FROM users WHERE id = ?", uid).Scan(&plan, &proUntil); err != nil {
		t.Fatalf("select user: %v", err)
	}
	if plan != "pro" {
		t.Errorf("plan: got %q want pro", plan)
	}
	if proUntil == "" {
		t.Errorf("pro_until: empty (expected ~30 days out)")
	}

	// Verify the invoice row is stamped paid.
	var status string
	var paidAt string
	if err := s.DB().QueryRow("SELECT status, COALESCE(paid_at, '') FROM invoices WHERE invoice_id = ?", seeded).Scan(&status, &paidAt); err != nil {
		t.Fatalf("select invoice: %v", err)
	}
	if status != "paid" {
		t.Errorf("invoice status: got %q want paid", status)
	}
	if paidAt == "" {
		t.Errorf("paid_at empty (expected a CURRENT_TIMESTAMP stamp)")
	}
}

// TestBillingWebhook_AlreadyPaid_Idempotent pins the idempotency
// guarantee: webhook retries from CryptoBot against the same invoice
// must NOT extend pro_until by 30 days again. MarkInvoicePaid has a
// `paid_at.Valid` early-return for already-paid invoices; this test
// makes sure the handler routes through that branch on a real-shaped
// double-call instead of silently moving the user's paid window by
// retry storms.
//
// The 30-day extension is timed relative to the original paid_at via
// `COALESCE(datetime(pro_until, '+30 days'), datetime('now', '+30 days'))`
// — so a retry storm against an already-paid invoice would stack 30
// days of pro_until per retry, corrupting the user's paid window. We
// simply assert pro_until is BYTE-IDENTICAL between the two calls: the
// idempotency branch returns BEFORE the users UPDATE, so no format
// ambiguity (sqlite CURRENT_TIMESTAMP vs RFC3339 vs ISO-space-separated)
// can mask a broken contract — if the strings differ, the UPDATE
// happened twice.
func TestBillingWebhook_AlreadyPaid_Idempotent(t *testing.T) {
	s, uid, _, _ := newTestBillingWebhookStore(t)

	body, _ := json.Marshal(paidPayload(999777))

	// First webhook: T0.
	req := httptest.NewRequest("POST", "/api/v1/billing/webhook", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	handleBillingWebhook(w, req, s)
	if w.Code != http.StatusOK {
		t.Fatalf("first call: got %d want 200", w.Code)
	}
	var proUntil1 string
	if err := s.DB().QueryRow("SELECT pro_until FROM users WHERE id = ?", uid).Scan(&proUntil1); err != nil {
		t.Fatalf("select pro_until #1: %v", err)
	}
	if proUntil1 == "" {
		t.Fatalf("pro_until empty after first webhook (handler took the wrong branch)")
	}

	// Sleep long enough that the clock would visibly advance across a
	// broken idempotency contract — a real retry storm could see
	// seconds-to-minutes between attempts. 250ms is plenty.
	time.Sleep(250 * time.Millisecond)

	req2 := httptest.NewRequest("POST", "/api/v1/billing/webhook", strings.NewReader(string(body)))
	w2 := httptest.NewRecorder()
	handleBillingWebhook(w2, req2, s)
	if w2.Code != http.StatusOK {
		t.Fatalf("second call: got %d want 200", w2.Code)
	}
	var proUntil2 string
	if err := s.DB().QueryRow("SELECT pro_until FROM users WHERE id = ?", uid).Scan(&proUntil2); err != nil {
		t.Fatalf("select pro_until #2: %v", err)
	}

	if proUntil1 != proUntil2 {
		t.Errorf("pro_until changed across identical webhook: %q -> %q — idempotency broken (a retry storm would stack 30-day extensions on the user's paid window)", proUntil1, proUntil2)
	}
}

// TestBillingWebhook_MarkInvoicePaidFailure is THE regression: forcing
// MarkInvoicePaid to error (by dropping invoices) must surface as 500
// so CryptoBot retries, NOT as a silent 200. The pre-fix behaviour
// would have been: user pays, webhook arrives, lookup fails with
// "no such table: invoices", handler logs error, responds
// {"ok":true}, CryptoBot stops retrying, user silently never upgrades
// despite having paid.
//
// Asymmetric pre-fix assertion would fail on (a) status != 500,
// (b) body containing {"ok":true}, and (c) absence of the structured
// 500 error marker. All three trip the same code path: the new
// `if err != nil { http.Error(w, ..., 500); return }` block.
func TestBillingWebhook_MarkInvoicePaidFailure(t *testing.T) {
	s, uid, _, _ := newTestBillingWebhookStore(t)

	// Drop invoices to force MarkInvoicePaid's SELECT to fail. Same
	// DROP-TABLE pattern as the bind-node / autoheal-log / create-invoice
	// regression tests — exercises the schema-drift failure mode
	// without fakes.
	if _, err := s.DB().Exec("DROP TABLE invoices"); err != nil {
		t.Fatalf("DROP TABLE invoices: %v", err)
	}

	body, _ := json.Marshal(paidPayload(999777))
	req := httptest.NewRequest("POST", "/api/v1/billing/webhook", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	handleBillingWebhook(w, req, s)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500 (pre-fix silent 200 would have orphaned the payment)", w.Code)
	}
	if strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("failure response leaked ok:true marker — CryptoBot would have stopped retrying: %q", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "internal error") {
		t.Errorf("expected 'internal error' marker so CryptoBot treats as retryable, got %q", w.Body.String())
	}

	// Defence in depth: user must NOT have been upgraded to pro (the
	// handler took the failure path BEFORE the user UPDATE runs).
	var plan string
	if err := s.DB().QueryRow("SELECT plan FROM users WHERE id = ?", uid).Scan(&plan); err != nil {
		t.Fatalf("select user: %v", err)
	}
	if plan == "pro" {
		t.Errorf("user upgraded to pro despite webhook failure (MarkInvoicePaid returned uid=%d but the invoices row is gone, so this should never have completed)", uid)
	}
}

// TestBillingWebhook_InvoiceNotFound pins the orphaned-payment path:
// webhook arrives for an invoice_id that was never inserted in our DB
// (e.g. the create-invoice handler pre-`d699a30` returned 200 with the
// pay URL but silently dropped the SaveInvoice row). Today this can
// only happen for invoices created before the fix landed; future code
// cannot reintroduce it. The handler must still return 500 so the
// alarm fires — operator can then refund or manually upgrade the user.
func TestBillingWebhook_InvoiceNotFound(t *testing.T) {
	s, _, _, _ := newTestBillingWebhookStore(t)

	body, _ := json.Marshal(paidPayload(424242)) // never inserted
	req := httptest.NewRequest("POST", "/api/v1/billing/webhook", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	handleBillingWebhook(w, req, s)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500 (orphaned payment must alarm, not silent-200)", w.Code)
	}
	if strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("not-found case leaked ok:true marker: %q", w.Body.String())
	}
}

// TestBillingWebhook_NonPaidStatusIgnored pins that webhook deliveries
// for non-paid statuses (active, expired) are accepted with 200 but do
// NOT trigger MarkInvoicePaid. Without this, the handler would either
// fire MarkInvoicePaid on every webhook (over-extending pro_until on
// "active" pings) or block the payment pipeline by erroring on
// intermediate-status notifications.
func TestBillingWebhook_NonPaidStatusIgnored(t *testing.T) {
	s, uid, _, seeded := newTestBillingWebhookStore(t)

	hook := paidPayload(999777)
	hook.Payload.Status = "active" // not "paid"
	body, _ := json.Marshal(hook)
	req := httptest.NewRequest("POST", "/api/v1/billing/webhook", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	handleBillingWebhook(w, req, s)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (non-paid webhooks should be 200 no-op), body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("expected ok:true marker, got %q", w.Body.String())
	}

	// User must NOT have been upgraded.
	var plan string
	if err := s.DB().QueryRow("SELECT plan FROM users WHERE id = ?", uid).Scan(&plan); err != nil {
		t.Fatalf("select user: %v", err)
	}
	if plan == "pro" {
		t.Errorf("user upgraded to pro on a non-paid webhook (status=%q)", hook.Payload.Status)
	}

	// Invoice row must still be in 'created' status, untouched.
	var status string
	if err := s.DB().QueryRow("SELECT status FROM invoices WHERE invoice_id = ?", seeded).Scan(&status); err != nil {
		t.Fatalf("select invoice: %v", err)
	}
	if status == "paid" {
		t.Errorf("invoice stamped paid on non-paid webhook (status=%q)", hook.Payload.Status)
	}
}

// TestBillingWebhook_MalformedJSON pins the decode-error path:
// non-JSON body → 400 with `bad request` marker. Without this, a
// misbehaving provider retry storm with corrupted bodies could fill
// logs with stack traces and never return a status code the caller
// could react to.
func TestBillingWebhook_MalformedJSON(t *testing.T) {
	s, _, _, _ := newTestBillingWebhookStore(t)

	req := httptest.NewRequest("POST", "/api/v1/billing/webhook", strings.NewReader("<html>not json</html>"))
	w := httptest.NewRecorder()
	handleBillingWebhook(w, req, s)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400, body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "bad request") {
		t.Errorf("expected 'bad request' marker, got %q", w.Body.String())
	}
}

// TestBillingWebhook_EmptyBody pins the empty-body failure path:
// CryptoBot health checks occasionally send POSTs with no body; the
// handler must respond 400 (decode fails), not crash, not silently
// treat as a paid webhook.
func TestBillingWebhook_EmptyBody(t *testing.T) {
	s, _, _, _ := newTestBillingWebhookStore(t)

	req := httptest.NewRequest("POST", "/api/v1/billing/webhook", strings.NewReader(""))
	w := httptest.NewRecorder()
	handleBillingWebhook(w, req, s)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 (empty body should fail JSON decode), body=%q", w.Code, w.Body.String())
	}
}
