package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
