package store

import (
	"strconv"
	"testing"
	"time"
)

// TestIncidentTimeline_MergeOrder — ack/note/resolve events must appear
// in chronological order regardless of the order they were inserted.
func TestIncidentTimeline_MergeOrder(t *testing.T) {
	s := newTestStore(t)
	id := insertIncidentWithTimes(t, s, map[string]int64{
		"started_at":      1000,
		"acknowledged_at": 1100,
		"resolved_at":     1400,
	})
	note1, err := s.AddIncidentNote(id, 1, "alice", "checking logs")
	if err != nil {
		t.Fatalf("note1: %v", err)
	}
	note2, err := s.AddIncidentNote(id, 1, "bob", "restarted")
	if err != nil {
		t.Fatalf("note2: %v", err)
	}
	// Backdate both notes so the deterministic ack/note/note/resolve
	// ordering holds regardless of clock skew between the row's created_at
	// and the resolved_at we set above.
	_, _ = s.db.Exec("UPDATE incident_notes SET created_at = 1200 WHERE id = ?", note1.ID)
	_, _ = s.db.Exec("UPDATE incident_notes SET created_at = 1300 WHERE id = ?", note2.ID)

	events, err := s.IncidentTimeline(id)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	want := []string{"ack", "note", "note", "resolve"}
	if len(events) != len(want) {
		t.Fatalf("len=%d want %d", len(events), len(want))
	}
	for i, w := range want {
		if events[i].Kind != w {
			t.Fatalf("event[%d].Kind=%q want %q", i, events[i].Kind, w)
		}
	}
	if events[3].Reason != "manual" {
		t.Fatalf("resolve reason: got %q", events[3].Reason)
	}
}

// TestIncidentTimeline_NoEvents — incident exists but has not been ack'd,
// resolved, or commented on yet.
func TestIncidentTimeline_NoEvents(t *testing.T) {
	s := newTestStore(t)
	id := makeTestIncident(t, s, "empty-timeline-node")
	events, err := s.IncidentTimeline(id)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("want empty, got %d", len(events))
	}
}

// TestIncidentTimeline_UnknownIncident — empty events, no error. This is
// the contract the GET handler relies on.
func TestIncidentTimeline_UnknownIncident(t *testing.T) {
	s := newTestStore(t)
	events, err := s.IncidentTimeline("999999")
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	if events == nil || len(events) != 0 {
		t.Fatalf("want empty events, got %+v", events)
	}
}

// TestUserOwnsIncident_AdminBypass — admin (uid=1) is treated as owner of
// every incident so the audit path keeps working.
func TestUserOwnsIncident_AdminBypass(t *testing.T) {
	s := newTestStore(t)
	id := makeTestIncident(t, s, "admin-bypass-node")
	owns, err := s.UserOwnsIncident(1, id)
	if err != nil {
		t.Fatalf("owns: %v", err)
	}
	if !owns {
		t.Fatalf("admin should own every incident")
	}
}

// TestUserOwnsIncident_UnknownIncident — no rows => false (not an error).
func TestUserOwnsIncident_UnknownIncident(t *testing.T) {
	s := newTestStore(t)
	owns, err := s.UserOwnsIncident(2, "999999")
	if err != nil {
		t.Fatalf("owns: %v", err)
	}
	if owns {
		t.Fatalf("unknown incident must not be owned")
	}
}

// insertIncidentWithTimes is a focused helper for timeline ordering tests
// — gives us per-column control over the timestamps that drive merge order.
func insertIncidentWithTimes(t *testing.T, s *PersistentStore, cols map[string]int64) string {
	t.Helper()
	started := cols["started_at"]
	if started == 0 {
		started = time.Now().Unix()
	}
	resInt := 0
	if v, ok := cols["resolved"]; ok && v != 0 {
		resInt = 1
	}
	out, err := s.db.Exec(
		`INSERT INTO incidents (node_id, severity, title, detail, started_at, resolved) VALUES (?, ?, ?, ?, ?, ?)`,
		"timeline-node", "warning", "t", "", started, resInt,
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id, _ := out.LastInsertId()
	idStr := strconv.FormatInt(id, 10)

	if cols["acknowledged_at"] != 0 {
		if _, err := s.db.Exec("UPDATE incidents SET acknowledged_at = ? WHERE id = ?", cols["acknowledged_at"], id); err != nil {
			t.Fatalf("update acknowledged_at: %v", err)
		}
	}
	if cols["resolved_at"] != 0 {
		// resolved_at > 0 implies resolved = 1 — keep them consistent so the
		// timeline helper picks up the resolve event.
		if _, err := s.db.Exec("UPDATE incidents SET resolved = 1, resolved_at = ?, resolution_reason = ? WHERE id = ?",
			cols["resolved_at"], "manual", id); err != nil {
			t.Fatalf("update resolved_at: %v", err)
		}
	}
	return idStr
}
