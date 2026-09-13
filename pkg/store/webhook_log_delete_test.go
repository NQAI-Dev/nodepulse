package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func TestDeleteWebhookDelivery_ScopesByUser(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("init store: %v", err)
	}

	now := time.Now().Unix()
	rows := []protocol.WebhookDelivery{
		{UserID: 7, IncidentID: "1", Event: "incident.created", URL: "https://hook.example/a", Attempts: 1, Status: 200, OK: true, Timestamp: now},
		{UserID: 7, IncidentID: "2", Event: "incident.created", URL: "https://hook.example/a", Attempts: 1, Status: 200, OK: true, Timestamp: now + 1},
		{UserID: 9, IncidentID: "3", Event: "incident.created", URL: "https://hook.example/b", Attempts: 1, Status: 200, OK: true, Timestamp: now + 2},
	}
	if _, err := s.FlushWebhookDeliveries(rows); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// user 9 must not be able to delete user 7's row.
	otherRows, _ := s.WebhookDeliveries(7, 10)
	if len(otherRows) != 2 {
		t.Fatalf("expected 2 rows for user 7, got %d", len(otherRows))
	}
	deleted, err := s.DeleteWebhookDelivery(9, otherRows[0].ID)
	if err != nil {
		t.Fatalf("cross-tenant delete should error out cleanly, got %v", err)
	}
	if deleted {
		t.Fatal("cross-tenant delete must report deleted=false")
	}
	if rowsLeft, _ := s.WebhookDeliveries(7, 10); len(rowsLeft) != 2 {
		t.Fatalf("user 7's rows must remain intact, got %d", len(rowsLeft))
	}

	// Owner deletes the row successfully.
	deleted, err = s.DeleteWebhookDelivery(7, otherRows[0].ID)
	if err != nil {
		t.Fatalf("owner delete: %v", err)
	}
	if !deleted {
		t.Fatal("owner delete must report deleted=true")
	}
	rowsLeft, _ := s.WebhookDeliveries(7, 10)
	if len(rowsLeft) != 1 {
		t.Fatalf("expected 1 row left for user 7, got %d", len(rowsLeft))
	}

	// Deleting the same row again must report deleted=false without error.
	deleted, err = s.DeleteWebhookDelivery(7, otherRows[0].ID)
	if err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if deleted {
		t.Fatal("second delete must report deleted=false")
	}

	// Unknown id must also report deleted=false without error.
	deleted, err = s.DeleteWebhookDelivery(7, 999999)
	if err != nil {
		t.Fatalf("unknown id delete: %v", err)
	}
	if deleted {
		t.Fatal("unknown id must report deleted=false")
	}
}
