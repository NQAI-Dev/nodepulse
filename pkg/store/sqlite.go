package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
	"github.com/NQAI-Dev/nodepulse/pkg/alerter"
	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

type PersistentStore struct {
	db            *sql.DB
	mem           *Store
	mu            sync.Mutex
	alerter       alerter.Notifier
	webhook       *alerter.WebhookDispatcher
	whRecorder    *alerter.WebhookRecorder // audit-trail wrapper around webhook
	// chatDispatch fans an incident out to Slack + Discord incoming
	// webhooks. Optional — nil disables the chat path entirely so older
	// binaries and tests without chat routing don't have to fake the
	// dispatcher. Set via SetChatDispatcher from cmd/server at startup.
	chatDispatch  *alerter.ChatDispatcher
	defaultChatID int64 // remembered at construction so we can target the configured chat without asking the Notifier
	uptime        *uptimeTracker
	netRates      *networkRateTracker
	// alertEval / alertInitOnce own the per-rule breach state for
	// EvaluateMetricAlert. Lazily allocated so legacy constructor paths
	// (notably the test helpers) keep working without parameter churn.
	alertEval     *AlertEvaluator
	alertInitOnce sync.Once
}

// Recorder exposes the webhook audit-trail wrapper so the janitor can drain
// its in-memory buffer into SQLite. May be nil if the constructor is invoked
// via lower-level test helpers; callers must nil-check.
func (p *PersistentStore) Recorder() *alerter.WebhookRecorder { return p.whRecorder }

// SetNotifier swaps the outbound user-facing dispatcher. Used by tests to
// capture calls; production wiring stays on the Telegram *Dispatcher.
//
// ponytail: setters beat constructors once the surface gets >5 callers; if
// the constructor signature ever changes for other reasons, fold this in.
func (p *PersistentStore) SetNotifier(n alerter.Notifier) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.alerter = n
}

// SetChatDispatcher attaches the Slack + Discord fan-out. Optional;
// nil disables both channels. The dispatcher reuses the same
// WebhookRecorder that the generic webhook side uses for audit rows,
// so this setter only stores the dispatcher reference.
func (p *PersistentStore) SetChatDispatcher(c *alerter.ChatDispatcher) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.chatDispatch = c
}

// ChatDispatcher exposes the optional chat fan-out for the operator
// stats endpoint. May be nil if the server was started without chat
// channels wired (legacy binary, test harness).
func (p *PersistentStore) ChatDispatcher() *alerter.ChatDispatcher {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.chatDispatch
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
		webhook_secret TEXT DEFAULT '',
		slack_webhook_url TEXT DEFAULT '',
		discord_webhook_url TEXT DEFAULT '',
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

	CREATE TABLE IF NOT EXISTS telegram_users (
		tg_id INTEGER PRIMARY KEY,
		user_id INTEGER NOT NULL,
		tg_username TEXT DEFAULT '',
		tg_first_name TEXT DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS incidents (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		node_id TEXT NOT NULL,
		severity TEXT NOT NULL,
		title TEXT NOT NULL,
		detail TEXT,
		started_at INTEGER NOT NULL,
		resolved INTEGER DEFAULT 0,
		resolved_at INTEGER DEFAULT 0,
		last_notified_at INTEGER DEFAULT 0,
		acknowledged_at INTEGER DEFAULT 0,
		resolution_reason TEXT DEFAULT ''
	);

	CREATE INDEX IF NOT EXISTS idx_incidents_node ON incidents(node_id, resolved);

	CREATE TABLE IF NOT EXISTS autoheal_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		node_id TEXT NOT NULL,
		command TEXT NOT NULL,
		status TEXT NOT NULL,
		reason TEXT,
		error TEXT,
		ts INTEGER NOT NULL
	);
	`
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}

	// Idempotent migrations for older databases.
	// We ADD COLUMN last_notified_at if it doesn't exist (ignore errors
	// that indicate the column already exists).
	_, _ = db.Exec("ALTER TABLE incidents ADD COLUMN last_notified_at INTEGER DEFAULT 0")
	_, _ = db.Exec("ALTER TABLE incidents ADD COLUMN resolved_at INTEGER DEFAULT 0")
	_, _ = db.Exec("ALTER TABLE incidents ADD COLUMN acknowledged_at INTEGER DEFAULT 0")
	// resolution_reason: short tag ("manual", "auto:service_recovered",
	// "auto:service_absent", "auto:abandoned_ttl", "maintenance") so the
	// public status timeline and operator audit can tell *why* an incident
	// closed itself without joining against heartbeat history.
	_, _ = db.Exec("ALTER TABLE incidents ADD COLUMN resolution_reason TEXT DEFAULT ''")
	// snoozed_until: unix timestamp until which re-notifications for this
	// incident are suppressed. 0 = not snoozed. Operators set it via the
	// Telegram inline-keyboard snooze shortcut; the alert path checks it
	// inside notifyAfterCreate so no extra round-trip is needed.
	_, _ = db.Exec("ALTER TABLE incidents ADD COLUMN snoozed_until INTEGER DEFAULT 0")

	// chat-webhook channels: Slack Incoming Webhook URLs and Discord
	// Webhook URLs are stored on user_settings so operators don't have
	// to spin their own signed endpoint like with the generic webhook.
	// "duplicate column name" failures on already-migrated DBs are
	// swallowed silently, matching the style of the other ALTER TABLE
	// calls in this block.
	_, _ = db.Exec("ALTER TABLE user_settings ADD COLUMN slack_webhook_url TEXT DEFAULT ''")
	_, _ = db.Exec("ALTER TABLE user_settings ADD COLUMN discord_webhook_url TEXT DEFAULT ''")

	// incident_notes: free-form operator comments attached to an incident.
	// Rendered alongside the existing audit timeline (ack/resolve events)
	// so on-call can hand off context. user_id + username are denormalized
	// so we can render the author without joining against users. body is
	// kept short on purpose — chat-grade notes, not post-mortems.
	db.Exec(`CREATE TABLE IF NOT EXISTS incident_notes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		incident_id TEXT NOT NULL,
		user_id INTEGER NOT NULL,
		username TEXT NOT NULL DEFAULT '',
		body TEXT NOT NULL,
		created_at INTEGER NOT NULL
	)`)
	db.Exec("CREATE INDEX IF NOT EXISTS idx_incnotes_incident ON incident_notes(incident_id, created_at)")
	db.Exec("CREATE INDEX IF NOT EXISTS idx_incidents_active ON incidents(node_id, title, resolved)")
	db.Exec("CREATE INDEX IF NOT EXISTS idx_incidents_history ON incidents(started_at, resolved)")
	db.Exec("CREATE INDEX IF NOT EXISTS idx_autoheal_logs_node_ts ON autoheal_logs(node_id, ts)")

	// Maintenance windows: planned silence periods during which alerts for
	// the matching nodes are suppressed. A row with end_unix=0 stays open
	// until the operator closes it; rows with end_unix < now are filtered
	// out of the active-window check at query time so we don't pay a
	// background sweep to garbage-collect them.
	db.Exec(`CREATE TABLE IF NOT EXISTS maintenance_windows (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		scope TEXT NOT NULL DEFAULT 'user',
		reason TEXT DEFAULT '',
		node_ids TEXT DEFAULT '',
		start_unix INTEGER NOT NULL,
		end_unix INTEGER DEFAULT 0,
		created_at INTEGER NOT NULL,
		created_by TEXT DEFAULT ''
	)`)
	db.Exec("CREATE INDEX IF NOT EXISTS idx_mw_user ON maintenance_windows(user_id, end_unix)")
	db.Exec("CREATE INDEX IF NOT EXISTS idx_mw_nodes ON maintenance_windows(node_ids, end_unix)")

	// Webhook delivery audit: one row per outbound webhook attempt. The
	// recorder (pkg/alerter/webhook_recorder.go) buffers rows in memory and
	// FlushWebhookDeliveries drains them in batches; the janitor prunes
	// anything older than webhookDeliveryRetentionDays to keep the table
	// bounded. user_id is denormalized so the operator endpoint can scope
	// reads without joining against incidents/node_owners.
	db.Exec(`CREATE TABLE IF NOT EXISTS webhook_deliveries (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		incident_id TEXT DEFAULT '',
		event TEXT NOT NULL,
		url TEXT NOT NULL,
		attempts INTEGER NOT NULL DEFAULT 1,
		status INTEGER NOT NULL DEFAULT 0,
		ok INTEGER NOT NULL DEFAULT 0,
		error TEXT DEFAULT '',
		latency_ms INTEGER NOT NULL DEFAULT 0,
		total_latency_ms INTEGER NOT NULL DEFAULT 0,
		ts INTEGER NOT NULL,
		payload TEXT DEFAULT ''
	)`)
	db.Exec("CREATE INDEX IF NOT EXISTS idx_whdel_user_ts ON webhook_deliveries(user_id, ts)")
	db.Exec("CREATE INDEX IF NOT EXISTS idx_whdel_ts ON webhook_deliveries(ts)")

	// payload: JSON-serialized WebhookAlert captured at dispatch time so an
	// operator can manually retry a failed delivery verbatim, even after the
	// original incident is resolved or the webhook URL/secret has rotated.
	_, _ = db.Exec("ALTER TABLE webhook_deliveries ADD COLUMN payload TEXT DEFAULT ''")

	// Node tags: persisted copy of the agent-reported labels. Stored as a
	// newline-separated list (one tag per line) so the LIKE-based filter
	// path doesn't need a JSON parser and can match `env=prod` exactly.
	// updated_at drives "since X" queries for status widgets that only
	// care about recently-tagged fleets.
	db.Exec(`CREATE TABLE IF NOT EXISTS node_tags (
		node_id TEXT PRIMARY KEY,
		tags TEXT NOT NULL DEFAULT '',
		updated_at INTEGER NOT NULL
	)`)

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

	uptimeSchema := `
	CREATE TABLE IF NOT EXISTS node_uptime_daily (
		node_id TEXT NOT NULL,
		day TEXT NOT NULL,
		total_secs INTEGER NOT NULL DEFAULT 0,
		up_secs INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (node_id, day)
	);
	CREATE INDEX IF NOT EXISTS idx_uptime_day ON node_uptime_daily(day);

	CREATE TABLE IF NOT EXISTS metric_samples (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		node_id TEXT NOT NULL,
		ts INTEGER NOT NULL,
		cpu_pct REAL NOT NULL,
		load1 REAL NOT NULL,
		mem_pct REAL NOT NULL,
		disk_pct REAL NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_samples_node_ts ON metric_samples(node_id, ts);

	CREATE TABLE IF NOT EXISTS network_samples (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		node_id TEXT NOT NULL,
		iface TEXT NOT NULL,
		ts INTEGER NOT NULL,
		rx_bytes INTEGER NOT NULL DEFAULT 0,
		tx_bytes INTEGER NOT NULL DEFAULT 0,
		rx_packets INTEGER NOT NULL DEFAULT 0,
		tx_packets INTEGER NOT NULL DEFAULT 0,
		rx_errors INTEGER NOT NULL DEFAULT 0,
		tx_errors INTEGER NOT NULL DEFAULT 0,
		rx_drops INTEGER NOT NULL DEFAULT 0,
		tx_drops INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_network_node_ts ON network_samples(node_id, ts);
	CREATE INDEX IF NOT EXISTS idx_network_node_iface_ts ON network_samples(node_id, iface, ts);

	CREATE TABLE IF NOT EXISTS probe_results (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		node_id TEXT NOT NULL,
		url TEXT NOT NULL,
		status_code INTEGER NOT NULL DEFAULT 0,
		latency_ms INTEGER NOT NULL DEFAULT 0,
		ok INTEGER NOT NULL DEFAULT 0,
		error TEXT DEFAULT '',
		ts INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_probes_url_ts ON probe_results(url, ts);
	CREATE INDEX IF NOT EXISTS idx_probes_node_ts ON probe_results(node_id, ts);

	-- metric_alert_rules: per-user threshold rules for cpu/mem/disk/load1.
	-- scope = 'node' pins to a single node_id, scope = 'fleet' matches nodes
	-- whose tags hit tag_selector (AND of comma-separated k=v pairs, or 'any:'
	-- prefix for OR). enabled = 1 is a soft-disable; rules with enabled = 0
	-- are skipped at evaluate time but stay editable. last_fired_at /
	-- last_cleared_at are bookkeeping timestamps surfaced on the operator API.
	CREATE TABLE IF NOT EXISTS metric_alert_rules (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		node_id TEXT NOT NULL DEFAULT '',
		tag_selector TEXT NOT NULL DEFAULT '',
		scope TEXT NOT NULL DEFAULT 'node',
		metric TEXT NOT NULL,
		op TEXT NOT NULL DEFAULT 'gt',
		threshold REAL NOT NULL DEFAULT 0,
		for_seconds INTEGER NOT NULL DEFAULT 0,
		severity TEXT NOT NULL DEFAULT 'warning',
		title TEXT NOT NULL DEFAULT '',
		enabled INTEGER NOT NULL DEFAULT 1,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		last_fired_at INTEGER NOT NULL DEFAULT 0,
		last_cleared_at INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_alert_rules_scope ON metric_alert_rules(scope, enabled);
	CREATE INDEX IF NOT EXISTS idx_alert_rules_user ON metric_alert_rules(user_id);
	CREATE INDEX IF NOT EXISTS idx_alert_rules_node ON metric_alert_rules(node_id);
	`
	if _, err := db.Exec(uptimeSchema); err != nil {
		return nil, err
	}

	// Forward-compat migration: pre-TCP-probe fleets have probe_results
	// tables without the `kind` column. CREATE TABLE IF NOT EXISTS
	// leaves the existing schema untouched, so we add the column and
	// the kind-keyed index in their own statements after the bulk
	// CREATE block. Tolerating "duplicate column name" / "duplicate
	// index name" lets us re-run the migration on already-upgraded
	// databases without erroring out. Note: the previous attempt put
	// both CREATE INDEX idx_probes_kind_ts and the ALTER inside the
	// bulk block, which crashed the upgrade path on any pre-existing
	// database (the index references a column the ALTER hadn't added
	// yet). Keep the index out of uptimeSchema.
	if _, err := db.Exec("ALTER TABLE probe_results ADD COLUMN kind TEXT NOT NULL DEFAULT 'http'"); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return nil, err
		}
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_probes_kind_ts ON probe_results(kind, ts)"); err != nil {
		return nil, err
	}

	wh := alerter.NewWebhook()
	return &PersistentStore{
		db:            db,
		mem:           New(),
		alerter:       alerter.New(botToken, chatID),
		webhook:       wh,
		whRecorder:    alerter.NewWebhookRecorder(wh),
		defaultChatID: chatID,
		uptime:        newUptimeTracker(),
		netRates:      newNetworkRateTracker(),
	}, nil
}

func (p *PersistentStore) Webhook() *alerter.WebhookDispatcher { return p.webhook }

// Alerter exposes the outbound user-facing dispatcher so server handlers
// (Telegram callback routing, future interactive flows) can talk to it
// without re-plumbing the constructor.
func (p *PersistentStore) Alerter() alerter.Notifier { return p.alerter }

// DefaultChatID returns the chat the dispatcher was configured with at boot.
// ponytail: read-only accessor exists so the server can pre-fill settings UI;
// if the value ever becomes per-user-only, drop this and read from user_settings directly.
func (p *PersistentStore) DefaultChatID() int64 { return p.defaultChatID }

// Ping verifies the underlying SQLite handle is reachable. Used by the
// /api/v1/ready endpoint to distinguish liveness (process up, /health)
// from readiness (DB roundtrip succeeds). Wraps *sql.DB.PingContext so
// the caller controls the deadline via ctx — a wedged DB doesn't hang
// the readiness probe forever.
func (p *PersistentStore) Ping(ctx context.Context) error {
	if p.db == nil {
		return errors.New("store: db handle is nil")
	}
	return p.db.PingContext(ctx)
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

// RegisterByTelegram creates or reuses a user bound to a Telegram user ID and
// returns a fresh API token. Returns the existing user if the tg_id is
// already linked, otherwise creates a fresh `tg_<id>` username with an
// unusable random password (Telegram users only authenticate via this path).
func (p *PersistentStore) RegisterByTelegram(tgID int64, firstName, username string) (int64, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var uid int64
	err := p.db.QueryRow("SELECT user_id FROM telegram_users WHERE tg_id = ?", tgID).Scan(&uid)
	if err == nil {
		// Existing TG user: refresh profile fields (Telegram users can rename
		// themselves, and the dashboard greeting pulls from these columns).
		// Issue a fresh token so a previous device's session can't outlive the
		// new login window.
		if firstName != "" || username != "" {
			p.db.Exec("UPDATE telegram_users SET tg_first_name = ?, tg_username = ? WHERE tg_id = ?",
				firstName, username, tgID)
		}
		token := fmt.Sprintf("np_tg_%x", sha256.Sum256([]byte(fmt.Sprintf("%d-%d", tgID, time.Now().UnixNano()))))[:36]
		p.db.Exec("INSERT INTO api_tokens (token, user_id, name) VALUES (?, ?, 'tg-login')", token, uid)
		return uid, token, nil
	}

	// New TG user: create backing user and link.
	uname := fmt.Sprintf("tg_%d", tgID)
	pwdHash := HashPassword(fmt.Sprintf("tg_%d_%d", tgID, time.Now().UnixNano()))
	res, err := p.db.Exec("INSERT INTO users (username, password_hash) VALUES (?, ?)", uname, pwdHash)
	if err != nil {
		// Username collision: extremely unlikely with `tg_<tgID>`, but handle gracefully.
		return 0, "", fmt.Errorf("username already exists")
	}
	uid, _ = res.LastInsertId()
	p.db.Exec("INSERT INTO user_settings (user_id) VALUES (?)", uid)
	p.db.Exec("INSERT INTO telegram_users (tg_id, user_id, tg_username, tg_first_name) VALUES (?, ?, ?, ?)",
		tgID, uid, username, firstName)

	token := fmt.Sprintf("np_tg_%x", sha256.Sum256([]byte(fmt.Sprintf("%d-%d", tgID, time.Now().UnixNano()))))[:36]
	p.db.Exec("INSERT INTO api_tokens (token, user_id, name) VALUES (?, ?, 'tg-login')", token, uid)
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

	// Persist the latest tag set alongside the heartbeat so the public
	// status page and tag-filter endpoint reflect the same labels without
	// joining against the in-memory store. Older agents send no Tags.
	if len(hb.Node.Tags) > 0 {
		if err := p.UpsertNodeTags(hb.NodeID, hb.Node.Tags); err != nil {
			log.Printf("node tags upsert: %v", err)
		}
	}

	if _, err := p.RecordHeartbeat(hb.NodeID, time.Now()); err != nil {
		log.Printf("uptime rollup: %v", err)
	}

	diskPct := 0.0
	if len(hb.Disks) > 0 {
		diskPct = hb.Disks[0].UsedPercent
	}
	cpuPct := 0.0
	if hb.CPU.Cores > 0 {
		// Saturate load1 against core count, then clamp to 100. Don't lie
		// above 100: dashboards key off 0..100 scale and would render the
		// spike off-chart.
		ratio := hb.CPU.Load1 / float64(hb.CPU.Cores)
		if ratio > 1.0 {
			ratio = 1.0
		}
		cpuPct = ratio * 100.0
	}
	if err := p.RecordSample(MetricSample{
		NodeID:      hb.NodeID,
		Timestamp:   hb.Timestamp,
		CPUPercent:  cpuPct,
		Load1:       hb.CPU.Load1,
		MemUsedPct:  hb.Memory.UsedPercent,
		DiskUsedPct: diskPct,
	}); err != nil {
		log.Printf("metrics sample: %v", err)
	}

	// Evaluate user-defined threshold rules against this heartbeat. The
	// evaluator owns its own state (per-rule breach start, firing
	// incident id) and routes fired alerts back through the existing
	// CreateIncident path, so telegram + webhook routing and cooldown
	// are reused without duplication. Pass the owner user id so
	// per-user rules fire on their fleet; scope=fleet also re-resolves
	// the tag selector against node_tags.
	if ownerID, _ := p.GetNodeOwner(hb.NodeID); ownerID > 0 {
		p.EvaluateMetricAlert(hb.NodeID, fmt.Sprintf("%d", ownerID), MetricSample{
			NodeID:      hb.NodeID,
			Timestamp:   hb.Timestamp,
			CPUPercent:  cpuPct,
			Load1:       hb.CPU.Load1,
			MemUsedPct:  hb.Memory.UsedPercent,
			DiskUsedPct: diskPct,
		})
	} else {
		// Admin / unbound nodes still get evaluated against scope=fleet
		// admin rules (user_id=0). Pass an empty owner so the evaluator's
		// query doesn't pretend the row belongs to a non-admin user.
		p.EvaluateMetricAlert(hb.NodeID, "", MetricSample{
			NodeID:      hb.NodeID,
			Timestamp:   hb.Timestamp,
			CPUPercent:  cpuPct,
			Load1:       hb.CPU.Load1,
			MemUsedPct:  hb.Memory.UsedPercent,
			DiskUsedPct: diskPct,
		})
	}

	if len(hb.Network) > 0 {
		samples := make([]NetworkSample, 0, len(hb.Network))
		for _, n := range hb.Network {
			samples = append(samples, NetworkSample{
				Iface:     n.Iface,
				Timestamp: hb.Timestamp,
				RxBytes:   n.RxBytes,
				TxBytes:   n.TxBytes,
				RxPackets: n.RxPackets,
				TxPackets: n.TxPackets,
				RxErrors:  n.RxErrors,
				TxErrors:  n.TxErrors,
				RxDrops:   n.RxDrops,
				TxDrops:   n.TxDrops,
			})
		}
		rates := p.RecordNetworkSamples(hb.NodeID, samples)
		p.EvaluateNetworkAlerts(hb.NodeID, rates)
	}

	if hb.Memory.UsedPercent > 92.0 {
		p.CreateIncident(hb.NodeID, "warning", "High Memory Pressure", fmt.Sprintf("RAM usage at %.1f%%", hb.Memory.UsedPercent))
	}
	for _, s := range hb.Services {
		if !s.Active && s.Type == "docker" {
			p.CreateIncident(hb.NodeID, "critical", "Container Stopped: "+s.Name, s.Status)
		}
	}

	// After creating any new incidents, sweep the open ones for this node
	// and auto-resolve the ones whose condition has cleared.
	p.ResolveStaleIncidents(hb)
}

// cooldownFor returns the minimum gap between two notifications for the same
// (node, title) incident. Critical alerts fire faster than warnings because
// each is more likely to demand an immediate action.
//
// ponytail: real values should come from per-tenant config; current defaults
// tune for ~10s agent cadence. If agents drop to 1s, halve these to avoid
// alert starvation, or add a backoff state in the incidents table.
func cooldownFor(severity string) time.Duration {
	if severity == "critical" {
		return 60 * time.Second
	}
	return 5 * time.Minute
}

func (p *PersistentStore) CreateIncident(nodeID, severity, title, detail string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	nowUnix := now.Unix()

	var openID int64
	var lastNotified int64
	var snoozedUntil int64
	err := p.db.QueryRow(
		"SELECT id, last_notified_at, COALESCE(snoozed_until, 0) FROM incidents WHERE node_id = ? AND title = ? AND resolved = 0 ORDER BY id DESC LIMIT 1",
		nodeID, title,
	).Scan(&openID, &lastNotified, &snoozedUntil)

	if err == sql.ErrNoRows {
		// No open incident: create one and notify. Pull the freshly minted
		// id back out so the dispatcher can attach inline Acknowledge /
		// Resolve buttons that route back to *this* row.
		res, ierr := p.db.Exec(
			"INSERT INTO incidents (node_id, severity, title, detail, started_at, resolved, last_notified_at) VALUES (?, ?, ?, ?, ?, 0, ?)",
			nodeID, severity, title, detail, nowUnix, nowUnix,
		)
		var incidentID string
		if ierr == nil {
			if id, idErr := res.LastInsertId(); idErr == nil {
				incidentID = fmt.Sprintf("%d", id)
			}
		}
		p.notifyAfterCreate(incidentID, nodeID, severity, title, detail, nowUnix)
		return
	}
	if err != nil {
		return
	}

	// Open incident already exists. Refresh detail (it's the latest snapshot)
	// and decide whether to re-notify.
	p.db.Exec("UPDATE incidents SET detail = ? WHERE id = ?", detail, openID)

	// Snooze wins over the cooldown gate: if the operator muted the alert,
	// we don't re-notify even if cooldown expired.
	if snoozedUntil > nowUnix {
		return
	}

	if lastNotified == 0 || nowUnix-lastNotified >= int64(cooldownFor(severity).Seconds()) {
		p.db.Exec("UPDATE incidents SET last_notified_at = ? WHERE id = ?", nowUnix, openID)
		p.notifyAfterCreate(fmt.Sprintf("%d", openID), nodeID, severity, title, detail, nowUnix)
	}
}

// notifyAfterCreate handles dispatch (Telegram + Webhook) for a freshly created
// or re-fired incident. The caller decides when this runs (always on insert,
// and on cooldown expiry for repeated crossings). incidentID is the database
// row id (base-10); empty string means no inline-keyboard buttons (path
// stays identical to the pre-buttons behaviour).
func (p *PersistentStore) notifyAfterCreate(incidentID, nodeID, severity, title, detail string, ts int64) {
	ownerID, _ := p.GetNodeOwner(nodeID)
	settings, _ := p.getSettingsLocked(ownerID)
	if settings == nil {
		return
	}
	var incidentIDNum int64
	if incidentID != "" {
		if n, err := strconv.ParseInt(incidentID, 10, 64); err == nil {
			incidentIDNum = n
		}
	}
	if severity == "critical" && !settings.NotifyCritical {
		return
	}
	if severity == "warning" && !settings.NotifyWarning {
		return
	}

	// Maintenance window short-circuit: an active silence window for this
	// owner/node suppresses the outbound notification but the incident row
	// is still recorded (for post-mortem queries). The dispatchers below
	// are skipped entirely; webhook + Telegram never see the event.
	// NOTE: must use the *_Locked variant — CreateIncident already holds
	// p.mu when it calls us, so taking the lock again would deadlock.
	if silenced, _ := p.isNodeSilencedLocked(ownerID, nodeID, ts); silenced {
		return
	}

	if p.alerter != nil {
		tgChat := p.defaultChatID
		if settings.TelegramChatID != "" {
			if parsed, err := strconv.ParseInt(settings.TelegramChatID, 10, 64); err == nil && parsed != 0 {
				tgChat = parsed
			}
		}
		// Fire synchronously: the underlying Telegram dispatcher already
		// uses a 5s-timeout HTTP client, so the worst-case cost per
		// incident is bounded. Going async here turned the test surface
		// into a race-condition minefield without buying real parallelism.
		if incidentID != "" {
			if rich, ok := p.alerter.(alerter.RichNotifier); ok {
				rich.NotifyIncidentWithButtonsTo(tgChat, incidentID, nodeID, severity, title, detail)
			} else {
				p.alerter.NotifyIncidentTo(tgChat, nodeID, severity, title, detail)
			}
		} else {
			p.alerter.NotifyIncidentTo(tgChat, nodeID, severity, title, detail)
		}
	}
	if p.webhook != nil && settings.WebhookURL != "" {
		whEvent := protocol.WebhookAlert{
			Event:     "incident.created",
			Timestamp: ts,
			Incident: &protocol.Incident{
				ID:        incidentID,
				NodeID:    nodeID,
				Severity:  severity,
				Title:     title,
				Detail:    detail,
				StartedAt: ts,
			},
		}
		// Same rationale as the alerter: webhook dispatcher does its own
		// retries/backoff with a bounded timeout, so keep the call site
		// synchronous for predictability. The recorder (when present)
		// writes the audit row in-memory; the janitor flushes it to
		// SQLite in batches.
		if p.whRecorder != nil {
			p.whRecorder.DispatchSigned(ownerID, incidentIDNum, settings.WebhookURL, settings.WebhookSecret, whEvent)
		} else {
			p.webhook.DispatchSigned(settings.WebhookURL, settings.WebhookSecret, whEvent)
		}
	}

	// Chat fanout: Slack + Discord incoming webhooks. The chat dispatcher
	// is optional; if nil (older binary, no chat configured) we simply
	// skip the block. Both URLs are short-circuited independently so a
	// Slack misconfig never blocks Discord (and vice versa). The
	// dispatcher's audit row lands in the same recorder the webhook side
	// uses, so the existing /api/v1/webhook/deliveries endpoint shows
	// all three channels in one place.
	if p.chatDispatch != nil {
		p.chatDispatch.DispatchIncident(*settings, protocol.Incident{
			ID:        incidentID,
			NodeID:    nodeID,
			Severity:  severity,
			Title:     title,
			Detail:    detail,
			StartedAt: ts,
		})
	}
}

// NotifyChatResolved is the public surface used by the auto-resolve
// path to fan a green-tick message out to Slack/Discord. Mirrors the
// fanout block in notifyAfterCreate but takes the incident row directly
// so the resolved path doesn't need to re-fetch.
func (p *PersistentStore) NotifyChatResolved(inc protocol.Incident) {
	if p.chatDispatch == nil {
		return
	}
	ownerID, _ := p.GetNodeOwner(inc.NodeID)
	settings, _ := p.getSettingsLocked(ownerID)
	if settings == nil {
		return
	}
	p.chatDispatch.DispatchResolved(*settings, inc)
}

// ResolveIncident marks an incident resolved. Returns ErrIncidentNotOwned when
// the incident exists but the user does not own the underlying node — callers
// must map that to HTTP 403 instead of a generic 500. The resolution is
// tagged with reason="manual" so audit timelines can distinguish operator
// action from auto-resolution paths.
func (p *PersistentStore) ResolveIncident(id string, userID int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	res, err := p.db.Exec(`
		UPDATE incidents
		   SET resolved = 1, resolved_at = ?, resolution_reason = 'manual'
		 WHERE id = ?
		   AND (
		    ? = 1
		    OR id IN (
		        SELECT i.id FROM incidents i
		        JOIN node_owners o ON o.node_id = i.node_id
		        WHERE o.user_id = ?
		    )
		   )`, time.Now().Unix(), id, userID, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// distinguish missing from forbidden: cheaper than an extra SELECT
		var exists int
		_ = p.db.QueryRow("SELECT 1 FROM incidents WHERE id = ?", id).Scan(&exists)
		if exists == 1 {
			return ErrIncidentForbidden
		}
		return ErrIncidentNotFound
	}
	return nil
}

var (
	ErrIncidentForbidden = fmt.Errorf("incident does not belong to user")
	ErrIncidentNotFound  = fmt.Errorf("incident not found")
)

func (p *PersistentStore) GetActiveIncidents(userID int64) []protocol.Incident {
	query := "SELECT id, node_id, severity, title, detail, started_at, resolved, acknowledged_at, last_notified_at FROM incidents WHERE resolved = 0 ORDER BY started_at DESC LIMIT 50"
	if userID > 1 {
		query = fmt.Sprintf(`SELECT i.id, i.node_id, i.severity, i.title, i.detail, i.started_at, i.resolved, i.acknowledged_at, i.last_notified_at
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
		if err := rows.Scan(&id, &inc.NodeID, &inc.Severity, &inc.Title, &inc.Detail, &inc.StartedAt, &res, &inc.AcknowledgedAt, &inc.LastNotifiedAt); err == nil {
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

// RecordAutoHealLogs persists a batch of agent-side auto-heal attempts.
// We accept either ingest tokens or the master token. Logs older than 24h
// are pruned at insert time so the table stays bounded.
//
// ponytail: storage is inlined; if the rate of auto-heal attempts ever
// exceeds ~50k/day across the fleet, swap the in-table storage for a
// rolling windowed on-disk log file per node and drop this method.
func (p *PersistentStore) RecordAutoHealLogs(nodeID string, _ int64, events []protocol.AutoHealLog) {
	if len(events) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	tx, err := p.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("INSERT INTO autoheal_logs (node_id, command, status, reason, error, ts) VALUES (?, ?, ?, ?, ?, ?)")
	if err != nil {
		return
	}
	defer stmt.Close()
	for _, ev := range events {
		if ev.Command == "" {
			continue
		}
		if _, err := stmt.Exec(nodeID, ev.Command, ev.Status, ev.Reason, ev.Error, ev.Ts); err != nil {
			continue
		}
	}
	tx.Commit()

	cutoff := time.Now().Add(-24 * time.Hour).Unix()
	p.db.Exec("DELETE FROM autoheal_logs WHERE ts < ?", cutoff)
}

// RecentAutoHealLogs returns the last N auto-heal attempts for a node, newest first.
func (p *PersistentStore) RecentAutoHealLogs(nodeID string, limit int) []protocol.AutoHealLog {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := p.db.Query(
		"SELECT command, status, reason, error, ts FROM autoheal_logs WHERE node_id = ? ORDER BY id DESC LIMIT ?",
		nodeID, limit,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []protocol.AutoHealLog
	for rows.Next() {
		var ev protocol.AutoHealLog
		if err := rows.Scan(&ev.Command, &ev.Status, &ev.Reason, &ev.Error, &ev.Ts); err == nil {
			out = append(out, ev)
		}
	}
	return out
}
