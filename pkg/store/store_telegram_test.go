package store

import (
	"os"
	"strings"
	"testing"
)

// TestRegisterByTelegram covers the SaaS entry-point introduced for the
// Telegram Login Widget / deep-link flow. The handler in cmd/server/main.go
// already verified the HMAC against the bot token before calling this method,
// so here we only need to assert the store-level guarantees:
//
//   1. A fresh tg_id creates a backing user, a telegram_users row, and a
//      single api_tokens row carrying the returned token.
//   2. A repeat tg_id reuses the existing user, issues a fresh token, and
//      does NOT create a second telegram_users row.
//   3. The issued token is valid against ValidateToken (so the dashboard
//      can actually use it after pickup).
//   4. The api_tokens.name is "tg-login" so audit/revoke paths can spot it.
func TestRegisterByTelegram(t *testing.T) {
	dbPath := "/tmp/test_telegram_register.db"
	os.Remove(dbPath)
	defer os.Remove(dbPath)

	st, err := NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	// 1. Fresh tg_id creates user + telegram_users row + token.
	const tgID int64 = 987654321
	uid1, tok1, err := st.RegisterByTelegram(tgID, "Alice", "alice_handle")
	if err != nil {
		t.Fatalf("first RegisterByTelegram failed: %v", err)
	}
	if uid1 <= 0 {
		t.Fatalf("expected positive uid, got %d", uid1)
	}
	if !strings.HasPrefix(tok1, "np_tg_") {
		t.Fatalf("expected np_tg_ token prefix, got %q", tok1)
	}
	if !st.ValidateToken(tok1) {
		t.Fatalf("issued token %q did not validate", tok1)
	}

	var telegramUsersCount int
	if err := st.db.QueryRow("SELECT COUNT(*) FROM telegram_users WHERE tg_id = ?", tgID).Scan(&telegramUsersCount); err != nil {
		t.Fatalf("count telegram_users: %v", err)
	}
	if telegramUsersCount != 1 {
		t.Fatalf("expected 1 telegram_users row after first register, got %d", telegramUsersCount)
	}

	// Persist username/first_name must have been stored.
	var storedUser, storedName string
	if err := st.db.QueryRow("SELECT tg_username, tg_first_name FROM telegram_users WHERE tg_id = ?", tgID).Scan(&storedUser, &storedName); err != nil {
		t.Fatalf("read telegram_users: %v", err)
	}
	if storedUser != "alice_handle" || storedName != "Alice" {
		t.Fatalf("tg_username/tg_first_name not stored: got %q/%q", storedUser, storedName)
	}

	// 2. Same tg_id again: reuses user, new token, no duplicate telegram_users row.
	uid2, tok2, err := st.RegisterByTelegram(tgID, "AliceRenamed", "alice_new")
	if err != nil {
		t.Fatalf("second RegisterByTelegram failed: %v", err)
	}
	if uid2 != uid1 {
		t.Fatalf("expected uid to be reused on second login, got %d vs %d", uid2, uid1)
	}
	if tok2 == tok1 {
		t.Fatalf("expected fresh token on second login, got identical value")
	}
	if !st.ValidateToken(tok2) {
		t.Fatalf("second token %q did not validate", tok2)
	}

	if err := st.db.QueryRow("SELECT COUNT(*) FROM telegram_users WHERE tg_id = ?", tgID).Scan(&telegramUsersCount); err != nil {
		t.Fatalf("count telegram_users after reuse: %v", err)
	}
	if telegramUsersCount != 1 {
		t.Fatalf("telegram_users row leaked on reuse, got %d", telegramUsersCount)
	}

	// 3. Distinct tg_id creates a distinct user.
	const otherTG int64 = 111222333
	uid3, tok3, err := st.RegisterByTelegram(otherTG, "Bob", "")
	if err != nil {
		t.Fatalf("third RegisterByTelegram failed: %v", err)
	}
	if uid3 == uid1 {
		t.Fatalf("expected distinct uid for distinct tg_id, both = %d", uid3)
	}
	if !st.ValidateToken(tok3) {
		t.Fatalf("third token did not validate")
	}

	// 4. api_tokens row carries name='tg-login' so revoke/audit can find it.
	var nameCount int
	if err := st.db.QueryRow("SELECT COUNT(*) FROM api_tokens WHERE user_id = ? AND name = 'tg-login'", uid1).Scan(&nameCount); err != nil {
		t.Fatalf("count tg-login tokens: %v", err)
	}
	if nameCount != 2 {
		t.Fatalf("expected 2 tg-login tokens for uid1 (one per login), got %d", nameCount)
	}
}

// TestRegisterByTelegramDifferentFirstNamePersisted guards against a regression
// where the second-call path silently drops updated profile fields. Telegram
// users can rename themselves; if we never refresh tg_first_name, the dashboard
// greeting would lie forever.
func TestRegisterByTelegramDifferentFirstNamePersisted(t *testing.T) {
	dbPath := "/tmp/test_telegram_rename.db"
	os.Remove(dbPath)
	defer os.Remove(dbPath)

	st, err := NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	const tgID int64 = 555000111
	if _, _, err := st.RegisterByTelegram(tgID, "OldName", "handle"); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if _, _, err := st.RegisterByTelegram(tgID, "NewName", "handle"); err != nil {
		t.Fatalf("second register: %v", err)
	}

	var name string
	if err := st.db.QueryRow("SELECT tg_first_name FROM telegram_users WHERE tg_id = ?", tgID).Scan(&name); err != nil {
		t.Fatalf("read name: %v", err)
	}
	if name != "NewName" {
		t.Fatalf("expected tg_first_name to update, still %q", name)
	}
}
