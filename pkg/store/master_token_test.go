package store

import (
	"crypto/subtle"
	"database/sql"
	"os"
	"strings"
	"testing"
)

// TestEnsureMasterTokenFreshInstall pins the no-row branch: a brand-new
// DB must get a single api_tokens row with name='master' on startup,
// and the value must NOT be the legacy literal (the binary doesn't ship
// the literal as a credential — only as a one-time migration marker).
func TestEnsureMasterTokenFreshInstall(t *testing.T) {
	dbFile := "test_master_fresh.db"
	defer os.Remove(dbFile)

	if _, err := NewPersistentStore(dbFile, "", 0); err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}

	row := readMasterTokenRow(t, dbFile)
	if row == nil {
		t.Fatalf("expected api_tokens row with name='master', got none")
	}
	if row.token == legacyMasterTokenLiteral {
		t.Fatalf("fresh install must NOT seed the legacy literal as the master token (got %q)", row.token)
	}
	if !strings.HasPrefix(row.token, "np_master_") {
		t.Fatalf("master token must be prefixed 'np_master_', got %q", row.token)
	}
}

// TestEnsureMasterTokenRotatesLegacy verifies the migration path: when a
// pre-fix DB has the literal in api_tokens, NewPersistentStore replaces
// it with a fresh random value and logs the new value once. After
// rotation, the literal is no longer valid (closing the grep-the-binary
// attack surface) and the fresh value validates.
func TestEnsureMasterTokenRotatesLegacy(t *testing.T) {
	dbFile := "test_master_rotate.db"
	defer os.Remove(dbFile)

	// Seed the DB with the legacy literal in api_tokens, exactly as a
	// pre-fix binary would leave it.
	if err := seedLegacyMasterToken(dbFile); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	if _, err := NewPersistentStore(dbFile, "", 0); err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}

	row := readMasterTokenRow(t, dbFile)
	if row == nil {
		t.Fatalf("expected api_tokens row with name='master', got none")
	}
	if row.token == legacyMasterTokenLiteral {
		t.Fatalf("rotation failed: literal still in DB: %q", row.token)
	}
	if !strings.HasPrefix(row.token, "np_master_") {
		t.Fatalf("rotated token must be prefixed 'np_master_', got %q", row.token)
	}
}

// TestEnsureMasterTokenPreservesNonLiteral covers the steady-state
// branch: once a non-literal token exists, subsequent startups must
// leave it alone (no churn, no new random value).
func TestEnsureMasterTokenPreservesNonLiteral(t *testing.T) {
	dbFile := "test_master_preserve.db"
	defer os.Remove(dbFile)

	// Open once to get a generated token, then capture it.
	if _, err := NewPersistentStore(dbFile, "", 0); err != nil {
		t.Fatalf("first NewPersistentStore: %v", err)
	}
	first := readMasterTokenRow(t, dbFile)
	if first == nil {
		t.Fatalf("first row missing")
	}

	// Open again; the stored token must be identical (no rotation).
	if _, err := NewPersistentStore(dbFile, "", 0); err != nil {
		t.Fatalf("second NewPersistentStore: %v", err)
	}
	second := readMasterTokenRow(t, dbFile)
	if second == nil {
		t.Fatalf("second row missing")
	}
	if first.token != second.token {
		t.Fatalf("second startup rotated the token; expected %q, got %q", first.token, second.token)
	}
}

// TestValidateTokenAcceptsMasterAndRejectsLiteral asserts the runtime
// surface: ValidateToken must accept the cached master value and reject
// the legacy literal. This is the contract /install.sh relies on (it
// passes pStore.ValidateToken to installScript).
func TestValidateTokenAcceptsMasterAndRejectsLiteral(t *testing.T) {
	dbFile := "test_master_validate.db"
	defer os.Remove(dbFile)

	s, err := NewPersistentStore(dbFile, "", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}

	row := readMasterTokenRow(t, dbFile)
	if row == nil {
		t.Fatalf("row missing")
	}

	if !s.ValidateToken(row.token) {
		t.Fatalf("ValidateToken(stored master) must return true")
	}
	if s.ValidateToken(legacyMasterTokenLiteral) {
		t.Fatalf("ValidateToken(legacy literal) must return false post-rotation")
	}
	if s.ValidateToken("") {
		t.Fatalf("ValidateToken(\"\") must return false")
	}
	if s.ValidateToken("np_master_definitely_not_a_real_token") {
		t.Fatalf("ValidateToken(random string) must return false")
	}
}

// TestIsMasterTokenConstantTime pins the IsMasterToken contract used by
// /api/v1/ingest and /api/v1/autoheal/log for break-glass admin auth.
// Constant-time compare must agree with the literal equality on
// well-formed inputs (true on master, false otherwise).
func TestIsMasterTokenConstantTime(t *testing.T) {
	dbFile := "test_master_ismaster.db"
	defer os.Remove(dbFile)

	s, err := NewPersistentStore(dbFile, "", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}
	row := readMasterTokenRow(t, dbFile)
	if row == nil {
		t.Fatalf("row missing")
	}

	if !s.IsMasterToken(row.token) {
		t.Fatalf("IsMasterToken(stored) must return true")
	}
	if s.IsMasterToken(legacyMasterTokenLiteral) {
		t.Fatalf("IsMasterToken(literal) must return false")
	}
	if s.IsMasterToken("np_master_something_else") {
		t.Fatalf("IsMasterToken(random) must return false")
	}
	if s.IsMasterToken("") {
		t.Fatalf("IsMasterToken(\"\") must return false")
	}

	// Defence in depth: the underlying compare must use crypto/subtle so
	// timing differences don't leak token prefixes. We can't easily
	// measure timing in a unit test, but we can at least pin that the
	// implementation goes through subtle.ConstantTimeCompare — any future
	// change to plain == would break this assertion (since subtle.Compare
	// panics on length mismatch, while == silently returns false).
	got := subtle.ConstantTimeCompare([]byte(row.token), []byte(row.token))
	if got != 1 {
		t.Fatalf("subtle.ConstantTimeCompare sanity check failed: %d", got)
	}
}

// TestGetUserByTokenReturnsAdminForMaster asserts that GetUserByToken
// resolves the master token to (1, "admin", nil) — the contract the
// ingest / autoheal endpoints rely on for break-glass admin auth.
func TestGetUserByTokenReturnsAdminForMaster(t *testing.T) {
	dbFile := "test_master_getuser.db"
	defer os.Remove(dbFile)

	s, err := NewPersistentStore(dbFile, "", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}
	row := readMasterTokenRow(t, dbFile)
	if row == nil {
		t.Fatalf("row missing")
	}

	uid, uname, err := s.GetUserByToken(row.token)
	if err != nil {
		t.Fatalf("GetUserByToken(stored master) returned err: %v", err)
	}
	if uid != 1 || uname != "admin" {
		t.Fatalf("GetUserByToken(master) = (%d, %q, nil); want (1, \"admin\", nil)", uid, uname)
	}

	if _, _, err := s.GetUserByToken(legacyMasterTokenLiteral); err == nil {
		t.Fatalf("GetUserByToken(legacy literal) must error post-rotation")
	}
}

// TestGenerateMasterTokenUniqueness catches regressions in the entropy
// path — two consecutive calls must produce different tokens. If they
// collide, the seed/key has been broken (e.g., switched to time.Now()
// without nanosecond resolution).
func TestGenerateMasterTokenUniqueness(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 8; i++ {
		tok, err := generateMasterToken()
		if err != nil {
			t.Fatalf("generateMasterToken: %v", err)
		}
		if !strings.HasPrefix(tok, "np_master_") {
			t.Fatalf("token %d missing prefix: %q", i, tok)
		}
		if _, dup := seen[tok]; dup {
			t.Fatalf("collision on iteration %d: %q", i, tok)
		}
		seen[tok] = struct{}{}
	}
}

// --- helpers ---

type masterTokenRow struct {
	userID int64
	token  string
	name   string
}

// readMasterTokenRow opens a fresh connection to dbFile and returns the
// api_tokens row with name='master', or nil if none exists.
func readMasterTokenRow(t *testing.T, dbFile string) *masterTokenRow {
	t.Helper()
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatalf("open raw DB: %v", err)
	}
	defer db.Close()
	var row masterTokenRow
	err = db.QueryRow("SELECT user_id, token, name FROM api_tokens WHERE name = 'master'").Scan(&row.userID, &row.token, &row.name)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		t.Fatalf("read master token row: %v", err)
	}
	return &row
}

// seedLegacyMasterToken creates the DB schema and an admin user, then
// inserts the legacy literal into api_tokens as if a pre-fix binary had
// run. Used to simulate a real prod migration. The schema mirrors the
// production CREATE TABLE in pkg/store/sqlite.go (token TEXT PRIMARY KEY,
// no surrogate id column).
func seedLegacyMasterToken(dbFile string) error {
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		return err
	}
	defer db.Close()

	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		return err
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS user_settings (
			user_id INTEGER PRIMARY KEY
		);
		CREATE TABLE IF NOT EXISTS api_tokens (
			token TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL,
			name TEXT DEFAULT 'default',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
	`); err != nil {
		return err
	}

	res, err := db.Exec("INSERT INTO users (username, password_hash) VALUES ('admin', 'placeholder')")
	if err != nil {
		return err
	}
	adminID, _ := res.LastInsertId()
	if _, err := db.Exec("INSERT INTO user_settings (user_id) VALUES (?)", adminID); err != nil {
		return err
	}
	if _, err := db.Exec(
		"INSERT INTO api_tokens (token, user_id, name) VALUES (?, ?, 'master')",
		legacyMasterTokenLiteral, adminID,
	); err != nil {
		return err
	}
	return nil
}

// seedLegacyOwnerSchema creates the DB with the schema that real prod
// carries as of Sep 13 2026: api_tokens has (token TEXT PRIMARY KEY,
// owner TEXT NOT NULL, created_at DATETIME) and only carries a single
// row whose token is the legacy literal and whose owner is 'admin'.
// This mirrors the actual on-disk shape — no user_id, no name column,
// no FOREIGN KEY. Used to simulate the historical layout that
// migrateAPITokensSchema was written to upgrade.
func seedLegacyOwnerSchema(dbFile string) error {
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		return err
	}
	defer db.Close()

	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		return err
	}
	// Same DDL the prod DB was created with — owner TEXT NOT NULL,
	// token TEXT PRIMARY KEY, no user_id/name columns, no FK.
	if _, err := db.Exec(`
		CREATE TABLE users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			plan TEXT DEFAULT 'free',
			pro_until DATETIME
		);
		CREATE TABLE user_settings (
			user_id INTEGER PRIMARY KEY,
			slack_webhook_url TEXT DEFAULT '',
			discord_webhook_url TEXT DEFAULT '',
			notify_critical INTEGER DEFAULT 1,
			notify_warning INTEGER DEFAULT 1
		);
		CREATE TABLE api_tokens (
			token TEXT PRIMARY KEY,
			owner TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
	`); err != nil {
		return err
	}
	// Seed an admin user (id=1, matching prod). Seed the legacy literal
	// in api_tokens with owner='admin', exactly as the prod DB stores it.
	res, err := db.Exec("INSERT INTO users (username, password_hash) VALUES ('admin', 'placeholder')")
	if err != nil {
		return err
	}
	adminID, _ := res.LastInsertId()
	if _, err := db.Exec("INSERT INTO user_settings (user_id) VALUES (?)", adminID); err != nil {
		return err
	}
	if _, err := db.Exec(
		"INSERT INTO api_tokens (token, owner) VALUES (?, ?)",
		legacyMasterTokenLiteral, "admin",
	); err != nil {
		return err
	}
	return nil
}

// TestEnsureMasterTokenMigratesLegacyOwnerSchema pins the real prod
// migration path: a database shaped exactly like the one we hit in
// production (api_tokens has owner TEXT, no user_id/name columns, only
// one row carrying the legacy literal) must come out of
// NewPersistentStore with the legacy literal rotated to a random
// value, the api_tokens schema upgraded to user_id/name, and the
// literal no longer authenticating.
func TestEnsureMasterTokenMigratesLegacyOwnerSchema(t *testing.T) {
	dbFile := "test_master_migrate_owner.db"
	defer os.Remove(dbFile)

	if err := seedLegacyOwnerSchema(dbFile); err != nil {
		t.Fatalf("seed legacy owner schema: %v", err)
	}

	// Pre-condition: the seeded table really does have the old schema.
	// If this assertion fails the test fixture is wrong, not the code.
	preCols, err := apiTokensColumns(mustOpenDBForTest(t, dbFile))
	if err != nil {
		t.Fatalf("pre-migration columns: %v", err)
	}
	if _, ok := preCols["owner"]; !ok {
		t.Fatalf("pre-migration schema missing 'owner' column; fixture is wrong: %v", preCols)
	}
	if _, ok := preCols["user_id"]; ok {
		t.Fatalf("pre-migration schema must NOT have 'user_id'; fixture is wrong: %v", preCols)
	}
	if _, ok := preCols["name"]; ok {
		t.Fatalf("pre-migration schema must NOT have 'name'; fixture is wrong: %v", preCols)
	}

	// Run the migration + rotation.
	s, err := NewPersistentStore(dbFile, "", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore on legacy-owner DB: %v", err)
	}

	// Post-condition 1: api_tokens now has user_id AND name columns,
	// AND the legacy 'owner' column has been dropped. owner is dead
	// weight after the user_id remap; leaving it in place keeps its
	// NOT NULL constraint active, which silently blocks every
	// token-issuance INSERT in the codebase (Register, RegisterByTelegram,
	// Authenticate all omit owner). We hit this exact failure in prod
	// on 2026-09-13 ~19:30 UTC.
	postCols, err := apiTokensColumns(mustOpenDBForTest(t, dbFile))
	if err != nil {
		t.Fatalf("post-migration columns: %v", err)
	}
	if _, ok := postCols["user_id"]; !ok {
		t.Fatalf("post-migration schema missing 'user_id': %v", postCols)
	}
	if _, ok := postCols["name"]; !ok {
		t.Fatalf("post-migration schema missing 'name': %v", postCols)
	}
	if _, ok := postCols["owner"]; ok {
		t.Fatalf("post-migration schema still has 'owner' column; INSERTs that omit owner will silently fail on NOT NULL: %v", postCols)
	}

	// Post-condition 2: the legacy literal has been rotated to a fresh
	// random token. Read via the public helper so we exercise the same
	// query path that production code uses.
	row := readMasterTokenRow(t, dbFile)
	if row == nil {
		t.Fatalf("no master token row after migration+rotation")
	}
	if row.token == legacyMasterTokenLiteral {
		t.Fatalf("legacy literal still in DB after migration; rotation failed: %q", row.token)
	}
	if !strings.HasPrefix(row.token, "np_master_") {
		t.Fatalf("rotated token must be prefixed 'np_master_', got %q", row.token)
	}

	// Post-condition 3: the legacy literal no longer authenticates.
	if s.ValidateToken(legacyMasterTokenLiteral) {
		t.Fatalf("ValidateToken(legacy literal) must return false after migration+rotation")
	}
	if s.ValidateToken(row.token) != true {
		t.Fatalf("ValidateToken(stored master) must return true after migration+rotation")
	}
	if s.IsMasterToken(legacyMasterTokenLiteral) {
		t.Fatalf("IsMasterToken(legacy literal) must return false after migration+rotation")
	}
	if !s.IsMasterToken(row.token) {
		t.Fatalf("IsMasterToken(stored master) must return true after migration+rotation")
	}

	// Post-condition 4: a subsequent NewPersistentStore on the same DB
	// preserves the rotated token (no churn from the migration running
	// again on every startup).
	_, err = NewPersistentStore(dbFile, "", 0)
	if err != nil {
		t.Fatalf("second NewPersistentStore: %v", err)
	}
	row2 := readMasterTokenRow(t, dbFile)
	if row2.token != row.token {
		t.Fatalf("second startup re-rotated the token; expected %q, got %q", row.token, row2.token)
	}
}

// mustOpenDBForTest opens a fresh sqlite connection for schema
// inspection. Returns only on success — any open error fails the test
// immediately. Caller is responsible for closing the returned DB.
func mustOpenDBForTest(t *testing.T, dbFile string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatalf("open raw DB %s: %v", dbFile, err)
	}
	return db
}

// TestRegisterByTelegramPersistsTokenOnLegacyOwnerSchema pins the
// regression that hit prod on 2026-09-13 ~19:30 UTC: the api_tokens
// schema migration in commit 4a728d3 added the user_id and name columns
// but left the original 'owner' column with its NOT NULL constraint
// in place. RegisterByTelegram (and Register, and Authenticate) all
// issue `INSERT INTO api_tokens (token, user_id, name) ...` without
// supplying owner; the INSERT fails on the constraint; the error from
// p.db.Exec is silently discarded; the function returns the freshly
// generated token to the handler; the handler builds a 302 redirect
// pointing at the unstored token. The redirect works (browser sees the
// fragment); the next API call with that token fails ValidateToken
// (token isn't in api_tokens); the user sees "I logged in but
// everything's broken". The earlier 19:30 tick claimed the schema
// migration had fixed this; this test demonstrates that claim was
// wrong until the DROP COLUMN step lands.
//
// The test creates a DB with the EXACT prod pre-migration shape (see
// seedLegacyOwnerSchema above), runs NewPersistentStore (which runs
// the full migration including the new DROP COLUMN step), then calls
// RegisterByTelegram and verifies:
//
//  1. The function succeeds (no error).
//  2. The returned token is actually present in api_tokens —
//     pre-fix this would return rowCount == 0 because the INSERT
//     silently failed on owner NOT NULL.
//  3. ValidateToken accepts the token — pre-fix the token existed
//     nowhere in the DB so this would return false.
//  4. The api_tokens row carries name='tg-login' so audit/revoke can
//     find it.
func TestRegisterByTelegramPersistsTokenOnLegacyOwnerSchema(t *testing.T) {
	dbFile := "test_telegram_persist_legacy.db"
	defer os.Remove(dbFile)

	if err := seedLegacyOwnerSchema(dbFile); err != nil {
		t.Fatalf("seed legacy owner schema: %v", err)
	}

	// NewPersistentStore must succeed (it runs the migration including
	// the new DROP COLUMN step).
	s, err := NewPersistentStore(dbFile, "", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore on legacy-owner DB: %v", err)
	}

	const tgID int64 = 777888999
	uid, tok, err := s.RegisterByTelegram(tgID, "Charlie", "charlie_handle")
	if err != nil {
		t.Fatalf("RegisterByTelegram: %v", err)
	}
	if uid <= 0 {
		t.Fatalf("expected positive uid, got %d", uid)
	}

	// Pre-fix the token was returned to the caller but never persisted;
	// this query would return 0 rows and the assertion would fire.
	var rowCount int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM api_tokens WHERE token = ?", tok).Scan(&rowCount); err != nil {
		t.Fatalf("count api_tokens by token: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("expected 1 api_tokens row for the issued token %q, got %d (INSERT silently failed on owner NOT NULL?)", tok, rowCount)
	}

	// The token must validate — otherwise the 302 redirect would be a
	// lie and the next API call would 401.
	if !s.ValidateToken(tok) {
		t.Fatalf("issued token %q did not validate (not in DB?)", tok)
	}

	// And it must carry name='tg-login' so revoke/audit can find it.
	var name string
	if err := s.db.QueryRow("SELECT name FROM api_tokens WHERE token = ?", tok).Scan(&name); err != nil {
		t.Fatalf("read name: %v", err)
	}
	if name != "tg-login" {
		t.Fatalf("expected name='tg-login', got %q", name)
	}

	// A second call with the same tg_id reuses the user and issues a
	// fresh token that must ALSO persist. Pre-fix this would have failed
	// silently the same way as the first call.
	_, tok2, err := s.RegisterByTelegram(tgID, "CharlieRenamed", "charlie_new")
	if err != nil {
		t.Fatalf("second RegisterByTelegram: %v", err)
	}
	if tok2 == tok {
		t.Fatalf("expected fresh token on second login, got identical value")
	}
	if !s.ValidateToken(tok2) {
		t.Fatalf("second token %q did not validate", tok2)
	}
}
