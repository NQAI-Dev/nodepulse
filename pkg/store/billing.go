package store

func (p *PersistentStore) InitBillingSchema() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS invoices (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			invoice_id TEXT UNIQUE NOT NULL,
			user_id INTEGER NOT NULL,
			plan TEXT NOT NULL,
			amount TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'created',
			pay_url TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			paid_at DATETIME
		)`,
		`ALTER TABLE users ADD COLUMN plan TEXT DEFAULT 'free'`,
		`ALTER TABLE users ADD COLUMN pro_until DATETIME`,
	}
	for _, q := range queries {
		p.db.Exec(q)
	}
	return nil
}

func (p *PersistentStore) SaveInvoice(invoiceID string, userID int64, plan, amount, payURL string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.db.Exec("INSERT INTO invoices (invoice_id, user_id, plan, amount, pay_url, status) VALUES (?, ?, ?, ?, ?, 'created')",
		invoiceID, userID, plan, amount, payURL)
	return err
}

func (p *PersistentStore) MarkInvoicePaid(invoiceID string) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var userID int64
	var plan string
	err := p.db.QueryRow("SELECT user_id, plan FROM invoices WHERE invoice_id = ?", invoiceID).Scan(&userID, &plan)
	if err != nil {
		return 0, err
	}

	p.db.Exec("UPDATE invoices SET status = 'paid', paid_at = CURRENT_TIMESTAMP WHERE invoice_id = ?", invoiceID)
	p.db.Exec("UPDATE users SET plan = 'pro', pro_until = datetime('now', '+30 days') WHERE id = ?", userID)
	return userID, nil
}

func (p *PersistentStore) GetUserPlan(userID int64) (string, error) {
	var plan string
	err := p.db.QueryRow("SELECT COALESCE(plan, 'free') FROM users WHERE id = ?", userID).Scan(&plan)
	if err != nil {
		return "free", err
	}
	return plan, nil
}

// Plan limits. Constants are kept here (not in a config) because they are
// product-policy — the public status page and the billing/plan API expose
// them verbatim, so changing a limit is a deliberate code review.
const (
	FreeNodeLimit    = 3
	FreeProbeLimit   = 5
	FreeRetentionDay = 7
)

func (p *PersistentStore) CanAddNode(userID int64) bool {
	if userID == 1 {
		return true
	}
	if isPro(p, userID) {
		return true
	}
	return p.userNodeCount(userID) < FreeNodeLimit
}

// CanAddProbe enforces the per-user synthetic-probe URL budget. We count
// distinct URLs (not row inserts) so a noisy probe doesn't exhaust the
// quota by itself. Returns true when the user can ship at least one more
// unique URL. The ingest handler still records results for already-known
// URLs even when the limit is reached, so we never drop data — we just
// reject new targets.
func (p *PersistentStore) CanAddProbe(userID int64, urls []string) bool {
	if userID == 1 || len(urls) == 0 {
		return true
	}
	if isPro(p, userID) {
		return true
	}
	existing, _ := p.userProbeURLCount(userID)
	for _, u := range urls {
		var seen int
		// Cheap URL existence check scoped to this user's nodes. If the
		// URL is already known, no quota is consumed; only NEW URLs cost.
		p.db.QueryRow(`SELECT COUNT(*) FROM probe_results pr
			JOIN node_owners o ON o.node_id = pr.node_id
			WHERE o.user_id = ? AND pr.url = ? LIMIT 1`, userID, u).Scan(&seen)
		if seen == 0 {
			existing++
			if existing > FreeProbeLimit {
				return false
			}
		}
	}
	return true
}

// userNodeCount returns how many nodes this user currently owns.
func (p *PersistentStore) userNodeCount(userID int64) int {
	var n int
	p.db.QueryRow("SELECT COUNT(*) FROM node_owners WHERE user_id = ?", userID).Scan(&n)
	return n
}

// userProbeURLCount returns the distinct probe URL count across this user's fleet.
func (p *PersistentStore) userProbeURLCount(userID int64) (int, error) {
	var n int
	err := p.db.QueryRow(`SELECT COUNT(DISTINCT pr.url) FROM probe_results pr
		JOIN node_owners o ON o.node_id = pr.node_id
		WHERE o.user_id = ?`, userID).Scan(&n)
	return n, err
}

// PlanUsage is the snapshot returned by /billing/plan. Counts are computed
// live (no caching) so the UI can show "2 of 3 nodes" without polling the
// nodes list. Limits come from the package constants so they never drift
// between code paths.
type PlanUsage struct {
	UserID       int64  `json:"user_id"`
	Plan         string `json:"plan"`
	ProUntil     string `json:"pro_until,omitempty"`
	NodesUsed    int    `json:"nodes_used"`
	NodesLimit   int    `json:"nodes_limit"`
	ProbesUsed   int    `json:"probes_used"`
	ProbesLimit  int    `json:"probes_limit"`
	RetentionDay int    `json:"retention_days"`
	IsPro        bool   `json:"is_pro"`
}

func (p *PersistentStore) GetPlanUsage(userID int64) (PlanUsage, error) {
	u := PlanUsage{UserID: userID, Plan: "free"}
	plan, _ := p.GetUserPlan(userID)
	u.Plan = plan
	u.IsPro = plan == "pro"
	if u.IsPro {
		var until string
		p.db.QueryRow("SELECT COALESCE(pro_until, '') FROM users WHERE id = ?", userID).Scan(&until)
		u.ProUntil = until
		u.NodesLimit = -1 // unlimited
		u.ProbesLimit = -1
		u.RetentionDay = 90
	} else {
		u.NodesLimit = FreeNodeLimit
		u.ProbesLimit = FreeProbeLimit
		u.RetentionDay = FreeRetentionDay
	}
	u.NodesUsed = p.userNodeCount(userID)
	u.ProbesUsed, _ = p.userProbeURLCount(userID)
	return u, nil
}

// isPro is a tiny helper that ignores the error from GetUserPlan. The
// billing schema initializes plan='free' for every user, so a missing row
// or driver hiccup falls back to "not pro" — which is the safe default
// (limits stay enforced instead of being silently bypassed).
func isPro(p *PersistentStore, userID int64) bool {
	plan, _ := p.GetUserPlan(userID)
	return plan == "pro"
}
