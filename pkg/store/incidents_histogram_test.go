package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestHistogram_EmptyFleetReturnsContinuousTimeline(t *testing.T) {
	s, err := NewPersistentStore(filepath.Join(t.TempDir(), "h.db"), "", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got := s.PublicIncidentHistogram(7)
	if got.Days != 7 || len(got.Buckets) != 7 {
		t.Fatalf("want 7 buckets, got %d", len(got.Buckets))
	}
	if got.UpdatedAt <= 0 {
		t.Fatal("UpdatedAt must be set")
	}
	for i, b := range got.Buckets {
		if len(b.TotalBySeverity) != 0 || len(b.OpenBySeverity) != 0 {
			t.Fatalf("empty bucket %d should have empty maps: %+v", i, b)
		}
	}
}

func TestHistogram_DaysOutOfRangeClamped(t *testing.T) {
	s, _ := NewPersistentStore(filepath.Join(t.TempDir(), "h.db"), "", 0)
	for _, in := range []int{-1, 0, 1000, 200} {
		got := s.PublicIncidentHistogram(in)
		if got.Days != 30 {
			t.Fatalf("days=%d should clamp to 30, got %d", in, got.Days)
		}
	}
}

func TestHistogram_BucketsIncidentsByDay(t *testing.T) {
	s, _ := NewPersistentStore(filepath.Join(t.TempDir(), "h.db"), "", 0)
	now := time.Now().UTC()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC).Unix()
	yesterday := todayStart - 86400
	threeDaysAgo := todayStart - 3*86400

	if _, err := s.db.Exec(
		`INSERT INTO incidents (node_id, severity, title, detail, started_at, resolved) VALUES (?, ?, ?, ?, ?, 0)`,
		"n1", "critical", "today-c", "", todayStart,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO incidents (node_id, severity, title, detail, started_at, resolved) VALUES (?, ?, ?, ?, ?, 1)`,
		"n2", "warning", "yesterday-w", "", yesterday,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO incidents (node_id, severity, title, detail, started_at, resolved) VALUES (?, ?, ?, ?, ?, 0)`,
		"n3", "warning", "old-w", "", threeDaysAgo,
	); err != nil {
		t.Fatal(err)
	}

	got := s.PublicIncidentHistogram(7)
	if len(got.Buckets) != 7 {
		t.Fatalf("want 7 buckets, got %d", len(got.Buckets))
	}
	// newest bucket is index 6 (today)
	today := got.Buckets[6]
	if today.TotalBySeverity["critical"] != 1 || today.OpenBySeverity["critical"] != 1 {
		t.Fatalf("today bucket wrong: %+v", today)
	}
	// yesterday at index 5
	yest := got.Buckets[5]
	if yest.TotalBySeverity["warning"] != 1 || yest.OpenBySeverity["warning"] != 0 {
		t.Fatalf("yesterday bucket wrong: %+v", yest)
	}
	// 3 days ago at index 3
	old := got.Buckets[3]
	if old.TotalBySeverity["warning"] != 1 || old.OpenBySeverity["warning"] != 1 {
		t.Fatalf("3-days-ago bucket wrong: %+v", old)
	}
}
