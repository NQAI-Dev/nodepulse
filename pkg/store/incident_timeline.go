package store

import (
	"sort"
)

// TimelineEvent is one row in the incident timeline. Kind tells the UI how
// to render ("ack", "resolve", "note"); the rest of the fields are
// optional depending on the kind. Notes carry body + author; ack/resolve
// carry only the timestamp.
//
// We materialize as a single struct rather than an interface{} so the
// front-end can read each field defensively without a switch on type.
type TimelineEvent struct {
	Kind      string `json:"kind"` // "ack" | "resolve" | "note"
	Timestamp int64  `json:"ts"`
	Username  string `json:"username,omitempty"`
	Body      string `json:"body,omitempty"`
	// Resolve-only: short reason tag ("manual", "auto:abandoned_ttl",
	// "maintenance", …) so the timeline can colour-code the close event
	// without a second lookup.
	Reason string `json:"reason,omitempty"`
}

// IncidentTimeline merges every audit event for a single incident (ack,
// resolve, operator notes) into one chronologically ordered list. The
// caller is expected to have already verified ownership — this method
// returns no error for "unknown id" because empty-timeline is a valid
// answer (incident exists but has no events yet).
//
// ponytail: built with three small SELECTs + an in-memory merge so the
// SQL stays portable. If timelines ever need to span thousands of notes
// per incident, switch to a UNION ALL view + a single ORDER BY.
func (p *PersistentStore) IncidentTimeline(incidentID string) ([]TimelineEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	events := make([]TimelineEvent, 0, 8)

	// Incident header row — only the timestamps we care about.
	var acknowledgedAt, resolvedAt int64
	var resolutionReason string
	err := p.db.QueryRow(`SELECT
		COALESCE(acknowledged_at, 0),
		COALESCE(resolved_at, 0),
		COALESCE(resolution_reason, '')
	    FROM incidents WHERE id = ?`, incidentID).Scan(
		&acknowledgedAt, &resolvedAt, &resolutionReason)
	if err != nil {
		// sql.ErrNoRows is a valid "no events yet" — return empty.
		return events, nil
	}

	if acknowledgedAt > 0 {
		events = append(events, TimelineEvent{
			Kind:      "ack",
			Timestamp: acknowledgedAt,
		})
	}
	if resolvedAt > 0 {
		events = append(events, TimelineEvent{
			Kind:      "resolve",
			Timestamp: resolvedAt,
			Reason:    resolutionReason,
		})
	}

	rows, err := p.db.Query(`SELECT username, body, created_at
		FROM incident_notes
		WHERE incident_id = ?
		ORDER BY created_at ASC, id ASC`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ev TimelineEvent
		if err := rows.Scan(&ev.Username, &ev.Body, &ev.Timestamp); err != nil {
			return nil, err
		}
		ev.Kind = "note"
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.SliceStable(events, func(i, j int) bool {
		return events[i].Timestamp < events[j].Timestamp
	})
	return events, nil
}
