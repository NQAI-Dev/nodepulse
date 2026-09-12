package store

import (
	"fmt"
	"strconv"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// ResolveStaleIncidents marks open incidents as resolved when their condition
// is no longer present in the latest heartbeat. Sends a resolution notification
// to Telegram/webhook for each transition.
//
// ponytail: O(2 * open incidents per node) per ingest; cheap until incident
// counts explode; add a per-node cap + lru cache if it ever does.
func (p *PersistentStore) ResolveStaleIncidents(hb *protocol.Heartbeat) {
	rows, err := p.db.Query(
		"SELECT id, node_id, severity, title FROM incidents WHERE node_id = ? AND resolved = 0",
		hb.NodeID,
	)
	if err != nil {
		return
	}
	defer rows.Close()

	type openInc struct {
		id       int64
		severity string
		title    string
	}
	var open []openInc
	for rows.Next() {
		var i openInc
		var nid string
		if err := rows.Scan(&i.id, &nid, &i.severity, &i.title); err == nil {
			open = append(open, i)
		}
	}
	if len(open) == 0 {
		return
	}

	for _, inc := range open {
		if incidentStillActive(inc.title, hb) {
			continue
		}
		p.markResolvedAndNotify(inc.id, hb.NodeID, inc.severity, inc.title)
	}
}

// incidentStillActive returns true when the heartbeat still exhibits the
// condition implied by the incident title.
func incidentStillActive(title string, hb *protocol.Heartbeat) bool {
	// Memory pressure incidents: title is exactly "High Memory Pressure".
	if title == "High Memory Pressure" {
		return hb.Memory.UsedPercent > 90.0
	}
	// Docker container incidents: title is "Container Stopped: <name>".
	const dockerPrefix = "Container Stopped: "
	if len(title) > len(dockerPrefix) && title[:len(dockerPrefix)] == dockerPrefix {
		name := title[len(dockerPrefix):]
		for _, s := range hb.Services {
			if s.Name == name {
				// Still considered active while the container is down.
				return !s.Active
			}
		}
		// Service no longer reported by agent → consider it cleared so we
		// don't leave an incident stuck forever after the agent stops
		// shipping metrics for it.
		return false
	}
	return false
}

func (p *PersistentStore) markResolvedAndNotify(id int64, nodeID, severity, title string) {
	p.mu.Lock()
	res, err := p.db.Exec("UPDATE incidents SET resolved = 1 WHERE id = ? AND resolved = 0", id)
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
