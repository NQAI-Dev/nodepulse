package store

import (
	"os"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func TestPersistentStore(t *testing.T) {
	dbFile := "test_pulse.db"
	defer os.Remove(dbFile)

	s, err := NewPersistentStore(dbFile, "", 0)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	if !s.ValidateToken("np_live_master_secret") {
		t.Fatalf("Default token must be valid")
	}

	hb := &protocol.Heartbeat{
		NodeID:    "test-node",
		Timestamp: time.Now().Unix(),
		CPU: protocol.CPUStats{
			Load1: 0.5,
			Cores: 2,
		},
		Memory: protocol.MemoryStats{
			UsedPercent: 95.0, // Should trigger incident
		},
	}
	s.Ingest(hb)

	incidents := s.GetActiveIncidents()
	if len(incidents) == 0 {
		t.Fatalf("Expected incident triggered for high memory")
	}

	err = s.ResolveIncident(incidents[0].ID)
	if err != nil {
		t.Fatalf("Failed to resolve incident: %v", err)
	}

	incidentsAfter := s.GetActiveIncidents()
	if len(incidentsAfter) != 0 {
		t.Fatalf("Expected 0 active incidents after resolve, got %d", len(incidentsAfter))
	}
}
