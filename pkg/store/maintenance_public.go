package store

import (
	"strings"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// GetPublicMaintenanceWindows returns active and upcoming maintenance windows
// safe for public status pages and syndication.
func (p *PersistentStore) GetPublicMaintenanceWindows() ([]protocol.PublicMaintenanceNotice, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now().Unix()
	rows, err := p.db.Query(`SELECT id, scope, reason, node_ids, start_unix, end_unix
		FROM maintenance_windows
		WHERE end_unix = 0 OR end_unix > ?
		ORDER BY start_unix ASC`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var res []protocol.PublicMaintenanceNotice
	for rows.Next() {
		var id, startUnix, endUnix int64
		var scope, reason, nodeIDs string
		if err := rows.Scan(&id, &scope, &reason, &nodeIDs, &startUnix, &endUnix); err != nil {
			return nil, err
		}

		var nodes []string
		if nodeIDs != "" {
			for _, n := range strings.Split(nodeIDs, ",") {
				if t := strings.TrimSpace(n); t != "" {
					nodes = append(nodes, t)
				}
			}
		}

		isActive := (startUnix <= now) && (endUnix == 0 || endUnix > now)

		res = append(res, protocol.PublicMaintenanceNotice{
			ID:        id,
			Scope:     scope,
			NodeIDs:   nodes,
			Reason:    reason,
			StartUnix: startUnix,
			EndUnix:   endUnix,
			Active:    isActive,
		})
	}
	if res == nil {
		res = []protocol.PublicMaintenanceNotice{}
	}
	return res, nil
}
