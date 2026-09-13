package store

import (
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// TestCloseMaintenanceWindow_OpenEnded — closing an open-ended window
// stamps end_unix and the row stops counting as active.
func TestCloseMaintenanceWindow_OpenEnded(t *testing.T) {
	s := newMaintenanceTestStore(t)
	win, err := s.CreateMaintenanceWindow(1, protocol.MaintenanceWindowRequest{
		Scope:     "user",
		StartUnix: 1,
		EndUnix:   0, // open-ended
	}, "alice")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	n, err := s.CloseMaintenanceWindow(1, win.ID)
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 row affected, got %d", n)
	}

	// Row stays in DB but end_unix is now set, so the active list drops it.
	active, err := s.ListMaintenanceWindows(1, false)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("expected 0 active windows after close, got %d", len(active))
	}
	all, err := s.ListMaintenanceWindows(1, true)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 1 || all[0].EndUnix == 0 {
		t.Fatalf("row should survive with end_unix set: %+v", all[0])
	}
	if all[0].EndUnix > time.Now().Unix() {
		t.Fatalf("end_unix in the future: %d", all[0].EndUnix)
	}
}

// TestCloseMaintenanceWindow_Idempotent — closing an already-closed window
// is a no-op (returns 0).
func TestCloseMaintenanceWindow_Idempotent(t *testing.T) {
	s := newMaintenanceTestStore(t)
	win, _ := s.CreateMaintenanceWindow(1, protocol.MaintenanceWindowRequest{
		Scope: "user", StartUnix: 1, EndUnix: 0,
	}, "alice")

	if n, err := s.CloseMaintenanceWindow(1, win.ID); err != nil || n != 1 {
		t.Fatalf("first close: n=%d err=%v", n, err)
	}
	if n, err := s.CloseMaintenanceWindow(1, win.ID); err != nil || n != 0 {
		t.Fatalf("second close should be no-op: n=%d err=%v", n, err)
	}
}

// TestCloseMaintenanceWindow_AlreadyEnded — closing a window that already
// has end_unix in the past must also be a no-op (end_unix != 0).
func TestCloseMaintenanceWindow_AlreadyEnded(t *testing.T) {
	s := newMaintenanceTestStore(t)
	win, _ := s.CreateMaintenanceWindow(1, protocol.MaintenanceWindowRequest{
		Scope: "user", StartUnix: 100, EndUnix: 200,
	}, "alice")

	if n, err := s.CloseMaintenanceWindow(1, win.ID); err != nil || n != 0 {
		t.Fatalf("close on bounded window: n=%d err=%v", n, err)
	}
}

// TestCloseMaintenanceWindow_CrossTenant — non-admin can't close
// another user's window. Admin can.
func TestCloseMaintenanceWindow_CrossTenant(t *testing.T) {
	s := newMaintenanceTestStore(t)
	win, _ := s.CreateMaintenanceWindow(42, protocol.MaintenanceWindowRequest{
		Scope: "user", StartUnix: 1, EndUnix: 0,
	}, "bob")

	if n, err := s.CloseMaintenanceWindow(7, win.ID); err != nil || n != 0 {
		t.Fatalf("non-owner close: n=%d err=%v", n, err)
	}
	if n, err := s.CloseMaintenanceWindow(1, win.ID); err != nil || n != 1 {
		t.Fatalf("admin close: n=%d err=%v", n, err)
	}
}

// TestCloseMaintenanceWindow_UnknownID — closing a missing id returns 0,
// not an error.
func TestCloseMaintenanceWindow_UnknownID(t *testing.T) {
	s := newMaintenanceTestStore(t)
	if n, err := s.CloseMaintenanceWindow(1, 999999); err != nil || n != 0 {
		t.Fatalf("unknown id close: n=%d err=%v", n, err)
	}
}
