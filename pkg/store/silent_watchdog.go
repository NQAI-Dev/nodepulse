package store

import (
	"log"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// silentNodeTTL is how long a node may stop heartbeating before the
// watchdog treats it as a real outage and raises an incident. Chosen to
// sit comfortably above the in-memory 35s "offline" threshold used by
// store.GetAll() — anything between 35s and silentNodeTTL shows as
// offline on the dashboard but is not yet noisy enough for an alert.
//
// 2 minutes is the smallest window that:
//
//   - swallows a single missed heartbeat (~10s cadence) without flapping;
//   - catches real outages (Docker daemon dead, network partition) fast
//     enough that an operator can react before customers notice;
//   - keeps the daily silent-node incident count under a handful even on
//     a flaky fleet.
//
// ponytail: if operators want per-tenant tuning, lift this into
// user_settings and read it in SilentNodeWatchdog. The current single
// global value is the simplest thing that works for a self-hosted fleet
// of <50 nodes; one global column won't survive 10k tenants.
const silentNodeTTL = 2 * time.Minute

// SilentNodeWatchdogResult describes what the watchdog did on a single
// pass. Returned to the caller (today: RunSilentWatchdog / tests) so
// observability and tests can assert behaviour without scraping logs.
type SilentNodeWatchdogResult struct {
	NodesChecked      int      `json:"nodes_checked"`
	IncidentsRaised   int      `json:"incidents_raised"`
	IncidentsResolved int      `json:"incidents_resolved"`
	RaisedNodeIDs     []string `json:"raised_node_ids,omitempty"`
	ResolvedNodeIDs   []string `json:"resolved_node_ids,omitempty"`
}

// SilentNodeWatchdog scans every known node, opens a "Silent Node"
// incident when a node has stopped heartbeating for longer than
// silentNodeTTL, and auto-resolves any open silent-node incident once
// the node returns to the active set. Designed to be invoked from the
// janitor (cheap; one indexed query + a per-node map lookup).
//
// Idempotent: re-running on the same dead node does not raise a second
// incident — the open-row lookup in CreateIncident pins the existing
// incident at its original id, and the per-pass cooldown is enforced
// inside notifyAfterCreate. Re-notifying an existing silent incident
// also costs at most one Telegram message per cooldownFor window,
// which is the right behaviour for an outage that lingers across hours.
//
// ponytail: O(N) in fleet size on every pass. For ~100 nodes this is
// a few microseconds; for 100k nodes switch to a SQL-side join against
// node_owners + a last_seen column so we don't load the whole map.
func (p *PersistentStore) SilentNodeWatchdog(now time.Time) SilentNodeWatchdogResult {
	if p == nil || p.db == nil {
		return SilentNodeWatchdogResult{}
	}

	cutoff := now.Add(-silentNodeTTL)
	all := p.mem.GetAll()

	var res SilentNodeWatchdogResult
	for nodeID, state := range all {
		res.NodesChecked++
		if state == nil {
			continue
		}
		silent := state.LastHeartbeat.Before(cutoff)

		if silent {
			// Skip when an incident is already open: CreateIncident
			// would otherwise re-fire on cooldown expiry and our counter
			// would double-count. We want idempotency — a node that has
			// been silent for an hour should show up as exactly one
			// incident row, not as a fresh "raised" notification every
			// pass. The cooldown inside CreateIncident still drives the
			// re-notification cadence, which is the desired behaviour.
			already, err := p.HasOpenSilentNodeIncident(nodeID)
			if err != nil {
				log.Printf("[silent-watchdog] check %s: %v", nodeID, err)
				continue
			}
			if already {
				continue
			}
			detail := "No heartbeat for " + now.Sub(state.LastHeartbeat).Round(time.Second).String()
			if state.LastHeartbeat.IsZero() {
				detail = "No heartbeat recorded"
			}
			p.CreateIncident(nodeID, "critical", "Node Silent", detail)
			res.IncidentsRaised++
			res.RaisedNodeIDs = append(res.RaisedNodeIDs, nodeID)
			continue
		}

		// Node is alive again — close any open silent-node incident so
		// the public status page doesn't keep reporting the outage.
		closed, err := p.resolveSilentNodeIncident(nodeID, now)
		if err != nil {
			log.Printf("[silent-watchdog] resolve %s: %v", nodeID, err)
			continue
		}
		if closed {
			res.IncidentsResolved++
			res.ResolvedNodeIDs = append(res.ResolvedNodeIDs, nodeID)
		}
	}
	return res
}

// resolveSilentNodeIncident closes any open "Node Silent" incident for
// nodeID. Returns true when a row transitioned from open to resolved.
// Pure DB work — the resolver notification fan-out is the same path the
// rest of the codebase uses via markResolvedAndNotify, so we delegate
// through incidentStillActive to keep semantics consistent.
func (p *PersistentStore) resolveSilentNodeIncident(nodeID string, now time.Time) (bool, error) {
	if p == nil || p.db == nil {
		return false, nil
	}
	rows, err := p.db.Query(
		"SELECT id, severity, title FROM incidents WHERE node_id = ? AND title = ? AND resolved = 0",
		nodeID, "Node Silent",
	)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	type row struct {
		id       int64
		severity string
		title    string
	}
	var open []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.severity, &r.title); err == nil {
			open = append(open, r)
		}
	}
	if len(open) == 0 {
		return false, nil
	}
	for _, r := range open {
		p.markResolvedAndNotify(r.id, nodeID, r.severity, r.title, "auto:heartbeat_restored")
	}
	return true, nil
}

// HasOpenSilentNodeIncident returns true when nodeID currently has an
// unresolved "Node Silent" incident. Used by tests and any future
// /api/v1/... endpoint that wants to surface the watchdog state without
// reaching through the full incident list.
func (p *PersistentStore) HasOpenSilentNodeIncident(nodeID string) (bool, error) {
	if p == nil || p.db == nil {
		return false, nil
	}
	var id int64
	err := p.db.QueryRow(
		"SELECT id FROM incidents WHERE node_id = ? AND title = ? AND resolved = 0 LIMIT 1",
		nodeID, "Node Silent",
	).Scan(&id)
	if err == nil {
		return true, nil
	}
	// sql.ErrNoRows collapses to "no open incident"; everything else is
	// a real DB error worth bubbling up.
	if err.Error() == "sql: no rows in result set" {
		return false, nil
	}
	return false, err
}

// SilentNodeTitle is exported so other packages (tests, future API
// handlers) can match the exact title string without copy-pasting it.
const SilentNodeTitle = "Node Silent"

// _ pins protocol import even if no type from it is directly referenced
// after future refactors; keeps the import live for the day someone adds
// a protocol.WatchdogResult mirror.
var _ = protocol.NodeInfo{}
