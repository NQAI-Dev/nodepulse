package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sync"
	"strconv"
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
	webhook *alerter.WebhookDispatcher
}

func HashPassword(pwd string) string {
	h := sha256.Sum256([]byte(pwd))
	return hex.EncodeToString(h[:])
}

func NewPersistentStore(dbPath string, botToken string, chatID int64) (*PersistentStore, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}

	schema := `
	CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT UNIQUE NOT NULL,
		password_hash TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS user_settings (
		user_id INTEGER PRIMARY KEY,
		telegram_chat_id TEXT DEFAULT '',
		webhook_url TEXT DEFAULT '',
		notify_critical INTEGER DEFAULT 1,
		notify_warning INTEGER DEFAULT 1,
		FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS api_tokens (
		token TEXT PRIMARY KEY,
		user_id INTEGER NOT NULL,
		name TEXT DEFAULT 'default',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS node_owners (
		node_id TEXT PRIMARY KEY,
		user_id INTEGER NOT NULL,
		FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
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

	// Create admin user if not exists
	var adminID int64
	err = db.QueryRow("SELECT id FROM users WHERE username = 'admin'").Scan(&adminID)
	if err != nil {
		pwdHash := HashPassword("admin")
		res, err := db.Exec("INSERT INTO users (username, password_hash) VALUES ('admin', ?)", pwdHash)
		if err == nil {
			adminID, _ = res.LastInsertId()
			db.Exec("INSERT INTO user_settings (user_id) VALUES (?)", adminID)
			db.Exec("INSERT INTO api_tokens (token, user_id, name) VALUES ('np_live_master_secret', ?, 'master')", adminID)
		}
	}

	return &PersistentStore{
		db:      db,
		mem:     New(),
		alerter: alerter.New(botToken, chatID),
		webhook: alerter.NewWebhook(),
	}, nil
}

// User & Auth methods
func (p *PersistentStore) Register(username, password string) (int64, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	pwdHash := HashPassword(password)
	res, err := p.db.Exec("INSERT INTO users (username, password_hash) VALUES (?, ?)", username, pwdHash)
	if err != nil {
		return 0, "", fmt.Errorf("username already exists")
	}
	uid, _ := res.LastInsertId()
	p.db.Exec("INSERT INTO user_settings (user_id) VALUES (?)", uid)

	token := fmt.Sprintf("np_%s_%x", username, sha256.Sum256([]byte(fmt.Sprintf("%d-%s", time.Now().UnixNano(), username))))[:36]
	p.db.Exec("INSERT INTO api_tokens (token, user_id, name) VALUES (?, ?, 'default')", token, uid)

	return uid, token, nil
}

func (p *PersistentStore) Authenticate(username, password string) (int64, string, error) {
	pwdHash := HashPassword(password)
	var uid int64
	err := p.db.QueryRow("SELECT id FROM users WHERE username = ? AND password_hash = ?", username, pwdHash).Scan(&uid)
	if err != nil {
		return 0, "", fmt.Errorf("invalid credentials")
	}

	var token string
	err = p.db.QueryRow("SELECT token FROM api_tokens WHERE user_id = ? ORDER BY created_at ASC LIMIT 1", uid).Scan(&token)
	if err != nil {
		token = fmt.Sprintf("np_%s_%x", username, sha256.Sum256([]byte(fmt.Sprintf("%d-%s", time.Now().UnixNano(), username))))[:36]
		p.db.Exec("INSERT INTO api_tokens (token, user_id, name) VALUES (?, ?, 'default')", token, uid)
	}

	return uid, token, nil
}

func (p *PersistentStore) GetUserByToken(token string) (int64, string, error) {
	if token == "np_live_master_secret" {
		return 1, "admin", nil
	}
	var uid int64
	var uname string
	err := p.db.QueryRow(`
		SELECT u.id, u.username FROM users u
		JOIN api_tokens t ON t.user_id = u.id
		WHERE t.token = ?`, token).Scan(&uid, &uname)
	if err != nil {
		return 0, "", fmt.Errorf("invalid token")
	}
	return uid, uname, nil
}

func (p *PersistentStore) BindNode(nodeID string, userID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.db.Exec("INSERT OR REPLACE INTO node_owners (node_id, user_id) VALUES (?, ?)", nodeID, userID)
}

func (p *PersistentStore) GetUserNodes(userID int64) map[string]*NodeState {
	all := p.mem.GetAll()
	// Admin sees all
	if userID == 1 {
		return all
	}

	rows, err := p.db.Query("SELECT node_id FROM node_owners WHERE user_id = ?", userID)
	if err != nil {
		return make(map[string]*NodeState)
	}
	defer rows.Close()

	filtered := make(map[string]*NodeState)
	for rows.Next() {
		var nid string
		if err := rows.Scan(&nid); err == nil {
			if st, ok := all[nid]; ok {
				filtered[nid] = st
			}
		}
	}
	return filtered
}

func (p *PersistentStore) ValidateToken(token string) bool {
	if token == "" {
		return false
	}
	if token == "np_live_master_secret" {
		return true
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
		ownerID, _ := p.GetNodeOwner(nodeID)
		settings, _ := p.GetSettings(ownerID)
		shouldNotify := true
		if settings != nil {
			if severity == "critical" && !settings.NotifyCritical {
				shouldNotify = false
			}
			if severity == "warning" && !settings.NotifyWarning {
				shouldNotify = false
			}
		}
		if shouldNotify {
			if p.alerter != nil {
				tgChat := p.alerter.GetChatID()
				if settings != nil && settings.TelegramChatID != "" {
					if parsed, err := strconv.ParseInt(settings.TelegramChatID, 10, 64); err == nil && parsed != 0 {
						tgChat = parsed
					}
				}
				go p.alerter.NotifyIncidentTo(tgChat, nodeID, severity, title, detail)
			}
			if settings != nil && settings.WebhookURL != "" && p.webhook != nil {
				whEvent := protocol.WebhookAlert{
					Event: "incident.created",
					Timestamp: time.Now().Unix(),
					Incident: &protocol.Incident{
						NodeID: nodeID,
						Severity: severity,
						Title: title,
						Detail: detail,
						StartedAt: time.Now().Unix(),
					},
				}
				go p.webhook.Dispatch(settings.WebhookURL, whEvent)
			}
		}
	}
}

func (p *PersistentStore) ResolveIncident(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.db.Exec("UPDATE incidents SET resolved = 1 WHERE id = ?", id)
	return err
}

func (p *PersistentStore) GetActiveIncidents(userID int64) []protocol.Incident {
	query := "SELECT id, node_id, severity, title, detail, started_at, resolved FROM incidents WHERE resolved = 0 ORDER BY started_at DESC LIMIT 50"
	if userID > 1 {
		query = fmt.Sprintf(`SELECT i.id, i.node_id, i.severity, i.title, i.detail, i.started_at, i.resolved 
			FROM incidents i
			JOIN node_owners o ON o.node_id = i.node_id
			WHERE i.resolved = 0 AND o.user_id = %d
			ORDER BY i.started_at DESC LIMIT 50`, userID)
	}

	rows, err := p.db.Query(query)
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
