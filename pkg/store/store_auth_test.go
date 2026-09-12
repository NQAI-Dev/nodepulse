package store

import (
	"os"
	"testing"
)

func TestAuthFlow(t *testing.T) {
	dbPath := "/tmp/test_auth.db"
	os.Remove(dbPath)
	defer os.Remove(dbPath)

	st, err := NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	// Register user
	uid, token, err := st.Register("testuser", "secret123")
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}
	if uid == 0 || token == "" {
		t.Fatalf("invalid uid or token")
	}

	// Authenticate
	authUID, authToken, err := st.Authenticate("testuser", "secret123")
	if err != nil {
		t.Fatalf("auth failed: %v", err)
	}
	if authUID != uid || authToken != token {
		t.Fatalf("mismatched auth result")
	}

	// Reject wrong password
	_, _, err = st.Authenticate("testuser", "wrongpass")
	if err == nil {
		t.Fatalf("expected error on wrong password")
	}
}
