package store

import (
	"sort"
	"strings"
	"time"
)

// UpsertNodeTags persists the latest tag set for a node. Empty input
// clears the row so a tag-less heartbeat doesn't leave stale labels
// hanging around (e.g. after an operator removes an env=prod label on
// the agent side).
func (p *PersistentStore) UpsertNodeTags(nodeID string, tags []string) error {
	if nodeID == "" {
		return nil
	}
	clean := sanitizeTags(tags)
	_, err := p.db.Exec(
		`INSERT INTO node_tags (node_id, tags, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(node_id) DO UPDATE SET tags = excluded.tags, updated_at = excluded.updated_at`,
		nodeID, strings.Join(clean, "\n"), time.Now().Unix(),
	)
	return err
}

// TagsForNode returns the persisted tag list for a node. Empty slice
// (not nil) when nothing is on file so callers can JSON-encode it as [].
func (p *PersistentStore) TagsForNode(nodeID string) []string {
	row := p.db.QueryRow(`SELECT tags FROM node_tags WHERE node_id = ?`, nodeID)
	var raw string
	if err := row.Scan(&raw); err != nil {
		return []string{}
	}
	if raw == "" {
		return []string{}
	}
	return strings.Split(raw, "\n")
}

// AllTaggedNodes returns node_id → tags for every node that has at least
// one tag. Used by the public status page to enrich per-node rows and by
// the tag-filter endpoint when no specific tag is requested.
func (p *PersistentStore) AllTaggedNodes() map[string][]string {
	out := make(map[string][]string)
	rows, err := p.db.Query(`SELECT node_id, tags FROM node_tags WHERE tags != ''`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			continue
		}
		if raw == "" {
			continue
		}
		out[id] = strings.Split(raw, "\n")
	}
	return out
}

// NodeIDsByTag returns the set of node ids carrying `tag` (exact match,
// e.g. "env=prod"). Empty tag returns nil. Cheap scan over node_tags is
// fine while the table stays in the low thousands of nodes — the index
// on the primary key alone is enough for the status-page traffic profile.
func (p *PersistentStore) NodeIDsByTag(tag string) []string {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return nil
	}
	// Exact tag match, anchored on the newline separators so `env=prod`
	// never matches `env=prod-nightly`. Four cases cover every position:
	// single tag row, tag at the start, tag in the middle, tag at the end.
	// CHAR(10) instead of '\n' because modernc/sqlite leaves the SQL escape
	// sequence alone (no string-literal escape handling).
	rows, err := p.db.Query(
		`SELECT node_id FROM node_tags
		 WHERE tags = ?
		    OR tags LIKE ? || CHAR(10) || '%'
		    OR tags LIKE '%' || CHAR(10) || ?
		    OR tags LIKE '%' || CHAR(10) || ? || CHAR(10) || '%'`,
		tag, tag, tag, tag,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// sanitizeTags drops empties, trims whitespace, deduplicates while keeping
// first occurrence. Order-stable so repeated ingests don't churn the row.
func sanitizeTags(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}
