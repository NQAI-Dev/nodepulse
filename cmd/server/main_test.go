package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
	"github.com/NQAI-Dev/nodepulse/pkg/store"
)

// alwaysValid / alwaysInvalid are the only two validator implementations
// the install-script tests need. Production wiring goes through
// pStore.ValidateToken (which queries api_tokens); here we exercise the
// handler's own logic in isolation.
func alwaysValid(string) bool    { return true }
func alwaysInvalid(string) bool  { return false }

func TestInstallScriptMissingToken(t *testing.T) {
	req := httptest.NewRequest("GET", "/install.sh", nil)
	w := httptest.NewRecorder()
	installScript(w, req, alwaysValid)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "missing") || !strings.Contains(body, "?token=") {
		t.Fatalf("expected user-facing 'missing ?token=' guidance, got: %q", body)
	}
	// Critical safety check: the response must never contain the literal
	// master admin token, regardless of validator behaviour.
	if strings.Contains(body, "np_live_master_secret") {
		t.Fatalf("response leaked master admin token: %q", body)
	}
	if strings.Contains(body, "NODEPULSE_TOKEN=") {
		t.Fatalf("response leaked NODEPULSE_TOKEN directive: %q", body)
	}
}

func TestInstallScriptInvalidToken(t *testing.T) {
	req := httptest.NewRequest("GET", "/install.sh?token=garbage", nil)
	w := httptest.NewRecorder()
	installScript(w, req, alwaysInvalid)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "invalid") {
		t.Fatalf("expected user-facing 'invalid' guidance, got: %q", body)
	}
	if strings.Contains(body, "NODEPULSE_TOKEN=") {
		t.Fatalf("response leaked NODEPULSE_TOKEN directive on invalid token: %q", body)
	}
}

func TestInstallScriptValidToken(t *testing.T) {
	req := httptest.NewRequest("GET", "/install.sh?token=np_user_test_abc", nil)
	w := httptest.NewRecorder()
	installScript(w, req, alwaysValid)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "text/x-shellscript" {
		t.Fatalf("content-type: got %q, want text/x-shellscript", ct)
	}
	body, _ := io.ReadAll(w.Body)
	s := string(body)
	if !strings.Contains(s, "#!/bin/sh") {
		t.Fatalf("missing shebang")
	}
	if !strings.Contains(s, `TOKEN="np_user_test_abc"`) {
		t.Fatalf("TOKEN directive missing or wrong value: %s", s)
	}
	if !strings.Contains(s, "NODEPULSE_TOKEN=np_user_test_abc") {
		t.Fatalf("NODEPULSE_TOKEN directive missing or wrong value")
	}
	// Defence in depth: even on a valid request the master token literal
	// must never appear in the body unless the caller actually supplied it.
	if strings.Contains(s, "np_live_master_secret") {
		t.Fatalf("response contains master admin token literal: %s", s)
	}
}

func TestInstallScriptNeverLeaksMasterTokenOnBarePath(t *testing.T) {
	// Regression test for the original bug: the old handler fell back to
	// the master admin token when ?token= was absent. Even with a validator
	// that would happily approve the master token (the realistic case),
	// the bare-path request must not be served.
	req := httptest.NewRequest("GET", "/install.sh", nil)
	w := httptest.NewRecorder()
	installScript(w, req, alwaysValid)

	if w.Code == http.StatusOK {
		t.Fatalf("bare /install.sh must not return 200, got body: %q", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "np_live_master_secret") {
		t.Fatalf("bare /install.sh leaked master admin token")
	}
}

// TestHeartbeatIngest_BindNodeFailure pins the b6842be fix at the HTTP
// boundary. The handler was rewritten to call pStore.BindNode (which
// returns error after the silent-p.db.Exec conversion) and surface a
// 500 when binding fails. Without this test, a future refactor that
// flips BindNode back to a discarded-error swallow would not be caught
// until the next prod schema drift (the 2026-09-13 ~19:30 UTC
// incident class). Drops the node_owners table to force BindNode to
// fail, sends a valid heartbeat, and asserts the 500 response.
//
// Regression guard for 2026-09-13 ~19:30 UTC prod incident class — if
// this test ever fails, BindNode has been silently broken again.
func TestHeartbeatIngest_BindNodeFailure(t *testing.T) {
	dbPath := "/tmp/test_bind_node_failure.db"
	os.Remove(dbPath)
	defer os.Remove(dbPath)

	pStore, err := store.NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}

	// Register a real user so we can authenticate the heartbeat with a
	// non-master token. Master token would bypass the BindNode branch
	// entirely (uid=0 path skips it), so the test must use a regular
	// user token to exercise the failure path.
	uid, token, err := pStore.Register("alice", "secret123")
	if err != nil {
		t.Fatalf("Register alice: %v", err)
	}
	if uid == 0 || token == "" {
		t.Fatalf("Register returned zero values (uid=%d token=%q)", uid, token)
	}

	// Now force BindNode to fail by dropping its table. Production
	// equivalent: schema drift, partial migration, operator action.
	// This mirrors the prod failure mode the silent-p.db.Exec class
	// used to mask — except now the error has to surface as a
	// non-2xx HTTP response.
	if _, err := pStore.DB().Exec("DROP TABLE node_owners"); err != nil {
		t.Fatalf("drop node_owners: %v", err)
	}

	hb := protocol.Heartbeat{
		NodeID: "test-node-01",
	}
	body, err := json.Marshal(hb)
	if err != nil {
		t.Fatalf("marshal heartbeat: %v", err)
	}

	req := httptest.NewRequest("POST", "/api/v1/ingest", strings.NewReader(string(body)))
	req.Header.Set("X-NodePulse-Token", token)
	w := httptest.NewRecorder()

	handleHeartbeatIngest(w, req, pStore)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%q", w.Code, w.Body.String())
	}
	respBody := w.Body.String()
	if !strings.Contains(respBody, "internal error") {
		t.Fatalf("response must contain 'internal error' guidance, got %q", respBody)
	}
	// Defence in depth: the handler must NEVER have called Ingest (which
	// would write the heartbeat row and pretend the binding succeeded)
	// or RecordProbeResults. We don't have a clean way to assert this
	// from outside the package without exposing internals, but the
	// status code itself proves BindNode returned early with an error
	// — pre-fix the handler would have returned 200 with ok:true and the
	// node_owners row would simply not exist.
}
