package store

import (
	"path/filepath"
	"testing"
)

func TestAcknowledgeIncident(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "test.db"), "", 0)
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	uid, _, err := s.Register("ops1", "secret123")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	s.BindNode("node-ack", uid)
	s.CreateIncident("node-ack", "warning", "Disk filling", "usage 91%")

	incs := s.GetActiveIncidents(uid)
	if len(incs) != 1 {
		t.Fatalf("expected 1 incident, got %d", len(incs))
	}

	// First ack should succeed.
	ok, err := s.AcknowledgeIncident(incs[0].ID)
	if err != nil || !ok {
		t.Fatalf("first ack: ok=%v err=%v", ok, err)
	}

	// Second ack on the still-open incident should be a no-op (idempotent),
	// RowsAffected==0 ⇒ ok=false.
	ok, err = s.AcknowledgeIncident(incs[0].ID)
	if err != nil || ok {
		t.Fatalf("second ack should report ok=false, got ok=%v err=%v", ok, err)
	}

	// After resolving, a further ack must be refused.
	if err := s.ResolveIncident(incs[0].ID, 1); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	ok, err = s.AcknowledgeIncident(incs[0].ID)
	if err != nil || ok {
		t.Fatalf("ack-after-resolve should fail, got ok=%v err=%v", ok, err)
	}

	// Unknown id ⇒ ok=false, no error.
	ok, err = s.AcknowledgeIncident("999999")
	if err != nil || ok {
		t.Fatalf("ack of nonexistent should return ok=false, got ok=%v err=%v", ok, err)
	}
}
