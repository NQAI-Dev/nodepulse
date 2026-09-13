package store

import (
	"database/sql"
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

	// Master admin token is randomly generated on first startup (see
	// ensureMasterToken in pkg/store/sqlite.go). Read it via a separate
	// connection because the store doesn't expose the cached value
	// through its public API — production code must use IsMasterToken or
	// ValidateToken, never read the literal directly.
	rawDB, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatalf("open raw DB: %v", err)
	}
	defer rawDB.Close()
	var masterTok string
	if err := rawDB.QueryRow("SELECT token FROM api_tokens WHERE name = 'master'").Scan(&masterTok); err != nil {
		t.Fatalf("read master token: %v", err)
	}
	if masterTok == "" || masterTok == "np_live_master_secret" {
		t.Fatalf("master token must be a fresh random value, got %q", masterTok)
	}
	if !s.ValidateToken(masterTok) {
		t.Fatalf("Stored master token must validate")
	}
	// Regression guard: the legacy literal must NOT validate after the
	// master-token-rotation fix lands.
	if s.ValidateToken("np_live_master_secret") {
		t.Fatalf("Legacy literal must NOT validate after rotation")
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

	incidents := s.GetActiveIncidents(1)
	if len(incidents) == 0 {
		t.Fatalf("Expected incident triggered for high memory")
	}

	err = s.ResolveIncident(incidents[0].ID, 1)
	if err != nil {
		t.Fatalf("Failed to resolve incident: %v", err)
	}

	incidentsAfter := s.GetActiveIncidents(1)
	if len(incidentsAfter) != 0 {
		t.Fatalf("Expected 0 active incidents after resolve, got %d", len(incidentsAfter))
	}
}
