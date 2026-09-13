package store

import (
	"path/filepath"
	"testing"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// newMaintenanceTestStore spins up an in-temp-dir persistent store with no
// alerter. Renamed to avoid colliding with newTestStore in metrics_test.go.
func newMaintenanceTestStore(t *testing.T) *PersistentStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}
	t.Cleanup(func() { _ = s }) // PersistentStore has no Close(); TempDir cleanup handles disk
	return s
}

func TestMaintenanceWindow_CreateListDelete(t *testing.T) {
	s := newMaintenanceTestStore(t)

	win, err := s.CreateMaintenanceWindow(1, protocol.MaintenanceWindowRequest{
		Scope:     "user",
		Reason:    "deploy v2",
		StartUnix: 1,                  // ancient start so it's "active" right now
		EndUnix:   0,                  // open-ended → not expired
	}, "alice")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if win.ID == 0 || win.UserID != 1 {
		t.Fatalf("bad window: %+v", win)
	}

	list, err := s.ListMaintenanceWindows(1, false)
	if err != nil || len(list) != 1 {
		t.Fatalf("expected 1 active window, got %d (err=%v)", len(list), err)
	}
	if list[0].Reason != "deploy v2" {
		t.Fatalf("reason lost: %+v", list[0])
	}

	deleted, err := s.DeleteMaintenanceWindow(1, win.ID)
	if err != nil || deleted != 1 {
		t.Fatalf("delete: n=%d err=%v", deleted, err)
	}

	list, _ = s.ListMaintenanceWindows(1, false)
	if len(list) != 0 {
		t.Fatalf("expected 0 after delete, got %d", len(list))
	}
}

func TestMaintenanceWindow_NodeScope(t *testing.T) {
	s := newMaintenanceTestStore(t)

	// node-scoped window: only the listed nodes are silenced.
	_, err := s.CreateMaintenanceWindow(7, protocol.MaintenanceWindowRequest{
		Scope:     "node",
		NodeIDs:   []string{"web-1", "web-2"},
		Reason:    "rolling restart",
		StartUnix: 100,
		EndUnix:   0, // open-ended
	}, "ops")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// user-7 owns both nodes; we tell the store via node_owners.
	_ = s.BindNode("web-1", 7)
	_ = s.BindNode("web-2", 7)
	_ = s.BindNode("db-1", 7)

	cases := []struct {
		node string
		want bool
	}{
		{"web-1", true},
		{"web-2", true},
		{"db-1", false},
	}
	for _, c := range cases {
		got, err := s.IsNodeSilenced(7, c.node, 200)
		if err != nil {
			t.Fatalf("IsNodeSilenced(%s): %v", c.node, err)
		}
		if got != c.want {
			t.Fatalf("IsNodeSilenced(%s) = %v, want %v", c.node, got, c.want)
		}
	}
}

func TestMaintenanceWindow_TimeBoundaries(t *testing.T) {
	s := newMaintenanceTestStore(t)

	// Window opens at 1000, closes at 2000.
	if _, err := s.CreateMaintenanceWindow(1, protocol.MaintenanceWindowRequest{
		Scope: "user", StartUnix: 1000, EndUnix: 2000,
	}, "alice"); err != nil {
		t.Fatal(err)
	}

	// Before the window: not silenced.
	if got, _ := s.IsNodeSilenced(1, "n1", 500); got {
		t.Fatal("pre-window should not silence")
	}
	// Inside the window: silenced.
	if got, _ := s.IsNodeSilenced(1, "n1", 1500); !got {
		t.Fatal("inside window must silence")
	}
	// After the window: not silenced.
	if got, _ := s.IsNodeSilenced(1, "n1", 3000); got {
		t.Fatal("post-window should not silence")
	}
}

func TestMaintenanceWindow_RejectsBadScope(t *testing.T) {
	s := newMaintenanceTestStore(t)

	if _, err := s.CreateMaintenanceWindow(1, protocol.MaintenanceWindowRequest{
		Scope: "tenant", StartUnix: 1, EndUnix: 2,
	}, "x"); err == nil {
		t.Fatal("expected error for unknown scope")
	}

	// node scope without node_ids must be rejected.
	if _, err := s.CreateMaintenanceWindow(1, protocol.MaintenanceWindowRequest{
		Scope: "node", NodeIDs: nil, StartUnix: 1, EndUnix: 2,
	}, "x"); err == nil {
		t.Fatal("expected error for node scope without ids")
	}

	// end before start must be rejected.
	if _, err := s.CreateMaintenanceWindow(1, protocol.MaintenanceWindowRequest{
		Scope: "user", StartUnix: 100, EndUnix: 50,
	}, "x"); err == nil {
		t.Fatal("expected error for end < start")
	}
}

func TestMaintenanceWindow_AutoClose(t *testing.T) {
	s := newMaintenanceTestStore(t)

	// Two open incidents on n1; open one on n2.
	_ = s.CreateIncident("n1", "warning", "High CPU", "x")
	_ = s.CreateIncident("n1", "critical", "Container Stopped", "y")
	_ = s.CreateIncident("n2", "warning", "Disk filling", "z")

	// Open a window covering n1 only.
	if _, err := s.CreateMaintenanceWindow(1, protocol.MaintenanceWindowRequest{
		Scope:     "node",
		NodeIDs:   []string{"n1"},
		StartUnix: 0, // since 1970 → definitely active
		EndUnix:   0,
	}, "alice"); err != nil {
		t.Fatal(err)
	}

	n, err := s.AutoCloseIncidentsInMaintenance(1, "n1", 100000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("autoclose n1: want 2, got %d", n)
	}

	// n2 must remain open.
	for _, inc := range s.GetActiveIncidents(1) {
		if inc.NodeID == "n2" && !inc.Resolved {
			// good
		} else if inc.NodeID == "n1" && inc.Resolved {
			// good
		} else if inc.NodeID == "n1" && !inc.Resolved {
			t.Fatalf("n1 incident should be auto-resolved: %+v", inc)
		}
	}
}

func TestMaintenanceWindow_DeleteScopesToOwner(t *testing.T) {
	s := newMaintenanceTestStore(t)

	win, err := s.CreateMaintenanceWindow(42, protocol.MaintenanceWindowRequest{
		Scope: "user", StartUnix: 1, EndUnix: 2,
	}, "bob")
	if err != nil {
		t.Fatal(err)
	}

	// Owner 99 cannot delete owner 42's window.
	if n, _ := s.DeleteMaintenanceWindow(99, win.ID); n != 0 {
		t.Fatalf("cross-tenant delete must be blocked, got n=%d", n)
	}
	// Owner 42 can.
	if n, _ := s.DeleteMaintenanceWindow(42, win.ID); n != 1 {
		t.Fatalf("owner delete: n=%d", n)
	}
}
