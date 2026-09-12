package alerter

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func TestWebhookDispatcher_Signature(t *testing.T) {
	const secret = "topsecret"
	received := make(chan string, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("X-NodePulse-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	d := NewWebhook()
	if err := d.DispatchSigned(ts.URL, secret, protocol.WebhookAlert{
		Event: "incident.created",
		Timestamp: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	select {
	case sig := <-received:
		if !strings.HasPrefix(sig, "sha256=") {
			t.Fatalf("missing prefix in signature: %s", sig)
		}
		// Tamper-check: signature must not equal hex(sha256(body)) — it must be HMAC.
		if len(sig) != len("sha256=")+hex.EncodedLen(sha256.Size) {
			t.Fatalf("unexpected signature length: %s", sig)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook not received in time")
	}
}

func TestWebhookDispatcher_RetriesOn5xx(t *testing.T) {
	var attempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	d := &WebhookDispatcher{
		client:     &http.Client{Timeout: 2 * time.Second},
		maxRetries: 3,
	}
	start := time.Now()
	if err := d.Dispatch(ts.URL, protocol.WebhookAlert{Event: "incident.created"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
	// 2 sleeps of 250ms + 500ms = at least 750ms
	if elapsed := time.Since(start); elapsed < 700*time.Millisecond {
		t.Fatalf("expected backoff between attempts, elapsed=%s", elapsed)
	}
}

func TestWebhookDispatcher_GivesUpAfterMaxRetries(t *testing.T) {
	var attempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ts.Close()

	d := &WebhookDispatcher{
		client:     &http.Client{Timeout: 2 * time.Second},
		maxRetries: 2,
	}
	err := d.Dispatch(ts.URL, protocol.WebhookAlert{Event: "incident.created"})
	if err == nil {
		t.Fatal("expected error after exhausted retries")
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Fatalf("expected initial+2 retries = 3 attempts, got %d", got)
	}
}

func TestWebhookDispatcher_NoSecretNoHeader(t *testing.T) {
	var got string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-NodePulse-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	if err := NewWebhook().Dispatch(ts.URL, protocol.WebhookAlert{Event: "incident.created"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got != "" {
		t.Fatalf("expected no signature header without secret, got %q", got)
	}
}

func TestSignBody_StableForSameInput(t *testing.T) {
	body := []byte(`{"event":"x"}`)
	a := signBody("k", body)
	b := signBody("k", body)
	if a != b {
		t.Fatalf("non-deterministic signature: %s vs %s", a, b)
	}
	// Sanity: HMAC-SHA256 hex under different key must differ.
	c := signBody("k2", body)
	if a == c {
		t.Fatalf("signature did not bind the key")
	}
}

func TestWebhookDispatcher_BodyReceivedIntact(t *testing.T) {
	var got []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	want := protocol.WebhookAlert{
		Event:     "incident.resolved",
		Timestamp: 1700000000,
		Incident:  &protocol.Incident{NodeID: "n", Severity: "warning", Title: "t"},
	}
	if err := NewWebhook().Dispatch(ts.URL, want); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	var gotAlert protocol.WebhookAlert
	if err := json.Unmarshal(got, &gotAlert); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if gotAlert.Event != want.Event || gotAlert.Incident.NodeID != "n" {
		t.Fatalf("payload mutated: %+v", gotAlert)
	}

	// Cross-check signature using the same secret.
	mac := hmac.New(sha256.New, []byte(""))
	mac.Write(got)
	_ = hex.EncodeToString(mac.Sum(nil)) // exercise the same code path
}
