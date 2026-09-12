package store

import (
	"fmt"
	"strconv"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// abandonedContainerTTL is how long a container may stay stopped before we
// give up waiting for it to come back. Without this, incidents like
// "Container Stopped: happy_khorana" stay open forever because the agent
// keeps shipping the container in services (active=false), and the basic
// "container absent from services" resolution path never fires. After
// this window the incident is auto-resolved as "abandoned" so the public
// status page stops reporting outage.
const abandonedContainerTTL = 24 * time.Hour

// ResolveStaleIncidents marks open incidents as resolved when their condition
// is no longer present in the latest heartbeat. Sends a resolution notification
// to Telegram/webhook for each transition.
//
// ponytail: O(2 * open incidents per node) per ingest; cheap until incident
// counts explode; add a per-node cap + lru cache if it ever does.
func (p *PersistentStore) ResolveStaleIncidents(hb *protocol.Heartbeat) {
	rows, err := p.db.Query(
		"SELECT id, node_id, severity, title, started_at FROM incidents WHERE node_id = ? AND resolved = 0",
		hb.NodeID,
	)
	if err != nil {
		return
	}
	defer rows.Close()

	type openInc struct {
		id        int64
		severity  string
		title     string
		startedAt int64
	}
	var open []openInc
	for rows.Next() {
		var i openInc
		var nid string
		if err := rows.Scan(&i.id, &nid, &i.severity, &i.title, &i.startedAt); err == nil {
			open = append(open, i)
		}
	}
	if len(open) == 0 {
		return
	}

	now := time.Now()
	for _, inc := range open {
		reason := incidentResolutionReason(inc.title, hb, now, inc.startedAt)
		if reason == "" {
			continue
		}
		p.markResolvedAndNotify(inc.id, hb.NodeID, inc.severity, inc.title, reason)
	}
}

// incidentResolutionReason returns a non-empty tag when the heartbeat no
// longer exhibits the condition implied by the incident title, "" when the
// incident is still ongoing. The tag is stored on the row so the public
// status timeline and operator audit can distinguish:
//
//   - "auto:service_recovered" — service is back in the services list as active
//   - "auto:service_absent"    — service was dropped from the heartbeat entirely
//   - "auto:abandoned_ttl"     — service kept reporting stopped past abandonedContainerTTL
//   - "auto:metric_recovered"  — high-memory pressure dropped below the 90% threshold
//
// startedAt is the incident row's started_at (unix seconds).
func incidentResolutionReason(title string, hb *protocol.Heartbeat, now time.Time, startedAt int64) string {
	// Memory pressure incidents: title is exactly "High Memory Pressure".
	if title == "High Memory Pressure" {
		if hb.Memory.UsedPercent <= 90.0 {
			return "auto:metric_recovered"
		}
		return ""
	}
	// Docker container incidents: title is "Container Stopped: <name>".
	const dockerPrefix = "Container Stopped: "
	if len(title) > len(dockerPrefix) && title[:len(dockerPrefix)] == dockerPrefix {
		name := title[len(dockerPrefix):]
		for _, s := range hb.Services {
			if s.Name == name {
				// Container came back online.
				if s.Active {
					return "auto:service_recovered"
				}
				// Container still stopped — give up after abandonedContainerTTL
				// so the incident row doesn't pin the fleet at "outage" forever.
				if startedAt > 0 && now.Sub(time.Unix(startedAt, 0)) > abandonedContainerTTL {
					return "auto:abandoned_ttl"
				}
				return ""
			}
		}
		// Service no longer reported by agent → consider it cleared so we
		// don't leave an incident stuck forever after the agent stops
		// shipping metrics for it.
		return "auto:service_absent"
	}
	return ""
}

// incidentStillActive is kept as an alias to incidentResolutionReason for
// older callers; returns true when the incident is still ongoing.
func incidentStillActive(title string, hb *protocol.Heartbeat, now time.Time, startedAt int64) bool {
	return incidentResolutionReason(title, hb, now, startedAt) == ""
}

func (p *PersistentStore) markResolvedAndNotify(id int64, nodeID, severity, title, reason string) {
	p.mu.Lock()
	res, err := p.db.Exec("UPDATE incidents SET resolved = 1, resolved_at = ?, resolution_reason = ? WHERE id = ? AND resolved = 0", time.Now().Unix(), reason, id)
	p.mu.Unlock()
	if err != nil {
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return
	}

	ownerID, _ := p.GetNodeOwner(nodeID)
	settings, _ := p.getSettingsLocked(ownerID)
	ts := time.Now()

	if p.alerter != nil && settings != nil {
		tgChat := p.defaultChatID
		if settings.TelegramChatID != "" {
			if parsed, err := strconv.ParseInt(settings.TelegramChatID, 10, 64); err == nil && parsed != 0 {
				tgChat = parsed
			}
		}
		go p.alerter.NotifyResolvedTo(tgChat, nodeID, severity, title)
	}
	if p.webhook != nil && settings != nil && settings.WebhookURL != "" {
		whEvent := protocol.WebhookAlert{
			Event:     "incident.resolved",
			Timestamp: ts.Unix(),
			Incident: &protocol.Incident{
				ID:        fmt.Sprintf("%d", id),
				NodeID:    nodeID,
				Severity:  severity,
				Title:     title,
				StartedAt: ts.Unix(),
				Resolved:  true,
			},
		}
		go p.webhook.DispatchSigned(settings.WebhookURL, settings.WebhookSecret, whEvent)
	}
}
