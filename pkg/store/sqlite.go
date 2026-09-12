package store

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
	"github.com/NQAI-Dev/nodepulse/pkg/alerter"
	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

type PersistentStore struct {
	db      *sql.DB
	mem     *Store
	mu      sync.Mutex
	alerter *alerter.Dispatcher
}

func NewPersistentStore(dbPath string, botToken string, chatID int64) (*PersistentStore, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}

	schema := `
	CREATE TABLE IF NOT EXISTS api_tokens (
		token TEXT PRIMARY KEY,
		owner TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS incidents (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		node_id TEXT NOT NULL,
		severity TEXT NOT NULL,
		title TEXT NOT NULL,
		detail TEXT,
		started_at INTEGER NOT NULL,
		resolved INTEGER DEFAULT 0
	);

	CREATE INDEX IF NOT EXISTS idx_incidents_node ON incidents(node_id, resolved);
	`
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}

	db.Exec("INSERT OR IGNORE INTO api_tokens (token, owner) VALUES ('np_live_master_secret', 'admin')")

	return &PersistentStore{
		db:      db,
		mem:     New(),
		alerter: alerter.New(botToken, chatID),
	}, nil
}

func (p *PersistentStore) ValidateToken(token string) bool {
	if token == "" {
		return false
	}
	var count int
	err := p.db.QueryRow("SELECT COUNT(*) FROM api_tokens WHERE token = ?", token).Scan(&count)
	return err == nil && count > 0
}

func (p *PersistentStore) Ingest(hb *protocol.Heartbeat) {
	p.mem.Ingest(hb)

	if hb.Memory.UsedPercent > 92.0 {
		p.CreateIncident(hb.NodeID, "warning", "High Memory Pressure", fmt.Sprintf("RAM usage at %.1f%%", hb.Memory.UsedPercent))
	}
	for _, s := range hb.Services {
		if !s.Active && s.Type == "docker" {
			p.CreateIncident(hb.NodeID, "critical", "Container Stopped: "+s.Name, s.Status)
		}
	}
}

func (p *PersistentStore) CreateIncident(nodeID, severity, title, detail string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var exists int
	err := p.db.QueryRow("SELECT COUNT(*) FROM incidents WHERE node_id = ? AND title = ? AND resolved = 0", nodeID, title).Scan(&exists)
	if err == nil && exists == 0 {
		p.db.Exec("INSERT INTO incidents (node_id, severity, title, detail, started_at, resolved) VALUES (?, ?, ?, ?, ?, 0)",
			nodeID, severity, title, detail, time.Now().Unix())
		if p.alerter != nil {
			go p.alerter.NotifyIncident(nodeID, severity, title, detail)
		}
	}
}

func (p *PersistentStore) ResolveIncident(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.db.Exec("UPDATE incidents SET resolved = 1 WHERE id = ?", id)
	return err
}

func (p *PersistentStore) GetActiveIncidents() []protocol.Incident {
	rows, err := p.db.Query("SELECT id, node_id, severity, title, detail, started_at, resolved FROM incidents WHERE resolved = 0 ORDER BY started_at DESC LIMIT 20")
	if err != nil {
		return nil
	}
	defer rows.Close()

	var list []protocol.Incident
	for rows.Next() {
		var inc protocol.Incident
		var id int64
		var res int
		if err := rows.Scan(&id, &inc.NodeID, &inc.Severity, &inc.Title, &inc.Detail, &inc.StartedAt, &res); err == nil {
			inc.ID = fmt.Sprintf("%d", id)
			inc.Resolved = res == 1
			list = append(list, inc)
		}
	}
	return list
}

func (p *PersistentStore) GetAll() map[string]*NodeState {
	return p.mem.GetAll()
}
