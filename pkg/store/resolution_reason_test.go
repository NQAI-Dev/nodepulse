package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// ResolveIncident must tag the row with reason="manual" so the public
// timeline and operator audit can distinguish operator action from
// auto-resolution paths.
func TestManualResolveTagsReason(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "manual.db"), "", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	now := time.Now()
	hb := mkHeartbeat("node-m", now)
	hb.Services = []protocol.ServiceStatus{
		{Name: "weird_container", Type: "docker", Active: false, Status: "Exited (1)"},
	}
	s.Ingest(&hb)

	openInc := s.GetActiveIncidents(1)
	if len(openInc) != 1 {
		t.Fatalf("expected 1 open incident, got %d", len(openInc))
	}

	if err := s.ResolveIncident(openInc[0].ID, 1); err != nil {
		t.Fatalf("ResolveIncident: %v", err)
	}

	row := s.db.QueryRow("SELECT resolved, COALESCE(resolution_reason, '') FROM incidents WHERE id = ?", openInc[0].ID)
	var resolved int
	var reason string
	if err := row.Scan(&resolved, &reason); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if resolved != 1 {
		t.Fatalf("expected resolved=1, got %d", resolved)
	}
	if reason != "manual" {
		t.Fatalf("expected resolution_reason=manual, got %q", reason)
	}
}

// Container came back online → auto-resolve with reason "auto:service_recovered".
func TestAutoResolveServiceRecovered(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "recov.db"), "", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	now := time.Now()
	hb := mkHeartbeat("node-r", now)
	hb.Services = []protocol.ServiceStatus{
		{Name: "flap", Type: "docker", Active: false, Status: "Exited (1)"},
	}
	s.Ingest(&hb)

	hb2 := mkHeartbeat("node-r", now.Add(time.Microsecond))
	hb2.Services = []protocol.ServiceStatus{
		{Name: "flap", Type: "docker", Active: true, Status: "Up 1 second"},
	}
	s.Ingest(&hb2)

	if remaining := s.GetActiveIncidents(1); len(remaining) != 0 {
		t.Fatalf("expected 0 open incidents after recovery, got %d", len(remaining))
	}

	var reason string
	if err := s.db.QueryRow("SELECT COALESCE(resolution_reason, '') FROM incidents WHERE title = 'Container Stopped: flap' ORDER BY id DESC LIMIT 1").Scan(&reason); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if reason != "auto:service_recovered" {
		t.Fatalf("expected reason=auto:service_recovered, got %q", reason)
	}
}

// Container dropped from heartbeat entirely → auto-resolve with reason
// "auto:service_absent" so the operator timeline isn't stuck at outage.
func TestAutoResolveServiceAbsent(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "absent.db"), "", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	now := time.Now()
	hb := mkHeartbeat("node-a", now)
	hb.Services = []protocol.ServiceStatus{
		{Name: "ephemeral", Type: "docker", Active: false, Status: "Exited (0)"},
	}
	s.Ingest(&hb)

	// Next heartbeat reports no services at all for that container.
	hb2 := mkHeartbeat("node-a", now.Add(time.Microsecond))
	s.Ingest(&hb2)

	if remaining := s.GetActiveIncidents(1); len(remaining) != 0 {
		t.Fatalf("expected 0 open incidents after service vanished, got %d", len(remaining))
	}

	var reason string
	if err := s.db.QueryRow("SELECT COALESCE(resolution_reason, '') FROM incidents WHERE title = 'Container Stopped: ephemeral' ORDER BY id DESC LIMIT 1").Scan(&reason); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if reason != "auto:service_absent" {
		t.Fatalf("expected reason=auto:service_absent, got %q", reason)
	}
}

// Container kept reporting stopped past abandonedContainerTTL → auto-resolve
// with reason "auto:abandoned_ttl".
func TestAutoResolveAbandonedTTL(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "ttl.db"), "", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	now := time.Now()
	hb := mkHeartbeat("node-t", now)
	hb.Services = []protocol.ServiceStatus{
		{Name: "ghost", Type: "docker", Active: false, Status: "Exited (1)"},
	}
	s.Ingest(&hb)

	openInc := s.GetActiveIncidents(1)
	if len(openInc) != 1 {
		t.Fatalf("expected 1 open incident right after open, got %d", len(openInc))
	}

	// Rewind started_at past abandonedContainerTTL.
	if _, err := s.db.Exec("UPDATE incidents SET started_at = ? WHERE id = ?",
		now.Add(-2*abandonedContainerTTL).Unix(), openInc[0].ID); err != nil {
		t.Fatalf("rewind started_at: %v", err)
	}

	hb2 := mkHeartbeat("node-t", now.Add(time.Microsecond))
	hb2.Services = hb.Services
	s.Ingest(&hb2)

	if remaining := s.GetActiveIncidents(1); len(remaining) != 0 {
		t.Fatalf("abandoned container incident should have auto-resolved, still open: %+v", remaining)
	}

	var reason string
	if err := s.db.QueryRow("SELECT COALESCE(resolution_reason, '') FROM incidents WHERE id = ?", openInc[0].ID).Scan(&reason); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if reason != "auto:abandoned_ttl" {
		t.Fatalf("expected reason=auto:abandoned_ttl, got %q", reason)
	}
}

// PublicIncidentHistory exposes resolution_reason so the status timeline UI
// can render "auto-resolved" badges without joining against heartbeat data.
func TestPublicIncidentHistoryExposesReason(t *testing.T) {
	dir := t.TempDir()
	s, err := NewPersistentStore(filepath.Join(dir, "pub.db"), "", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	now := time.Now()
	hb := mkHeartbeat("node-p", now)
	hb.Services = []protocol.ServiceStatus{
		{Name: "back", Type: "docker", Active: false, Status: "Exited (1)"},
	}
	s.Ingest(&hb)

	hb2 := mkHeartbeat("node-p", now.Add(time.Microsecond))
	hb2.Services = []protocol.ServiceStatus{
		{Name: "back", Type: "docker", Active: true, Status: "Up 1 second"},
	}
	s.Ingest(&hb2)

	items, err := s.GetPublicIncidentHistory(0, 50)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 history row, got %d", len(items))
	}
	if !items[0].Resolved {
		t.Fatalf("expected resolved=true, got false")
	}
	if items[0].ResolutionReason != "auto:service_recovered" {
		t.Fatalf("expected reason=auto:service_recovered in public history, got %q", items[0].ResolutionReason)
	}
}
