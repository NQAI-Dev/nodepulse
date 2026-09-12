package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func TestFlushWebhookDeliveries_PersistsRows(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("init store: %v", err)
	}

	now := time.Now().Unix()
	rows := []protocol.WebhookDelivery{
		{UserID: 7, IncidentID: "42", Event: "incident.created", URL: "https://hook.example/x", Attempts: 1, Status: 200, OK: true, TotalLatencyMs: 12, Timestamp: now},
		{UserID: 7, IncidentID: "43", Event: "incident.created", URL: "https://hook.example/x", Attempts: 4, Status: 502, OK: false, Error: "webhook returned status 502", TotalLatencyMs: 4210, Timestamp: now + 1},
		{UserID: 9, Event: "incident.resolved", URL: "https://other.example/y", Attempts: 1, Status: 200, OK: true, Timestamp: now + 2},
	}
	written, err := s.FlushWebhookDeliveries(rows)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if written != len(rows) {
		t.Fatalf("expected %d written, got %d", len(rows), written)
	}

	userRows, err := s.WebhookDeliveries(7, 10)
	if err != nil {
		t.Fatalf("query user 7: %v", err)
	}
	if len(userRows) != 2 {
		t.Fatalf("expected 2 rows for user 7, got %d", len(userRows))
	}
	// Newest first: row index 1 should come back first because its ts is later.
	if userRows[0].IncidentID != "43" {
		t.Fatalf("expected newest-first ordering, got %+v", userRows[0])
	}
	if userRows[0].OK {
		t.Fatal("the failed delivery should be marked ok=false")
	}
	if userRows[0].Error == "" {
		t.Fatal("expected error message preserved")
	}
	if userRows[1].Status != 200 || !userRows[1].OK {
		t.Fatalf("expected 2nd row to be the success: %+v", userRows[1])
	}

	stats, err := s.WebhookDeliveryStats(7, 3600)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if total, _ := stats["total"].(int64); total != 2 {
		t.Fatalf("expected total=2, got %v", stats["total"])
	}
	if ok, _ := stats["ok"].(int64); ok != 1 {
		t.Fatalf("expected ok=1, got %v", stats["ok"])
	}
}

func TestFlushWebhookDeliveries_EmptyNoOp(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("init store: %v", err)
	}
	n, err := s.FlushWebhookDeliveries(nil)
	if err != nil || n != 0 {
		t.Fatalf("empty flush should be a no-op: n=%d err=%v", n, err)
	}
}

func TestWebhookDeliveryStats_RespectsWindow(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("init store: %v", err)
	}

	// Insert one old + one recent row directly so we can test the cutoff.
	old := time.Now().Add(-48 * time.Hour).Unix()
	recent := time.Now().Unix()
	rows := []protocol.WebhookDelivery{
		{UserID: 1, Event: "incident.created", URL: "u", Status: 200, OK: true, Timestamp: old},
		{UserID: 1, Event: "incident.created", URL: "u", Status: 502, OK: false, Error: "boom", Timestamp: recent},
	}
	if _, err := s.FlushWebhookDeliveries(rows); err != nil {
		t.Fatalf("flush: %v", err)
	}
	stats, err := s.WebhookDeliveryStats(1, 3600)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if total, _ := stats["total"].(int64); total != 1 {
		t.Fatalf("old row should fall outside 1h window, got total=%v", stats["total"])
	}
}

func TestWebhookJanitor_PrunesOldRows(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("init store: %v", err)
	}

	expired := time.Now().Add(-(webhookDeliveryRetentionDays + 5) * 24 * time.Hour).Unix()
	recent := time.Now().Unix()
	rows := []protocol.WebhookDelivery{
		{UserID: 1, Event: "incident.created", URL: "u", Status: 200, OK: true, Timestamp: expired},
		{UserID: 1, Event: "incident.created", URL: "u", Status: 200, OK: true, Timestamp: expired - 1},
		{UserID: 1, Event: "incident.created", URL: "u", Status: 200, OK: true, Timestamp: recent},
	}
	if _, err := s.FlushWebhookDeliveries(rows); err != nil {
		t.Fatalf("flush: %v", err)
	}
	before := WebhookDeliveriesPrunedTotal
	s.runJanitorPass()
	after := WebhookDeliveriesPrunedTotal
	deleted := after - before
	if deleted < 2 {
		t.Fatalf("expected at least 2 expired rows pruned, got %d", deleted)
	}

	stats, err := s.WebhookDeliveryStats(1, int64(webhookDeliveryRetentionDays+5)*24*3600)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if total, _ := stats["total"].(int64); total != 1 {
		t.Fatalf("only the recent row should survive, got total=%v", stats["total"])
	}
}
