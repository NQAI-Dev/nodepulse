package store

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// makeTestIncident inserts a fresh incident row directly and returns its
// stringified id. Inline INSERT matches the pattern used by fleet_test.go
// and incidents_histogram_test.go — a helper would just be sugar over the
// same one-line Exec.
func makeTestIncident(t *testing.T, s *PersistentStore, nodeID string) string {
	t.Helper()
	now := time.Now().Unix()
	res, err := s.db.Exec(
		`INSERT INTO incidents (node_id, severity, title, detail, started_at, resolved) VALUES (?, ?, ?, ?, ?, 0)`,
		nodeID, "warning", "test", "", now,
	)
	if err != nil {
		t.Fatalf("insert incident: %v", err)
	}
	id, _ := res.LastInsertId()
	return strconv.FormatInt(id, 10)
}

// TestAddIncidentNote covers the happy path + the empty-body validation.
func TestAddIncidentNote(t *testing.T) {
	s := newTestStore(t)
	id := makeTestIncident(t, s, "note-test-node")

	// empty body
	if _, err := s.AddIncidentNote(id, 1, "alice", "   "); err != ErrIncidentNoteEmpty {
		t.Fatalf("expected ErrIncidentNoteEmpty, got %v", err)
	}

	// happy path
	n, err := s.AddIncidentNote(id, 1, "alice", "Investigating — disk full")
	if err != nil {
		t.Fatalf("AddIncidentNote: %v", err)
	}
	if n.ID == 0 || n.Body != "Investigating — disk full" || n.Username != "alice" {
		t.Fatalf("unexpected note: %+v", n)
	}
	if n.IncidentID != id {
		t.Fatalf("incident_id mismatch: got %q want %q", n.IncidentID, id)
	}
}

// TestAddIncidentNoteTooLong checks the upper bound on body length.
func TestAddIncidentNoteTooLong(t *testing.T) {
	s := newTestStore(t)
	id := makeTestIncident(t, s, "long-note-node")

	huge := strings.Repeat("a", 1025)
	if _, err := s.AddIncidentNote(id, 1, "alice", huge); err != ErrIncidentNoteTooLong {
		t.Fatalf("expected ErrIncidentNoteTooLong, got %v", err)
	}
}

// TestAddIncidentNoteUnknownIncident — adding to a missing id must 404,
// not silently insert or 500.
func TestAddIncidentNoteUnknownIncident(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.AddIncidentNote("999999", 1, "alice", "ghost"); err != ErrIncidentNotFound {
		t.Fatalf("expected ErrIncidentNotFound, got %v", err)
	}
}

// TestListIncidentNotesOrdering — chronological order so the UI can render
// top-to-bottom without re-sorting.
func TestListIncidentNotesOrdering(t *testing.T) {
	s := newTestStore(t)
	id := makeTestIncident(t, s, "ordered-node")

	for _, body := range []string{"first", "second", "third"} {
		if _, err := s.AddIncidentNote(id, 1, "alice", body); err != nil {
			t.Fatalf("AddIncidentNote(%q): %v", body, err)
		}
	}
	notes, err := s.ListIncidentNotes(id)
	if err != nil {
		t.Fatalf("ListIncidentNotes: %v", err)
	}
	if len(notes) != 3 {
		t.Fatalf("expected 3 notes, got %d", len(notes))
	}
	want := []string{"first", "second", "third"}
	for i, n := range notes {
		if n.Body != want[i] {
			t.Fatalf("note[%d]=%q want %q", i, n.Body, want[i])
		}
	}
}

// TestListIncidentNotesEmpty — no notes => empty slice, not nil-deref in UI.
func TestListIncidentNotesEmpty(t *testing.T) {
	s := newTestStore(t)
	id := makeTestIncident(t, s, "empty-notes-node")
	notes, err := s.ListIncidentNotes(id)
	if err != nil {
		t.Fatalf("ListIncidentNotes: %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("expected empty list, got %d", len(notes))
	}
}
