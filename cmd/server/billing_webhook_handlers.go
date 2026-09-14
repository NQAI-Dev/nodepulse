package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
	"github.com/NQAI-Dev/nodepulse/pkg/store"
)

// handleBillingWebhook serves POST /api/v1/billing/webhook. It is the
// payment-provider callback that tells us the user has actually paid.
//
// Flow: decode the CryptoBot webhook envelope → if Payload.Status is
// "paid", call MarkInvoicePaid (idempotent: stamps paid_at + extends
// pro_until by 30 days on first call, no-op on retry) → return
// {"ok":true} so the provider stops retrying.
//
// The critical bug this file-scope extraction closes is the
// silent-payment-loss class on the inverse side of the create-invoice
// boundary. Pre-fix the handler logged "Error marking invoice 999001
// paid: ..." but then wrote {"ok":true} with HTTP 200 regardless.
// CryptoBot only retries on non-2xx, so a real DB error (missing row,
// table dropped, transient lock) would fire once in journalctl and then
// vanish: the user paid real money, our system replied "ok", no retries
// happened, the user's plan stays Free forever, the operator has no
// alarm beyond the single log line. With this fix, any MarkInvoicePaid
// failure surfaces as HTTP 500 so the provider will retry — a transient
// sqlite busy resolves on the next attempt, a real schema-drift error
// fires repeatedly in journalctl and an operator catches it via the
// existing alert pipeline.
//
// File-scope (not a closure inside main) so the handler can be
// unit-tested with a real *PersistentStore + an injected webhook
// payload — no live CryptoBot, no signature verification, no network.
// See TestBillingWebhook_* in billing_webhook_handlers_test.go for the
// regression that pins the error path.
func handleBillingWebhook(w http.ResponseWriter, r *http.Request, pStore *store.PersistentStore) {
	var hook protocol.CryptoBotWebhook
	if err := json.NewDecoder(r.Body).Decode(&hook); err != nil {
		http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
		return
	}

	if hook.Payload.Status == "paid" {
		invID := fmt.Sprintf("%d", hook.Payload.InvoiceID)
		uid, err := pStore.MarkInvoicePaid(invID)
		if err != nil {
			// MarkInvoicePaid returned error: surface as 500 so the
			// payment provider retries. We log with both the invoice
			// id (for grep) and the wrapped error (operator action).
			log.Printf("[billing] mark paid invoice %s failed: %v", invID, err)
			http.Error(w, `{"error":"internal error — retrying"}`, http.StatusInternalServerError)
			return
		}
		log.Printf("[billing] invoice %s paid, uid=%d upgraded to PRO", invID, uid)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}
