package store

import (
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func (p *PersistentStore) GetPublicStatus() protocol.PublicStatusPage {
	allNodes := p.mem.GetAll()
	totalNodes := len(allNodes)
	onlineNodes := 0
	var services []protocol.PublicService

	for nodeID, n := range allNodes {
		if n.Status == "online" {
			onlineNodes++
		}
		for _, s := range n.Latest.Services {
			services = append(services, protocol.PublicService{
				Name:   s.Name,
				Node:   nodeID,
				Status: s.Status,
				Active: s.Active,
			})
		}
	}

	incidents := p.GetActiveIncidents(1)
	var pubIncidents []protocol.PublicIncident
	for _, inc := range incidents {
		pubIncidents = append(pubIncidents, protocol.PublicIncident{
			ID:        inc.ID,
			NodeID:    inc.NodeID,
			Title:     inc.Title,
			Severity:  inc.Severity,
			StartedAt: inc.StartedAt,
		})
	}

	systemStatus := "operational"
	if len(pubIncidents) > 0 {
		systemStatus = "degraded"
		for _, inc := range pubIncidents {
			if inc.Severity == "critical" {
				systemStatus = "outage"
				break
			}
		}
	} else if totalNodes > 0 && onlineNodes < totalNodes {
		systemStatus = "degraded"
	}

	return protocol.PublicStatusPage{
		Title:           "NodePulse Cloud System Status",
		Description:     "Real-time operational telemetry and health status of NodePulse monitored infrastructure.",
		Status:          systemStatus,
		UpdatedAt:       time.Now().Unix(),
		NodesTotal:      totalNodes,
		NodesOnline:     onlineNodes,
		Services:        services,
		Incidents:       pubIncidents,
	}
}
