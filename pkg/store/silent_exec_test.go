package store

import (
	"os"
	"strings"
	"testing"
)

// TestSilentExecAntiPatternRemoved pins the lesson from the 2026-09-13
// ~19:30 UTC prod incident: Register / RegisterByTelegram / Authenticate
// used to discard every p.db.Exec error after the first INSERT, which
// silently masked schema drift. The api_tokens.owner NOT NULL constraint
// blocked every token INSERT, the function returned the freshly generated
// (but never persisted) token to the handler, and the user got a 302
// pointing at a token ValidateToken would reject on every subsequent
// request. Today's fix (migrateAPITokensSchema step 5: DROP COLUMN owner)
// removed the immediate cause; this test pins the meta-fix: the silent
// anti-pattern itself. Drop api_tokens, call each auth entrypoint, and
// assert every one surfaces a non-nil error containing the function name
// and operation context — so the next schema drift won't get masked the
// same way.
func TestSilentExecAntiPatternRemoved(t *testing.T) {
	dbPath := "/tmp/test_silent_exec_pattern.db"
	os.Remove(dbPath)
	defer os.Remove(dbPath)

	st, err := NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}

	// Simulate the exact failure mode from prod: the target table is gone
	// (operator ran a migration that dropped it, or the table was never
	// created on a partially-migrated install). Every INSERT into
	// api_tokens will now fail with "no such table".
	if _, err := st.db.Exec("DROP TABLE api_tokens"); err != nil {
		t.Fatalf("drop api_tokens: %v", err)
	}

	t.Run("RegisterReturnsError", func(t *testing.T) {
		uid, token, err := st.Register("alice", "secret123")
		if err == nil {
			t.Fatalf("Register must return error when api_tokens is missing, got nil (uid=%d token=%q)", uid, token)
		}
		if uid != 0 || token != "" {
			t.Fatalf("Register must return zero values on error, got uid=%d token=%q", uid, token)
		}
		// Context chain must mention what failed so operators can grep journalctl.
		if !strings.Contains(err.Error(), "issue default token") {
			t.Fatalf("Register error must mention token issuance, got %q", err.Error())
		}
	})

	t.Run("RegisterByTelegramReturnsErrorOnFresh", func(t *testing.T) {
		uid, token, err := st.RegisterByTelegram(1234567890, "Fresh", "fresh_handle")
		if err == nil {
			t.Fatalf("RegisterByTelegram (fresh tg_id) must return error when api_tokens is missing, got nil (uid=%d token=%q)", uid, token)
		}
		if uid != 0 || token != "" {
			t.Fatalf("RegisterByTelegram must return zero values on error, got uid=%d token=%q", uid, token)
		}
		// Fresh path goes through users + user_settings + telegram_users +
		// api_tokens. The first failing call is user_settings because we
		// only dropped api_tokens — but on real schema drift the table
		// that vanishes could be any of them. Just assert the error
		// chain is non-empty and mentions the operation.
		if !strings.Contains(err.Error(), "create user_settings") &&
			!strings.Contains(err.Error(), "link telegram_users") &&
			!strings.Contains(err.Error(), "issue tg-login token") {
			t.Fatalf("RegisterByTelegram error must surface the failing operation, got %q", err.Error())
		}
	})

	t.Run("RegisterByTelegramReturnsErrorOnExisting", func(t *testing.T) {
		// Pre-fix: RegisterByTelegram with an existing tg_id would refresh
		// telegram_users (silently) and then issue a fresh token (silently),
		// returning uid+token to the caller with no error. The handler then
		// 302'd with a token that ValidateToken would reject. Pin the
		// post-fix behavior: any of those INSERTs failing must propagate.
		// Pre-create a telegram_users row to skip the fresh-user branch.
		if _, err := st.db.Exec(`INSERT INTO telegram_users (tg_id, user_id, tg_username, tg_first_name)
			VALUES (?, 1, 'preset', 'Preset')`, int64(555000111)); err != nil {
			t.Fatalf("seed telegram_users: %v", err)
		}
		uid, token, err := st.RegisterByTelegram(555000111, "Alice", "alice_handle")
		if err == nil {
			t.Fatalf("RegisterByTelegram (existing tg_id) must return error when api_tokens is missing, got nil (uid=%d token=%q)", uid, token)
		}
		if uid != 0 || token != "" {
			t.Fatalf("RegisterByTelegram must return zero values on error, got uid=%d token=%q", uid, token)
		}
		// Either the telegram_users UPDATE or the api_tokens INSERT failed
		// (depending on whether firstName/username are non-empty). Both
		// branches must surface.
		if !strings.Contains(err.Error(), "refresh telegram profile") &&
			!strings.Contains(err.Error(), "issue tg-login token") {
			t.Fatalf("RegisterByTelegram (existing) error must surface the failing operation, got %q", err.Error())
		}
	})

	t.Run("AuthenticateReturnsErrorOnTokenInsertFailure", func(t *testing.T) {
		// Authenticate's INSERT only runs when the user has NO existing
		// token. To exercise that branch we need a user row but no
		// api_tokens row for them. api_tokens is dropped above, so the
		// first INSERT naturally fires.
		pwdHash := HashPassword("secret123")
		if _, err := st.db.Exec(`INSERT INTO users (username, password_hash) VALUES (?, ?)`, "lonely_user", pwdHash); err != nil {
			t.Fatalf("seed users: %v", err)
		}
		uid, token, err := st.Authenticate("lonely_user", "secret123")
		if err == nil {
			t.Fatalf("Authenticate must return error when api_tokens is missing, got nil (uid=%d token=%q)", uid, token)
		}
		if uid != 0 || token != "" {
			t.Fatalf("Authenticate must return zero values on error, got uid=%d token=%q", uid, token)
		}
		if !strings.Contains(err.Error(), "issue default token") {
			t.Fatalf("Authenticate error must mention token issuance, got %q", err.Error())
		}
	})
}
