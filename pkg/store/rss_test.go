package store

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func TestRenderRSSFeed(t *testing.T) {
	now := time.Now().Unix()
	incidents := []protocol.PublicIncidentHistory{
		{
			ID:               "inc-101",
			NodeID:           "test-node",
			Title:            "Memory pressure high",
			Severity:         "warning",
			StartedAt:        now - 300,
			Resolved:         true,
			ResolvedAt:       now - 60,
			ResolutionReason: "autoheal memory clean",
		},
		{
			ID:        "inc-102",
			NodeID:    "prod-node",
			Title:     "Host unreachable",
			Severity:  "critical",
			StartedAt: now - 100,
			Resolved:  false,
		},
	}

	data, err := RenderRSSFeed("https://pulse.example.com", "Example RSS Feed", incidents)
	if err != nil {
		t.Fatalf("RenderRSSFeed failed: %v", err)
	}

	raw := string(data)
	if !strings.Contains(raw, "<rss version=\"2.0\">") {
		t.Errorf("missing RSS 2.0 root tag: %s", raw)
	}
	if !strings.Contains(raw, "<title>Example RSS Feed</title>") {
		t.Errorf("missing feed title")
	}
	if !strings.Contains(raw, "Memory pressure high") {
		t.Errorf("missing incident title")
	}
	if !strings.Contains(raw, "urn:nodepulse:incident:inc-101") {
		t.Errorf("missing GUID")
	}
	if !strings.Contains(raw, "RESOLVED (autoheal memory clean)") {
		t.Errorf("missing resolution reason in description")
	}

	var parsed RSSFeed
	if err := xml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal generated RSS failed: %v", err)
	}
	if len(parsed.Channel.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(parsed.Channel.Items))
	}
	if parsed.Channel.Items[0].Title != "[warning] Memory pressure high" {
		t.Errorf("unexpected item title: %s", parsed.Channel.Items[0].Title)
	}
}
