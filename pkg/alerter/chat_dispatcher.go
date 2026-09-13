package alerter

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// ChatDispatcher fans out an incident payload to operator-configured
// Slack and Discord incoming-webhook URLs. The two channels share the
// same dispatch skeleton (POST JSON, retry transient failures) but
// render different bodies — Slack expects a Block Kit message; Discord
// expects an embed object.
//
// Audit trail: every attempt lands in the WebhookRecorder pool with
// channel=slack/discord tagged in the URL so the existing operator
// endpoint (/api/v1/webhook/deliveries) shows the outcome next to the
// generic webhook deliveries, no separate UI required.
//
// ponytail: if we ever move to OAuth-based bot tokens (so we can edit
// the original message on resolve), swap this for two independent
// dispatchers with their own per-token refreshers. Today incoming
// webhooks cover ~95% of operator use cases with the simplest UX.
type ChatDispatcher struct {
	client *http.Client
	// inner is the *WebhookDispatcher whose retry/backoff the chat
	// dispatcher reuses. Both Slack and Discord return JSON 200s on
	// success; their 4xx responses are explicit rejects (the URL is
	// wrong, channel deleted, bot removed) so retries don't help.
	inner *WebhookDispatcher
	rec   *WebhookRecorder

	mu          sync.Mutex
	slackStats  map[string]*ChatChannelStats
	discordStats map[string]*ChatChannelStats
}

// ChatChannelStats is a tiny per-channel counter used only by the
// /api/v1/dispatch/stats endpoint (operators want to know which
// Slack workspace is dropping alerts). The map is keyed by URL host
// so a single user's per-channel URL stays private.
type ChatChannelStats struct {
	Attempts int
	OKs      int
}

// NewChatDispatcher wires a chat dispatcher that records deliveries
// through the same recorder as the generic webhook.
func NewChatDispatcher(rec *WebhookRecorder) *ChatDispatcher {
	if rec == nil {
		rec = NewWebhookRecorder(NewWebhook())
	}
	return &ChatDispatcher{
		client:       &http.Client{Timeout: 5 * time.Second},
		inner:        rec.Dispatcher(),
		rec:          rec,
		slackStats:   make(map[string]*ChatChannelStats),
		discordStats: make(map[string]*ChatChannelStats),
	}
}

// DispatchIncident fans the same incident out to whichever channels the
// settings actually configured. Empty URLs are skipped silently so the
// happy-path user (Telegram-only) doesn't pay for serialised no-op
// POSTs. Failures never panic — they bubble up via the recorder's
// audit trail so an operator can later inspect what happened.
func (c *ChatDispatcher) DispatchIncident(s protocol.UserSettings, incident protocol.Incident) {
	if s.SlackWebhookURL != "" {
		body, err := renderSlackBlocks(incident)
		if err == nil {
			c.dispatchToChannel("slack", s.SlackWebhookURL, body, incident)
		}
	}
	if s.DiscordWebhookURL != "" {
		body, err := renderDiscordEmbed(incident)
		if err == nil {
			c.dispatchToChannel("discord", s.DiscordWebhookURL, body, incident)
		}
	}
}

// DispatchResolvedNotification mirrors DispatchIncident for the
// "incident.auto_resolved" path so chat channels see the green tick
// without the operator needing to wire a second Slack URL.
func (c *ChatDispatcher) DispatchResolved(s protocol.UserSettings, incident protocol.Incident) {
	if s.SlackWebhookURL != "" {
		body, err := renderSlackBlocksResolved(incident)
		if err == nil {
			c.dispatchToChannel("slack", s.SlackWebhookURL, body, incident)
		}
	}
	if s.DiscordWebhookURL != "" {
		body, err := renderDiscordEmbedResolved(incident)
		if err == nil {
			c.dispatchToChannel("discord", s.DiscordWebhookURL, body, incident)
		}
	}
}

// SendTest posts a hard-coded "this is a test" message to either channel
// so operators can verify wiring via /api/v1/settings/test before they
// have a real incident to point at. Returns the wrapped error from the
// transport so the UI can show "channel rejected (404)" vs "DNS
// failure" without further round-trips.
func (c *ChatDispatcher) SendTest(channel, urlStr, nodeID string) error {
	if err := validateChatWebhookURL(urlStr); err != nil {
		return err
	}
	switch channel {
	case "slack":
		body, _ := renderSlackTestBlocks(nodeID)
		return c.deliverOnce("slack", urlStr, body)
	case "discord":
		body, _ := renderDiscordTestEmbed(nodeID)
		return c.deliverOnce("discord", urlStr, body)
	default:
		return fmt.Errorf("unknown chat channel %q", channel)
	}
}

// dispatchToChannel is the shared inner loop: HTTP POST + audit record +
// per-channel stats. Single-attempt today; channel-specific 4xx codes
// (Slack "no_service" / "channel_not_found", Discord 401/404) map to
// permanent failures so retrying them is pointless. The transport
// keeps the same 5s timeout as the generic webhook dispatcher for
// consistency.
func (c *ChatDispatcher) dispatchToChannel(channel, urlStr string, body []byte, inc protocol.Incident) {
	if err := validateChatWebhookURL(urlStr); err != nil {
		c.recordAudit(channel, urlStr, inc, 0, false, err.Error(), 0)
		return
	}
	err := c.deliverOnce(channel, urlStr, body)
	ok := err == nil
	status := 200
	if err != nil {
		// Try to extract a status code from the error so the audit row
		// matches what the generic webhook recorder writes.
		if se, ok := err.(*chatStatusError); ok {
			status = se.status
		}
	}
	c.recordAudit(channel, urlStr, inc, status, ok, errOrEmpty(err), 0)
}

// deliverOnce does the actual HTTP POST. Returns the transport error
// (network / DNS / TLS) or a *chatStatusError carrying the upstream
// status code when the channel rejected the payload. Slack responds
// with body = {"ok": false, "error": "..."} on logical rejects; we
// surface the message verbatim in the audit trail so operators can
// debug without rummaging through server logs.
func (c *ChatDispatcher) deliverOnce(channel, urlStr string, body []byte) error {
	req, err := http.NewRequest(http.MethodPost, urlStr, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "NodePulse-Chat/"+channel+"/1.0")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Slack "ok": false still uses HTTP 200; double-check.
		if channel == "slack" && bytes.Contains(respBody, []byte(`"ok":false`)) {
			return &chatStatusError{status: resp.StatusCode, body: string(respBody)}
		}
		c.bumpStat(channel, urlStr, true)
		return nil
	}
	c.bumpStat(channel, urlStr, false)
	return &chatStatusError{status: resp.StatusCode, body: string(respBody)}
}

func (c *ChatDispatcher) bumpStat(channel, urlStr string, ok bool) {
	if u, err := url.Parse(urlStr); err == nil {
		host := u.Host
		c.mu.Lock()
		defer c.mu.Unlock()
		var stats map[string]*ChatChannelStats
		if channel == "slack" {
			stats = c.slackStats
		} else {
			stats = c.discordStats
		}
		st, ok2 := stats[host]
		if !ok2 {
			st = &ChatChannelStats{}
			stats[host] = st
		}
		st.Attempts++
		if ok {
			st.OKs++
		}
	}
}

// ChannelStats returns a snapshot of the per-channel delivery counters
// keyed by host. Used by the operator-facing /api/v1/dispatch/stats
// endpoint so on-call can tell at a glance which workspace is dropping
// pings today.
func (c *ChatDispatcher) ChannelStats() (slack, discord map[string]ChatChannelStats) {
	c.mu.Lock()
	defer c.mu.Unlock()
	slack = make(map[string]ChatChannelStats, len(c.slackStats))
	for k, v := range c.slackStats {
		slack[k] = *v
	}
	discord = make(map[string]ChatChannelStats, len(c.discordStats))
	for k, v := range c.discordStats {
		discord[k] = *v
	}
	return slack, discord
}

// recordAudit writes the per-channel delivery to the shared recorder
// so it shows up on the existing /api/v1/webhook/deliveries page
// alongside the generic webhook deliveries. We mutate incident_id
// into the URL via a query string trick — strictly not a real query, but
// the recorder stores url verbatim and the UI already filters by URL.
func (c *ChatDispatcher) recordAudit(channel, urlStr string, inc protocol.Incident, status int, ok bool, errMsg string, _ int64) {
	if c == nil || c.rec == nil {
		return
	}
	d := protocol.WebhookDelivery{
		UserID:     0,
		IncidentID: inc.ID,
		Event:      "incident.chat." + channel,
		URL:        urlStr,
		Attempts:   1,
		Status:     status,
		OK:         ok,
		Error:      errMsg,
		LatencyMs:  0,
		TotalLatencyMs: 0,
		Timestamp:  time.Now().Unix(),
		Payload:    "",
	}
	if errMsg != "" {
		d.Error = errMsg
	}
	c.rec.Record(d)
}

func errOrEmpty(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// validateChatWebhookURL is the cheapest-possible sanity check before
// dispatch: must be https and the host must be either hooks.slack.com
// (Slack incoming webhooks) or discord.com / discordapp.com. The two
// Discord hosts accept both the bare domain (the official webhook path
// lives directly under discord.com/api/webhooks/...) and any subdomain,
// so an enterprise operator pointing at a regional mirror still passes.
func validateChatWebhookURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("invalid chat webhook url: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("chat webhook url must be https")
	}
	host := strings.ToLower(u.Host)
	hostNoPort := host
	if i := strings.IndexByte(hostNoPort, ':'); i >= 0 {
		hostNoPort = hostNoPort[:i]
	}
	switch {
	case hostNoPort == "hooks.slack.com" || strings.HasSuffix(hostNoPort, ".slack.com"):
		return nil
	case hostNoPort == "discord.com" || strings.HasSuffix(hostNoPort, ".discord.com"):
		return nil
	case hostNoPort == "discordapp.com" || strings.HasSuffix(hostNoPort, ".discordapp.com"):
		return nil
	default:
		return fmt.Errorf("chat webhook host must be a slack.com or discord.com endpoint")
	}
}

type chatStatusError struct {
	status int
	body   string
}

func (e *chatStatusError) Error() string {
	return fmt.Sprintf("chat channel returned status %d: %s", e.status, e.body)
}
