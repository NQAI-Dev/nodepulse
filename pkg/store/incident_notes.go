package store

import (
	"errors"
	"strings"
	"time"
)

// IncidentNote is a single free-form operator comment attached to an
// incident. Bodies are intentionally short — chat-grade hand-off context,
// not a post-mortem. Long notes should land in the wiki/issue tracker and
// just be linked here.
type IncidentNote struct {
	ID         int64  `json:"id"`
	IncidentID string `json:"incident_id"`
	UserID     int64  `json:"user_id"`
	Username   string `json:"username"`
	Body       string `json:"body"`
	CreatedAt  int64  `json:"created_at"`
}

// ErrIncidentNoteEmpty is returned when an operator posts a blank note — we
// keep that distinct from "note too long" because the UI surfaces a
// different remediation hint for each.
var (
	ErrIncidentNoteEmpty   = errors.New("note body is empty")
	ErrIncidentNoteTooLong = errors.New("note body exceeds 1024 chars")
)

// AddIncidentNote inserts a comment on an incident after validating length
// and confirming the incident exists. We do not enforce node ownership
// here on purpose: on-call rotations span multiple tenants in some
// deployments, and the audit trail (user_id + username) is enough to
// attribute abuse.
//
// ponytail: if/when we add role-based permissions, swap the existence check
// for a JOIN against node_owners and gate with the user's role.
func (p *PersistentStore) AddIncidentNote(incidentID string, userID int64, username, body string) (IncidentNote, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return IncidentNote{}, ErrIncidentNoteEmpty
	}
	if len(body) > 1024 {
		return IncidentNote{}, ErrIncidentNoteTooLong
	}
	if username == "" {
		username = "user"
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	var exists int
	err := p.db.QueryRow("SELECT 1 FROM incidents WHERE id = ?", incidentID).Scan(&exists)
	if err != nil {
		// sql.ErrNoRows falls through to ErrIncidentNotFound so the handler
		// can map it to a 404 like the rest of the incidents API.
		return IncidentNote{}, ErrIncidentNotFound
	}

	now := time.Now().Unix()
	res, err := p.db.Exec(`INSERT INTO incident_notes
		(incident_id, user_id, username, body, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		incidentID, userID, username, body, now)
	if err != nil {
		return IncidentNote{}, err
	}
	id, _ := res.LastInsertId()
	return IncidentNote{
		ID:         id,
		IncidentID: incidentID,
		UserID:     userID,
		Username:   username,
		Body:       body,
		CreatedAt:  now,
	}, nil
}

// ListIncidentNotes returns every note attached to an incident, oldest
// first — that matches the natural top-to-bottom timeline the UI renders.
// Capped at 200 rows; an incident with 200 comments has bigger problems.
func (p *PersistentStore) ListIncidentNotes(incidentID string) ([]IncidentNote, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	rows, err := p.db.Query(`SELECT id, incident_id, user_id, username, body, created_at
		FROM incident_notes
		WHERE incident_id = ?
		ORDER BY created_at ASC, id ASC
		LIMIT 200`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var notes []IncidentNote
	for rows.Next() {
		var n IncidentNote
		if err := rows.Scan(&n.ID, &n.IncidentID, &n.UserID, &n.Username, &n.Body, &n.CreatedAt); err != nil {
			return nil, err
		}
		notes = append(notes, n)
	}
	return notes, nil
}
