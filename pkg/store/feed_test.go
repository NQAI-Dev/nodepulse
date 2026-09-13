package store

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func TestRenderAtomFeed(t *testing.T) {
	now := time.Now().Unix()
	incidents := []protocol.PublicIncidentHistory{
		{
			ID:               "101",
			NodeID:           "node-alpha",
			Severity:         "critical",
			Title:            "High CPU load detected (>95%)",
			StartedAt:        now - 3600,
			Resolved:         true,
			ResolvedAt:       now - 1800,
			ResolutionReason: "auto:metric_recovered",
		},
		{
			ID:        "102",
			NodeID:    "node-beta",
			Severity:  "warning",
			Title:     "Disk space below 10%",
			StartedAt: now - 600,
			Resolved:  false,
		},
	}

	data, err := RenderAtomFeed("https://pulse.example.com", "Example Status Feed", incidents)
	if err != nil {
		t.Fatalf("RenderAtomFeed failed: %v", err)
	}

	raw := string(data)
	if !strings.HasPrefix(raw, xml.Header) {
		t.Errorf("expected XML header, got: %s", raw[:min(len(raw), 50)])
	}
	if !strings.Contains(raw, "<feed xmlns=\"http://www.w3.org/2005/Atom\">") {
		t.Errorf("missing Atom xmlns")
	}
	if !strings.Contains(raw, "<title>Example Status Feed</title>") {
		t.Errorf("feed title mismatch")
	}
	if !strings.Contains(raw, "urn:nodepulse:incident:101") {
		t.Errorf("missing entry 101 ID")
	}
	if !strings.Contains(raw, "urn:nodepulse:incident:102") {
		t.Errorf("missing entry 102 ID")
	}
	if !strings.Contains(raw, "RESOLVED (auto:metric_recovered)") {
		t.Errorf("missing resolution reason in entry 101")
	}

	// Verify valid XML structure parseable
	var parsed AtomFeed
	if err := xml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal generated XML: %v", err)
	}
	if len(parsed.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(parsed.Entries))
	}
	if parsed.Entries[0].Title != "[critical] High CPU load detected (>95%)" {
		t.Errorf("unexpected entry 0 title: %s", parsed.Entries[0].Title)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
