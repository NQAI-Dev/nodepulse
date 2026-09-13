package store

import (
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func TestGetPublicMaintenanceWindows(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()

	// 1. Create active window
	_, err := s.CreateMaintenanceWindow(1, protocol.MaintenanceWindowRequest{
		Scope:     "node",
		NodeIDs:   []string{"node-1", "node-2"},
		Reason:    "Kernel upgrade",
		StartUnix: now - 300,
		EndUnix:   now + 3600,
	}, "admin")
	if err != nil {
		t.Fatalf("failed to create window: %v", err)
	}

	// 2. Create past expired window
	_, err = s.db.Exec(`INSERT INTO maintenance_windows
		(user_id, scope, reason, node_ids, start_unix, end_unix, created_at, created_by)
		VALUES (1, 'user', 'Old maintenance', '', ?, ?, ?, 'admin')`, now-7200, now-3600, now-7200)
	if err != nil {
		t.Fatalf("failed to insert expired window: %v", err)
	}

	// 3. Query public maintenance
	pub, err := s.GetPublicMaintenanceWindows()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(pub) != 1 {
		t.Fatalf("expected 1 active/upcoming window, got %d", len(pub))
	}

	w := pub[0]
	if w.Reason != "Kernel upgrade" {
		t.Errorf("reason mismatch: got %q", w.Reason)
	}
	if !w.Active {
		t.Errorf("expected window to be active")
	}
	if len(w.NodeIDs) != 2 || w.NodeIDs[0] != "node-1" {
		t.Errorf("nodeIDs mismatch: got %v", w.NodeIDs)
	}
}
