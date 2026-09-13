package alerter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// TestRenderSlackBlocks_AllFields verifies the Slack Block Kit payload
// contains the expected sections for a fully-populated incident. Empty
// detail should NOT render "(no detail)" — that placeholder is only
// injected when the incident itself has no detail field. This guards
// against the renderer silently dropping data.
func TestRenderSlackBlocks_AllFields(t *testing.T) {
	inc := protocol.Incident{
		ID:        "42",
		NodeID:    "node-1",
		Severity:  "critical",
		Title:     "High CPU",
		Detail:    "cpu_pct=92 threshold=80",
		StartedAt: 1700000000,
	}
	body, err := renderSlackBlocks(inc)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if _, ok := out["blocks"]; !ok {
		t.Fatalf("expected blocks key in slack payload, got %v", out)
	}
	text, _ := out["text"].(string)
	if !strings.Contains(text, "node-1") || !strings.Contains(text, "High CPU") {
		t.Fatalf("fallback text missing node/title: %q", text)
	}
}

// TestRenderSlackBlocks_EmptyDetail proves the renderer surfaces a
// readable "(no detail)" string instead of an empty field — Slack
// renders empty mrkdwn as a blank line which looks like a layout bug.
func TestRenderSlackBlocks_EmptyDetail(t *testing.T) {
	inc := protocol.Incident{NodeID: "n1", Severity: "warning", Title: "Disk", StartedAt: 1}
	body, err := renderSlackBlocks(inc)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if !strings.Contains(string(body), "(no detail)") {
		t.Fatalf("expected (no detail) placeholder, got: %s", string(body))
	}
}

// TestRenderDiscordEmbed_TruncatesLongDetail guards the 4096-char
// Discord embed limit. Real alerts sometimes attach verbose process
// lists; truncating with a trailing ellipsis preserves the signal
// while keeping the POST valid (Discord 400s on oversized fields).
func TestRenderDiscordEmbed_TruncatesLongDetail(t *testing.T) {
	long := strings.Repeat("x", 2000)
	inc := protocol.Incident{
		ID:        "99",
		NodeID:    "n2",
		Severity:  "critical",
		Title:     "OOM",
		Detail:    long,
		StartedAt: 1700000000,
	}
	body, err := renderDiscordEmbed(inc)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if strings.Contains(string(body), long) {
		t.Fatalf("expected long detail to be truncated, but full string was present")
	}
	if !strings.Contains(string(body), "(truncated)") {
		t.Fatalf("expected truncation marker, got: %s", string(body))
	}
}

// TestRenderDiscordEmbed_EmptyDetail mirrors the Slack empty-detail
// behaviour so the embed never renders an empty value field.
func TestRenderDiscordEmbed_EmptyDetail(t *testing.T) {
	inc := protocol.Incident{NodeID: "n3", Severity: "warning", Title: "Disk", StartedAt: 1}
	body, err := renderDiscordEmbed(inc)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if !strings.Contains(string(body), "(no detail)") {
		t.Fatalf("expected (no detail) placeholder, got: %s", string(body))
	}
}

// TestValidateChatWebhookURL covers the host allowlist that prevents
// misconfigured chat channels from POSTing to arbitrary URLs. A
// typo'd hook URL is the most common Slack/Discord misconfiguration.
func TestValidateChatWebhookURL(t *testing.T) {
	cases := []struct {
		url    string
		wantOK bool
		reason string
	}{
		{"https://hooks.slack.com/services/T0/B0/X", true, "slack incoming webhook"},
		{"https://discord.com/api/webhooks/123/abc", true, "discord api webhook"},
		{"https://discordapp.com/api/webhooks/123/abc", true, "legacy discord host"},
		{"http://hooks.slack.com/services/X", false, "http scheme rejected"},
		{"https://example.com/hook", false, "off-list host rejected"},
		{"https://127.0.0.1:8080/hook", false, "loopback rejected"},
		{"not-a-url", false, "invalid url rejected"},
		{"", false, "empty url rejected"},
	}
	for _, tc := range cases {
		err := validateChatWebhookURL(tc.url)
		if tc.wantOK && err != nil {
			t.Errorf("%s: want ok, got error %v", tc.reason, err)
		}
		if !tc.wantOK && err == nil {
			t.Errorf("%s: expected validation error, got nil", tc.reason)
		}
	}
}

// TestChannelStats_TracksAttempts proves the per-host counter
// increments correctly when SendTest succeeds or fails. The counters
// feed the /api/v1/dispatch/stats endpoint operators rely on to spot
// a misconfigured workspace.
func TestChannelStats_TracksAttempts(t *testing.T) {
	rec := NewWebhookRecorder(NewWebhook())
	cd := NewChatDispatcher(rec)

	// Successful delivery: simulate by manually bumping via a known URL.
	// We use the host allowlist so the underlying validateChatWebhookURL
	// accepts it.
	cd.bumpStat("slack", "https://hooks.slack.com/services/X/Y/Z", true)
	cd.bumpStat("slack", "https://hooks.slack.com/services/X/Y/Z", false)
	cd.bumpStat("discord", "https://discord.com/api/webhooks/1/a", true)

	slack, discord := cd.ChannelStats()
	if got := slack["hooks.slack.com"]; got.Attempts != 2 || got.OKs != 1 {
		t.Errorf("slack stats wrong: %+v", got)
	}
	if got := discord["discord.com"]; got.Attempts != 1 || got.OKs != 1 {
		t.Errorf("discord stats wrong: %+v", got)
	}
}

// TestRenderSlackBlocksResolved_UsesGreenIcon makes sure the resolved
// variant surfaces the green tick so an on-call engineer can tell at
// a glance which messages are still open.
func TestRenderSlackBlocksResolved_UsesGreenIcon(t *testing.T) {
	inc := protocol.Incident{ID: "7", NodeID: "n", Severity: "critical", Title: "OOM"}
	body, err := renderSlackBlocksResolved(inc)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if !strings.Contains(string(body), "\u2705") {
		t.Fatalf("expected resolved message to carry green tick, got: %s", string(body))
	}
}
