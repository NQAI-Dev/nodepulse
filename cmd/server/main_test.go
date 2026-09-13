package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

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

// TestAutohealLog_RecordAutoHealLogsFailure pins the b6842be fix at the
// HTTP boundary for the autoheal log endpoint. Drops autoheal_logs to
// simulate schema drift (the exact failure class that the silent-p.db.Exec
// anti-pattern used to mask), then sends a valid payload with a non-master
// token and asserts the handler surfaces 500 + 'internal error' instead of
// silently returning {accepted:true}.
//
// Regression guard for 2026-09-13 ~19:30 UTC prod incident class — if
// this test ever fails, RecordAutoHealLogs has been silently broken again
// from the HTTP entry point's perspective.
func TestAutohealLog_RecordAutoHealLogsFailure(t *testing.T) {
	dbPath := "/tmp/test_autoheal_log_failure.db"
	os.Remove(dbPath)
	defer os.Remove(dbPath)

	pStore, err := store.NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}

	// Register a real user so the autoheal log batch is authenticated
	// against a non-master token. A master token would still exercise the
	// RecordAutoHealLogs branch, but using a regular user mirrors the
	// normal agent ingest flow more closely.
	uid, token, err := pStore.Register("bob", "secret456")
	if err != nil {
		t.Fatalf("Register bob: %v", err)
	}
	if uid == 0 || token == "" {
		t.Fatalf("Register returned zero values (uid=%d token=%q)", uid, token)
	}

	// Now force RecordAutoHealLogs to fail by dropping its table.
	// Production equivalent: schema drift, partial migration, operator
	// action. tx.Prepare on a missing table returns SQLITE_ERROR →
	// RecordAutoHealLogs surfaces it as an error → handler must 500.
	if _, err := pStore.DB().Exec("DROP TABLE autoheal_logs"); err != nil {
		t.Fatalf("drop autoheal_logs: %v", err)
	}

	payload := map[string]interface{}{
		"node_id": "test-node-02",
		"events": []protocol.AutoHealLog{
			{
				Command: "systemctl restart myapp",
				Status:  "ok",
				Reason:  "high_cpu",
				Ts:      1700000000,
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	req := httptest.NewRequest("POST", "/api/v1/autoheal/log", strings.NewReader(string(body)))
	req.Header.Set("X-NodePulse-Token", token)
	w := httptest.NewRecorder()

	handleAutohealLog(w, req, pStore)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%q", w.Code, w.Body.String())
	}
	respBody := w.Body.String()
	if !strings.Contains(respBody, "internal error") {
		t.Fatalf("response must contain 'internal error' guidance, got %q", respBody)
	}
	// Defence in depth: the handler must NEVER have returned {accepted:true}.
	// Pre-fix (b6842be) the discarded-error swallow would have replied
	// 200 + {"accepted":true}, leaving the agent believing the events
	// were persisted when no INSERT ever happened.
	if strings.Contains(respBody, `"accepted":true`) {
		t.Fatalf("handler silently accepted autoheal logs after backend failure: %q", respBody)
	}
}

// signedTelegramHash returns the HMAC-SHA256 hash a real Telegram Login
// Widget would produce for the given query string, using botToken as the
// shared secret. Mirrors the algorithm in handleTgCallback so the test
// can construct a query that passes the handler's HMAC validation and
// reaches the RegisterByTelegram call — which is where the prod failure
// mode lives.
func signedTelegramHash(query url.Values, botToken string) string {
	keys := make([]string, 0, len(query))
	for k := range query {
		if k == "hash" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+query.Get(k))
	}
	checkString := strings.Join(parts, "\n")
	secretKey := sha256.Sum256([]byte(botToken))
	mac := hmac.New(sha256.New, secretKey[:])
	mac.Write([]byte(checkString))
	return hex.EncodeToString(mac.Sum(nil))
}

// TestTgCallback_RegisterByTelegramFailure pins the 7173c0d fix at the
// HTTP boundary. The handler was rewritten to surface errors from
// pStore.RegisterByTelegram as 500 instead of silently returning a 302
// with a fragment containing a token that was never persisted. Drops
// api_tokens to force the auth-path INSERT inside RegisterByTelegram to
// fail, signs a valid Telegram-style query, GETs the endpoint, and
// asserts the 500 response — NOT a 302 redirect.
//
// Regression guard for 2026-09-13 ~19:30 UTC prod incident class — every
// Telegram login (the SaaS onboarding flow) goes through this endpoint,
// and a silent-INSERT regression would surface to the user as "logged
// in, dashboard greets me, but every subsequent API call returns 401"
// which is the EXACT pre-aacd56e failure mode.
func TestTgCallback_RegisterByTelegramFailure(t *testing.T) {
	dbPath := "/tmp/test_tg_callback_failure.db"
	os.Remove(dbPath)
	defer os.Remove(dbPath)

	pStore, err := store.NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}

	const testBotToken = "123456:ABC-DEF-test-bot-token"

	// Construct a valid Telegram-style query: id, first_name, username,
	// auth_date (within 5-minute freshness window), and the computed hash.
	authDate := strconv.FormatInt(time.Now().Unix(), 10)
	q := url.Values{}
	q.Set("id", "888111222")
	q.Set("first_name", "TelegramUser")
	q.Set("username", "tguser")
	q.Set("auth_date", authDate)
	q.Set("hash", signedTelegramHash(q, testBotToken))

	// Force RegisterByTelegram to fail by dropping api_tokens. This
	// mirrors the EXACT prod failure mode the silent-p.db.Exec class
	// used to mask — the auth-path INSERT for the freshly-issued
	// tg-login token fails with "no such table: api_tokens". After
	// 7173c0d this error must surface; pre-fix it was discarded and the
	// handler would have 302'd with a fragment pointing at a token
	// that was never persisted.
	if _, err := pStore.DB().Exec("DROP TABLE api_tokens"); err != nil {
		t.Fatalf("drop api_tokens: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/v1/tg-callback?"+q.Encode(), nil)
	w := httptest.NewRecorder()

	handleTgCallback(w, req, pStore, testBotToken)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%q", w.Code, w.Body.String())
	}
	respBody := w.Body.String()
	if !strings.Contains(respBody, "internal error") {
		t.Fatalf("response must contain 'internal error' guidance, got %q", respBody)
	}
	// Defence in depth: the handler must NEVER have issued a 302 with
	// a fragment containing a token. Pre-fix (7173c0d) the
	// discarded-error swallow would have replied 302 with
	// /index.html#token=...&user_id=...&tg_id=..., leaving the
	// dashboard with a token that's not in api_tokens and every
	// subsequent API call returning 401.
	if w.Code == http.StatusFound && strings.Contains(w.Header().Get("Location"), "#token=") {
		t.Fatalf("handler silently issued redirect with fragment after backend failure: %q", w.Header().Get("Location"))
	}
}

// TestAutohealLogsRead_AuthFailure pins the auth boundary on the
// read-side counterpart of POST /api/v1/autoheal/log. Without a valid
// non-master token the handler must return 401 with a JSON error body,
// NOT 200 with another user's autoheal events. Defence against anyone
// reverting the auth check or accidentally making the read endpoint
// unauthenticated.
func TestAutohealLogsRead_AuthFailure(t *testing.T) {
	dbPath := "/tmp/test_autoheal_logs_read_auth.db"
	os.Remove(dbPath)
	defer os.Remove(dbPath)

	pStore, err := store.NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}

	// No Authorization header, no ?token= query param: must 401.
	req := httptest.NewRequest("GET", "/api/v1/autoheal/logs?node_id=any-node", nil)
	w := httptest.NewRecorder()
	handleAutohealLogsRead(w, req, pStore)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing-token: got %d, want 401; body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"error":"unauthorized"`) {
		t.Fatalf("missing-token: body must mention unauthorized, got %q", w.Body.String())
	}

	// Invalid token in Authorization header: must 401.
	req2 := httptest.NewRequest("GET", "/api/v1/autoheal/logs?node_id=any-node", nil)
	req2.Header.Set("Authorization", "Bearer np_invalid_xyz_zzz")
	w2 := httptest.NewRecorder()
	handleAutohealLogsRead(w2, req2, pStore)

	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("invalid-bearer: got %d, want 401; body=%q", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), `"error":"unauthorized"`) {
		t.Fatalf("invalid-bearer: body must mention unauthorized, got %q", w2.Body.String())
	}
}

// TestAutohealLogsRead_MissingNodeID pins the required-parameter check.
// Without node_id the handler must return 400, NOT silently default to
// "all nodes" or "user's first node" — both would be information
// disclosure paths.
func TestAutohealLogsRead_MissingNodeID(t *testing.T) {
	dbPath := "/tmp/test_autoheal_logs_read_nonode.db"
	os.Remove(dbPath)
	defer os.Remove(dbPath)

	pStore, err := store.NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}

	uid, token, err := pStore.Register("alice", "secret456")
	if err != nil {
		t.Fatalf("Register alice: %v", err)
	}
	if uid == 0 || token == "" {
		t.Fatalf("Register returned zero values (uid=%d token=%q)", uid, token)
	}

	req := httptest.NewRequest("GET", "/api/v1/autoheal/logs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handleAutohealLogsRead(w, req, pStore)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("no-node-id: got %d, want 400; body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "node_id required") {
		t.Fatalf("no-node-id: body must mention node_id required, got %q", w.Body.String())
	}
}

// TestAutohealLogsRead_NodeNotInFleet pins the multi-tenant boundary
// enforced via GetUserNodes. A valid token from user A asking for
// user B's node must return 403, NOT 200 with B's autoheal events.
// This is the read-side counterpart to the per-user BindNode filter on
// the POST side; without it, any registered user could enumerate
// every node's remediation history.
func TestAutohealLogsRead_NodeNotInFleet(t *testing.T) {
	dbPath := "/tmp/test_autoheal_logs_read_cross.db"
	os.Remove(dbPath)
	defer os.Remove(dbPath)

	pStore, err := store.NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}

	// Two distinct users. Alice owns her own node; Bob asks for Alice's
	// node — must be rejected.
	uidA, _, err := pStore.Register("alice", "secret456")
	if err != nil {
		t.Fatalf("Register alice: %v", err)
	}
	_, tokenB, err := pStore.Register("bob", "secret789")
	if err != nil {
		t.Fatalf("Register bob: %v", err)
	}

	// Bind Alice's node to her uid so it shows up in GetUserNodes(uidA).
	seedFleetNode(t, pStore, uidA, "alice-node-01")

	req := httptest.NewRequest("GET", "/api/v1/autoheal/logs?node_id=alice-node-01", nil)
	req.Header.Set("Authorization", "Bearer "+tokenB)
	w := httptest.NewRecorder()
	handleAutohealLogsRead(w, req, pStore)

	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant: got %d, want 403; body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "node not in your fleet") {
		t.Fatalf("cross-tenant: body must mention 'node not in your fleet', got %q", w.Body.String())
	}
}

// TestAutohealLogsRead_EmptyResult pins the empty-state response shape.
// A valid token, valid node_id, but no events recorded → 200 with
// `{"node_id":"X","events":[]}` (NOT null, NOT 404, NOT a different
// schema like `{"logs":[]}`).
func TestAutohealLogsRead_EmptyResult(t *testing.T) {
	dbPath := "/tmp/test_autoheal_logs_read_empty.db"
	os.Remove(dbPath)
	defer os.Remove(dbPath)

	pStore, err := store.NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}

	uid, token, err := pStore.Register("carol", "secret456")
	if err != nil {
		t.Fatalf("Register carol: %v", err)
	}
	seedFleetNode(t, pStore, uid, "carol-node-01")

	req := httptest.NewRequest("GET", "/api/v1/autoheal/logs?node_id=carol-node-01", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handleAutohealLogsRead(w, req, pStore)

	if w.Code != http.StatusOK {
		t.Fatalf("empty: got %d, want 200; body=%q", w.Code, w.Body.String())
	}
	var got struct {
		NodeID string                `json:"node_id"`
		Events []protocol.AutoHealLog `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal body: %v; raw=%q", err, w.Body.String())
	}
	if got.NodeID != "carol-node-01" {
		t.Fatalf("node_id: got %q, want carol-node-01", got.NodeID)
	}
	if len(got.Events) != 0 {
		t.Fatalf("events: expected empty slice, got %d entries: %+v", len(got.Events), got.Events)
	}
}

// TestAutohealLogsRead_WithEvents pins the happy-path round trip:
// RecordAutoHealLogs persists 3 events → GET returns 3 events with the
// same fields and in the same order (RecentAutoHealLogs ordering must
// be stable across releases).
func TestAutohealLogsRead_WithEvents(t *testing.T) {
	dbPath := "/tmp/test_autoheal_logs_read_with.db"
	os.Remove(dbPath)
	defer os.Remove(dbPath)

	pStore, err := store.NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}

	uid, token, err := pStore.Register("dave", "secret456")
	if err != nil {
		t.Fatalf("Register dave: %v", err)
	}
	seedFleetNode(t, pStore, uid, "dave-node-01")

	// Seed 3 distinct events via the production RecordAutoHealLogs
	// path — that's the only entry point that writes to autoheal_logs.
	// Timestamps MUST be within the last 24 hours — RecordAutoHealLogs
	// runs a cleanup DELETE for older rows at the end of every batch,
	// and the test would silently lose events with ancient ts values.
	now := time.Now().Unix()
	seed := []protocol.AutoHealLog{
		{Command: "systemctl restart nginx", Status: "ok", Reason: "high_cpu", Ts: now - 300},
		{Command: "systemctl restart app", Status: "ok", Reason: "oom_kill", Ts: now - 200},
		{Command: "systemctl restart db", Status: "failed", Reason: "disk_full", Ts: now - 100},
	}
	if err := pStore.RecordAutoHealLogs("dave-node-01", uid, seed); err != nil {
		t.Fatalf("RecordAutoHealLogs: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/v1/autoheal/logs?node_id=dave-node-01", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handleAutohealLogsRead(w, req, pStore)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%q", w.Code, w.Body.String())
	}
	var got struct {
		NodeID string                `json:"node_id"`
		Events []protocol.AutoHealLog `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal body: %v; raw=%q", err, w.Body.String())
	}
	if got.NodeID != "dave-node-01" {
		t.Fatalf("node_id: got %q, want dave-node-01", got.NodeID)
	}
	if len(got.Events) != 3 {
		t.Fatalf("events: got %d, want 3; raw=%v", len(got.Events), got.Events)
	}
	// RecentAutoHealLogs returns newest-first (ORDER BY id DESC), so
	// the events come back in reverse insertion order. Pin that
	// ordering as part of the contract — a future commit that
	// accidentally reverses it would break operator dashboards that
	// assume the most recent remediation is at index 0.
	for i, want := range []protocol.AutoHealLog{seed[2], seed[1], seed[0]} {
		if got.Events[i].Command != want.Command ||
			got.Events[i].Status != want.Status ||
			got.Events[i].Reason != want.Reason ||
			got.Events[i].Ts != want.Ts {
			t.Fatalf("event[%d]: got %+v, want %+v", i, got.Events[i], want)
		}
	}
}

// TestAutohealLogsRead_LimitClamping pins the ?limit= parsing rules:
// invalid (non-numeric), zero, negative, and out-of-range (>200) values
// must all silently fall back to the default of 50. A regression that
// passes garbage through to RecentAutoHealLogs could cause the SQLite
// query to error or — worse — silently return the entire table.
func TestAutohealLogsRead_LimitClamping(t *testing.T) {
	dbPath := "/tmp/test_autoheal_logs_read_limit.db"
	os.Remove(dbPath)
	defer os.Remove(dbPath)

	pStore, err := store.NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}

	uid, token, err := pStore.Register("erin", "secret456")
	if err != nil {
		t.Fatalf("Register erin: %v", err)
	}
	seedFleetNode(t, pStore, uid, "erin-node-01")

	// Pin the clamp by exercising all four bad-value shapes in one
	// request stream — each must still return 200 with the default
	// applied. The handler doesn't surface the chosen limit back to
	// the caller, so the assertion is "no error, 200 OK" + the
	// behaviour was exercised by the parse path.
	cases := []struct {
		name  string
		query string
	}{
		{"invalid-non-numeric", "limit=abc"},
		{"zero", "limit=0"},
		{"negative", "limit=-5"},
		{"above-max", "limit=500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/v1/autoheal/logs?node_id=erin-node-01&"+tc.query, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			handleAutohealLogsRead(w, req, pStore)

			if w.Code != http.StatusOK {
				t.Fatalf("got %d, want 200; body=%q", w.Code, w.Body.String())
			}
			var got struct {
				NodeID string                `json:"node_id"`
				Events []protocol.AutoHealLog `json:"events"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal body: %v; raw=%q", err, w.Body.String())
			}
			if got.NodeID != "erin-node-01" {
				t.Fatalf("node_id: got %q, want erin-node-01", got.NodeID)
			}
		})
	}
}

// seedFleetNode is a test helper that puts nodeID into both halves of the
// "user's fleet" predicate the read-side handlers depend on:
//
//   1. node_owners row (BindNode) — what GetUserNodes filters by.
//   2. p.mem in-memory NodeState (pStore.Ingest) — what GetUserNodes
//      returns when the filter passes. RecentAutoHealLogs joins against
//      autoheal_logs and never reads from the in-memory store, so this
//      second step is what makes GetUserNodes return a non-empty map for
//      nodeID. BindNode alone leaves the in-memory map empty, so
//      GetUserNodes returns map[nodeID] -> missing even though
//      node_owners has the row — the handler then 403s with "node not
//      in your fleet", which is correct behaviour for production (real
//      agents only ever populate the in-memory map by sending a
//      heartbeat) but a surprising trap for tests.
//
// The heartbeat is minimal: just NodeID + a populated CPU/Memory/Disks
// so p.mem.Ingest's status heuristic doesn't blow up. No Tags /
// Services / Probes needed.
func seedFleetNode(t *testing.T, pStore *store.PersistentStore, uid int64, nodeID string) {
	t.Helper()
	if err := pStore.BindNode(nodeID, uid); err != nil {
		t.Fatalf("BindNode %q: %v", nodeID, err)
	}
	pStore.Ingest(&protocol.Heartbeat{
		NodeID:    nodeID,
		Timestamp: time.Now().Unix(),
		Node: protocol.NodeInfo{
			ID:       nodeID,
			Hostname: nodeID,
			OS:       "linux",
			Arch:     "amd64",
			Version:  "test",
		},
		CPU: protocol.CPUStats{
			UsagePercent: 10,
			Load1:        0.1,
			Load5:        0.1,
			Load15:       0.1,
			Cores:        2,
		},
		Memory: protocol.MemoryStats{
			TotalBytes:     1 << 30,
			AvailableBytes: 1 << 29,
			UsedBytes:      1 << 29,
			UsedPercent:    50,
		},
		Disks: []protocol.DiskStats{{
			MountPoint: "/",
			TotalBytes: 1 << 40,
			FreeBytes:  1 << 39,
			UsedPercent: 25,
		}},
	})
}
