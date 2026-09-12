package store

import (
	"path/filepath"
	"testing"
)

func TestIncidentDispatcherRouting(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	s, err := NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("failed to init store: %v", err)
	}

	uid, _, err := s.Register("alice", "secret123")
	if err != nil {
		t.Fatalf("failed to register: %v", err)
	}

	s.BindNode("node-alice", uid)

	// Update settings with a webhook url
	err = s.UpdateSettings(uid, "123456", "https://example.com/webhook", "shh", true, true)
	if err != nil {
		t.Fatalf("failed to update settings: %v", err)
	}

	cfg, err := s.GetSettings(uid)
	if err != nil {
		t.Fatalf("failed to get settings: %v", err)
	}
	if cfg.TelegramChatID != "123456" || cfg.WebhookURL != "https://example.com/webhook" {
		t.Fatalf("unexpected settings: %+v", cfg)
	}

	// Create incident
	s.CreateIncident("node-alice", "critical", "Docker Down", "Test detail")

	incidents := s.GetActiveIncidents(uid)
	if len(incidents) != 1 {
		t.Fatalf("expected 1 incident, got %d", len(incidents))
	}

	if incidents[0].NodeID != "node-alice" || incidents[0].Title != "Docker Down" {
		t.Errorf("unexpected incident data: %+v", incidents[0])
	}
}
