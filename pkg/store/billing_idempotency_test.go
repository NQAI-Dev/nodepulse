package store

import (
	"strings"
	"testing"
	"time"
)

// waitOneSecForTimestampMove sleeps long enough that a naive UPDATE ...
// CURRENT_TIMESTAMP call would land in a different second. SQLite's
// CURRENT_TIMESTAMP has 1-second resolution, so without this gap the
// pre-fix MarkInvoicePaid (which always re-stamped) would happen to
// produce the same value as the first call and slip past the assertion.
func waitOneSecForTimestampMove(t *testing.T) {
	t.Helper()
	time.Sleep(1100 * time.Millisecond)
}

// TestMarkInvoicePaid_Idempotency protects against a real billing bug:
// payment providers (YooKassa, CryptoBot, Stripe, etc.) retry the webhook
// on non-2xx. If MarkInvoicePaid always anchors pro_until to "now + 30 days"
// instead of extending from the existing pro_until, a flaky network could
// chop the user's paid window every time the provider re-tries — the user
// paid once and ends up with a subscription that decays with each retry.
//
// This test exercises the path twice (with a 1.1s gap so the underlying
// datetime('now') moves into a different second, defeating any false-pass
// from naive timestamp equality) and asserts that the second call does
// not reset pro_until or re-stamp paid_at.
func TestMarkInvoicePaid_Idempotency(t *testing.T) {
	s := newBillingStore(t)

	uid, _, err := s.Register("billing-idempotent-user", "secret123")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.SaveInvoice("inv-1", uid, "pro", "5.00", "https://pay/1"); err != nil {
		t.Fatalf("save invoice: %v", err)
	}

	if _, err := s.MarkInvoicePaid("inv-1"); err != nil {
		t.Fatalf("first MarkInvoicePaid: %v", err)
	}

	var firstUntil string
	if err := s.db.QueryRow("SELECT COALESCE(pro_until, '') FROM users WHERE id = ?", uid).Scan(&firstUntil); err != nil {
		t.Fatalf("read firstUntil: %v", err)
	}
	if firstUntil == "" {
		t.Fatalf("expected pro_until set after first payment; got empty")
	}

	// Wait so a buggy re-stamp would land in a different second, then
	// re-fire the webhook. A correct implementation is a no-op.
	waitOneSecForTimestampMove(t)

	if _, err := s.MarkInvoicePaid("inv-1"); err != nil {
		t.Fatalf("second MarkInvoicePaid: %v", err)
	}

	var secondUntil string
	if err := s.db.QueryRow("SELECT COALESCE(pro_until, '') FROM users WHERE id = ?", uid).Scan(&secondUntil); err != nil {
		t.Fatalf("read secondUntil: %v", err)
	}

	if secondUntil != firstUntil {
		t.Fatalf("pro_until reset on second webhook: first=%q second=%q — second call should be a no-op", firstUntil, secondUntil)
	}

	// sanity: paid_at should also stay stable (we don't double-stamp it)
	var paidAtCount int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM invoices WHERE invoice_id = ? AND paid_at IS NOT NULL", "inv-1").Scan(&paidAtCount); err != nil {
		t.Fatalf("paid_at count: %v", err)
	}
	if paidAtCount != 1 {
		t.Fatalf("expected exactly 1 row with paid_at; got %d", paidAtCount)
	}
}

// TestMarkInvoicePaid_StampsPaidAtOnlyOnce covers the related half of the
// same idempotency surface: the audit row in invoices should be stamped at
// the first successful webhook, not overwritten on retries.
func TestMarkInvoicePaid_StampsPaidAtOnlyOnce(t *testing.T) {
	s := newBillingStore(t)
	uid, _, _ := s.Register("billing-stamp-user", "secret123")
	if err := s.SaveInvoice("inv-2", uid, "pro", "5.00", ""); err != nil {
		t.Fatal(err)
	}

	if _, err := s.MarkInvoicePaid("inv-2"); err != nil {
		t.Fatal(err)
	}
	var firstPaidAt string
	s.db.QueryRow("SELECT paid_at FROM invoices WHERE invoice_id = ?", "inv-2").Scan(&firstPaidAt)
	if !strings.Contains(firstPaidAt, "-") {
		t.Fatalf("expected paid_at populated; got %q", firstPaidAt)
	}

	// Wait one full second so a naive UPDATE ... CURRENT_TIMESTAMP would
	// yield a different string. Then call MarkInvoicePaid again — paid_at
	// should not move.
	waitOneSecForTimestampMove(t)

	if _, err := s.MarkInvoicePaid("inv-2"); err != nil {
		t.Fatal(err)
	}
	var secondPaidAt string
	s.db.QueryRow("SELECT paid_at FROM invoices WHERE invoice_id = ?", "inv-2").Scan(&secondPaidAt)
	if firstPaidAt != secondPaidAt {
		t.Fatalf("paid_at changed on retry: %q -> %q (should be idempotent)", firstPaidAt, secondPaidAt)
	}
}

// TestMarkInvoicePaid_SecondInvoiceExtendsNotResets covers the stacking
// case: a user pays invoice #1 (pro_until = T+30d), then pays invoice #2
// while still PRO (e.g. renewing early). Their paid window should grow to
// T+60d, not collapse to "now + 30d" — the user paid for 60 days, they
// get 60 days.
//
// Pre-fix MarkInvoicePaid always anchored pro_until to datetime('now',
// '+30 days'), so a renewal at T+10d would move pro_until BACK from
// T+30d to T+10d+30d — losing the 10 days of remaining paid time. The
// correct behavior is max(existing_pro_until, now) + 30d.
//
// We seed pro_until to a far-future date so the test is deterministic
// without a multi-day wait: pre-fix pro_until resets to ~now+30d
// (~2026-10-13); post-fix pro_until extends from 2099 to ~2099+30d.
// Any value in the 2099 year range passes; anything in 2026 fails.
func TestMarkInvoicePaid_SecondInvoiceExtendsNotResets(t *testing.T) {
	s := newBillingStore(t)
	uid, _, err := s.Register("billing-stack-user", "secret123")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.SaveInvoice("inv-A", uid, "pro", "5.00", "https://pay/A"); err != nil {
		t.Fatalf("save inv-A: %v", err)
	}
	if err := s.SaveInvoice("inv-B", uid, "pro", "5.00", "https://pay/B"); err != nil {
		t.Fatalf("save inv-B: %v", err)
	}

	// Seed an existing pro_until far in the future — simulates a user
	// who already has plenty of paid time remaining.
	if _, err := s.db.Exec(`UPDATE users SET plan='pro', pro_until='2099-01-01 00:00:00' WHERE id=?`, uid); err != nil {
		t.Fatalf("seed pro_until: %v", err)
	}

	// New invoice payment: must EXTEND from existing 2099-01-01, not
	// reset to now+30d (~2026-10-13).
	if _, err := s.MarkInvoicePaid("inv-B"); err != nil {
		t.Fatalf("MarkInvoicePaid inv-B: %v", err)
	}
	var newUntil string
	if err := s.db.QueryRow("SELECT COALESCE(pro_until, '') FROM users WHERE id = ?", uid).Scan(&newUntil); err != nil {
		t.Fatalf("read newUntil: %v", err)
	}

	parsed, err := time.Parse("2006-01-02 15:04:05", newUntil)
	if err != nil {
		t.Fatalf("parse newUntil %q: %v", newUntil, err)
	}
	if parsed.Year() < 2099 {
		t.Fatalf("pro_until was reset instead of extended: got %q (year %d) — expected 2099+ range, not %d", newUntil, parsed.Year(), time.Now().Year())
	}

	// inv-B should be marked paid exactly once.
	var paidCount int
	s.db.QueryRow("SELECT COUNT(*) FROM invoices WHERE invoice_id = ? AND paid_at IS NOT NULL", "inv-B").Scan(&paidCount)
	if paidCount != 1 {
		t.Fatalf("expected inv-B paid exactly once; got %d", paidCount)
	}
}
