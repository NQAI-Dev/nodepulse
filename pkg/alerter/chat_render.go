package alerter

import (
	"encoding/json"
	"fmt"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// renderSlackBlocks builds a Slack Block Kit message that mirrors the
// layout of the existing Telegram alert (icon + header + key/value
// fields + timestamp) without copying the inline-keyboard buttons —
// Slack does not have a callback-query equivalent for incoming
// webhooks, so the only thing an operator can do via chat is read.
//
// Empty Detail becomes "(no detail)" so the field always renders
// something readable; a non-empty Detail is passed through verbatim
// because the metric-alert path stuffs structured data into it
// (metric=X value=Y threshold=Z).
func renderSlackBlocks(inc protocol.Incident) ([]byte, error) {
	detail := inc.Detail
	if detail == "" {
		detail = "(no detail)"
	}
	icon := "⚠️"
	if inc.Severity == "critical" {
		icon = "🚨"
	}
	body := map[string]interface{}{
		"text": fmt.Sprintf("%s [NodePulse Incident] %s — %s", icon, inc.NodeID, inc.Title),
		"blocks": []interface{}{
			map[string]interface{}{
				"type": "header",
				"text": map[string]interface{}{
					"type": "plain_text",
					"text": fmt.Sprintf("%s NodePulse Incident — %s", icon, inc.Severity),
				},
			},
			map[string]interface{}{
				"type": "section",
				"fields": []interface{}{
					map[string]interface{}{"type": "mrkdwn", "text": fmt.Sprintf("*Node:*\n`%s`", inc.NodeID)},
					map[string]interface{}{"type": "mrkdwn", "text": fmt.Sprintf("*Severity:*\n%s", inc.Severity)},
					map[string]interface{}{"type": "mrkdwn", "text": fmt.Sprintf("*Issue:*\n%s", inc.Title)},
					map[string]interface{}{"type": "mrkdwn", "text": fmt.Sprintf("*Detail:*\n%s", detail)},
				},
			},
			map[string]interface{}{
				"type": "context",
				"elements": []interface{}{
					map[string]interface{}{"type": "mrkdwn", "text": fmt.Sprintf("Started at %d (unix seconds)", inc.StartedAt)},
				},
			},
		},
	}
	return json.Marshal(body)
}

// renderSlackBlocksResolved is the green-tick variant for auto-resolve;
// reuses the same block layout but flips the icon and leading text so
// an on-call engineer skimming #alerts can tell at a glance which
// messages are still open vs. resolved.
func renderSlackBlocksResolved(inc protocol.Incident) ([]byte, error) {
	body := map[string]interface{}{
		"text": fmt.Sprintf("✅ [NodePulse Resolved] %s — %s", inc.NodeID, inc.Title),
		"blocks": []interface{}{
			map[string]interface{}{
				"type": "section",
				"text": map[string]interface{}{
					"type": "mrkdwn",
					"text": fmt.Sprintf("✅ *NodePulse Incident Resolved*\n*Node:* `%s`\n*Severity:* %s\n*Issue:* %s", inc.NodeID, inc.Severity, inc.Title),
				},
			},
		},
	}
	return json.Marshal(body)
}

func renderSlackTestBlocks(nodeID string) ([]byte, error) {
	body := map[string]interface{}{
		"text": "[NodePulse Test Alert] Slack channel wiring ok",
		"blocks": []interface{}{
			map[string]interface{}{
				"type": "section",
				"text": map[string]interface{}{
					"type": "mrkdwn",
					"text": fmt.Sprintf("🧪 *NodePulse Test Alert*\nSlack incoming webhook for node `%s` is healthy.", nodeID),
				},
			},
		},
	}
	return json.Marshal(body)
}

// renderDiscordEmbed mirrors the Slack block layout using Discord's
// embed schema: a single embed object with a title, three fields, and
// a footer carrying the timestamp. embeds cap at 4096 chars on the
// description field, so very large Detail payloads get truncated
// rather than failing the whole POST — Discord rejects the entire
// payload (400) when a field exceeds the limit, and we don't want
// legitimate alerts dropped because the agent attached a verbose
// process list.
func renderDiscordEmbed(inc protocol.Incident) ([]byte, error) {
	detail := inc.Detail
	if len(detail) > 900 {
		detail = detail[:900] + "\n…(truncated)"
	}
	if detail == "" {
		detail = "(no detail)"
	}
	color := 0xd29922 // amber
	if inc.Severity == "critical" {
		color = 0xda3633 // red
	} else if inc.Severity == "warning" {
		color = 0xd29922
	} else {
		color = 0x388bfd
	}
	body := map[string]interface{}{
		"username": "NodePulse",
		"embeds": []interface{}{
			map[string]interface{}{
				"title":       fmt.Sprintf("[%s] %s — %s", inc.Severity, inc.NodeID, inc.Title),
				"description": fmt.Sprintf("A new NodePulse incident has opened.\n\n**Severity:** %s\n**Detail:** %s", inc.Severity, detail),
				"color":       color,
				"footer": map[string]interface{}{
					"text": fmt.Sprintf("Incident #%s • started %d", inc.ID, inc.StartedAt),
				},
			},
		},
	}
	return json.Marshal(body)
}

func renderDiscordEmbedResolved(inc protocol.Incident) ([]byte, error) {
	body := map[string]interface{}{
		"username": "NodePulse",
		"embeds": []interface{}{
			map[string]interface{}{
				"title":       fmt.Sprintf("[RESOLVED] %s — %s", inc.NodeID, inc.Title),
				"description": fmt.Sprintf("✅ Incident on `%s` auto-resolved (severity: %s).", inc.NodeID, inc.Severity),
				"color":       0x2ea043,
			},
		},
	}
	return json.Marshal(body)
}

func renderDiscordTestEmbed(nodeID string) ([]byte, error) {
	body := map[string]interface{}{
		"username": "NodePulse",
		"embeds": []interface{}{
			map[string]interface{}{
				"title":       "[Test Alert] Discord webhook healthy",
				"description": fmt.Sprintf("Discord incoming webhook for node `%s` is healthy 🧪", nodeID),
				"color":       0x2ea043,
			},
		},
	}
	return json.Marshal(body)
}
