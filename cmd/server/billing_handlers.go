package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/NQAI-Dev/nodepulse/pkg/billing"
	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
	"github.com/NQAI-Dev/nodepulse/pkg/store"
)

// createInvoiceFn is the function-type injected into the billing
// create-invoice handler so tests can swap a fake CryptoBot client
// without standing up the real one. Mirrors installScript's
// validateToken injection pattern: production wiring goes through
// billing.NewCryptoBot(...).CreateInvoice, tests supply a stub.
type createInvoiceFn func(amount, asset, desc, payload string) (*billing.CreateInvoiceResponse, error)

// handleBillingCreateInvoice serves POST /api/v1/billing/create-invoice.
//
// Flow: read the user token (header Bearer or ?token= fallback) →
// resolve the owning user via GetUserByToken → call createInvoice
// (CryptoBot in production, stub in tests) → persist the issued
// invoice via SaveInvoice → return the pay URL to the caller.
//
// SaveInvoice returns error after the silent-p.db.Exec conversion in
// 8af1a60; this handler surfaces that error as a 500 instead of the
// pre-fix silent-200-with-phantom-invoice swallow. The bug class:
// CryptoBot would issue a real invoice_id + pay URL → handler discards
// SaveInvoice error → returns 200 with the pay URL → user pays →
// webhook arrives → MarkInvoicePaid fails on lookup (no row) → user
// never upgrades despite having paid. 500 here gives the user a chance
// to retry before they hit "Pay".
//
// File-scope (not a closure inside main) so the handler can be
// exercised in isolation by unit tests against a real *PersistentStore
// plus an injected createInvoice stub — see
// TestBillingCreateInvoice_SaveInvoiceFailure in billing_handlers_test.go
// for the regression that pins the SaveInvoice error path.
func handleBillingCreateInvoice(w http.ResponseWriter, r *http.Request, pStore *store.PersistentStore, createInvoice createInvoiceFn) {
	// Auth: Authorization: Bearer <token> header is the standard path
	// for the dashboard, ?token=<token> is the fallback for curl /
	// programmatic clients (matches the authMw convention used by
	// /api/v1/auth/* on the same mux).
	token := r.Header.Get("Authorization")
	token = strings.TrimPrefix(token, "Bearer ")
	if token == "" {
		token = r.URL.Query().Get("token")
	}

	uid, _, err := pStore.GetUserByToken(token)
	if err != nil {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	inv, err := createInvoice("5.00", "USDT", "NodePulse Pro Plan (1 Month)", fmt.Sprintf("%d", uid))
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	invID := fmt.Sprintf("%d", inv.Result.InvoiceID)
	if err := pStore.SaveInvoice(invID, uid, "pro", inv.Result.Amount, inv.Result.PayURL); err != nil {
		// SaveInvoice now returns error (8af1a60). If the DB write fails after
		// CryptoBot already issued an invoice, the user would pay, webhook would
		// fire, MarkInvoicePaid would fail on lookup, and the user would silently
		// never upgrade. 500 here surfaces the failure immediately so the user
		// can retry instead of paying against a phantom invoice.
		log.Printf("[billing] save invoice %s for uid=%d failed: %v", invID, uid, err)
		http.Error(w, `{"error":"failed to persist invoice — please retry"}`, http.StatusInternalServerError)
		return
	}

	log.Printf("Created invoice %s for uid=%d", invID, uid)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(protocol.InvoiceResponse{
		InvoiceID: invID,
		PayURL:    inv.Result.PayURL,
		Amount:    inv.Result.Amount,
		Currency:  inv.Result.Asset,
	})
}
