package store

import (
	"sort"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// ListPublicNodes returns the public-facing node list, optionally filtered
// by an exact tag (e.g. "env=prod"). Tag is matched against the persisted
// node_tags rows; empty tag returns everything currently heartbeating.
//
// ponytail: when node count grows past ~5k and tag-filter calls turn into
// a hot path, denormalise tags into a per-tag lookup table. For the
// status-page traffic profile (a few widget polls per minute) the LIKE
// scan is fine.
func (p *PersistentStore) ListPublicNodes(tag string) []protocol.PublicNode {
	all := p.mem.GetAll()
	tagMap := p.AllTaggedNodes()

	var allowed map[string]struct{}
	if tag != "" {
		ids := p.NodeIDsByTag(tag)
		allowed = make(map[string]struct{}, len(ids))
		for _, id := range ids {
			allowed[id] = struct{}{}
		}
	}

	out := make([]protocol.PublicNode, 0, len(all))
	for nodeID, st := range all {
		if allowed != nil {
			if _, ok := allowed[nodeID]; !ok {
				continue
			}
		}
		hostname := st.Info.Hostname
		if hostname == "" {
			hostname = nodeID
		}
		tags := tagMap[nodeID]
		if tags == nil {
			tags = []string{}
		}
		out = append(out, protocol.PublicNode{
			NodeID:    nodeID,
			Hostname:  hostname,
			Status:    st.Status,
			Tags:      tags,
			UpdatedAt: st.LastHeartbeat.Unix(),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// TouchNodeTags is a small admin/operator convenience exposed through
// the API later if needed: lets ops edit tags without waiting for the
// agent's next heartbeat. Returns the canonicalised tag list that ended
// up persisted, so callers can echo it back to the user.
func (p *PersistentStore) TouchNodeTags(nodeID string, tags []string) ([]string, error) {
	if err := p.UpsertNodeTags(nodeID, tags); err != nil {
		return nil, err
	}
	return p.TagsForNode(nodeID), nil
}

// now is overridable for tests; defaulting keeps the public surface tiny.
var nowFn = time.Now
