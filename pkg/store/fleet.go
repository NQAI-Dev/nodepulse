package store

import (
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// GetPublicFleetSummary returns a compact fleet snapshot for status widgets.
// Cheaper than GetPublicStatus — no per-service breakdown, no per-node
// uptime rows — so external monitors can poll it frequently.
//
// ponytail: the 7-day uptime rollup reads from the same data path as
// AllNodesUptime(7). If polling volume grows, swap to a cached summary
// invalidated on every heartbeat ingest.
func (p *PersistentStore) GetPublicFleetSummary() protocol.PublicFleetSummary {
	allNodes := p.mem.GetAll()
	now := time.Now()
	summary := protocol.PublicFleetSummary{
		UpdatedAt:  now.Unix(),
		NodesTotal: len(allNodes),
		Status:     "operational",
	}

	for _, n := range allNodes {
		switch n.Status {
		case "online":
			summary.NodesOnline++
		case "warning":
			summary.NodesWarning++
		default:
			summary.NodesOffline++
		}
	}

	openIncidents := p.GetActiveIncidents(1)
	for _, inc := range openIncidents {
		summary.IncidentsOpen++
		if inc.Severity == "critical" {
			summary.IncidentsCrit++
		}
	}

	if summary.IncidentsCrit > 0 {
		summary.Status = "outage"
	} else if summary.IncidentsOpen > 0 || summary.NodesOffline > 0 || summary.NodesWarning > 0 {
		summary.Status = "degraded"
	}

	if summary.NodesTotal > 0 {
		if rows, err := p.AllNodesUptime(7); err == nil && len(rows) > 0 {
			// Weight by observed seconds, not by UptimePct * Days: the old
			// formula collapsed to `Days` (always 7) because every row has
			// the same window — uptime_7d_pct became literally 7 for any
			// non-zero fleet. Real fleet uptime is up_secs / total_secs.
			var up, observed int64
			for _, r := range rows {
				up += r.UpSecs
				observed += r.TotalSecs
			}
			if observed > 0 {
				summary.Uptime7dPct = float64(up) / float64(observed) * 100.0
			}
		}
	}

	return summary
}
