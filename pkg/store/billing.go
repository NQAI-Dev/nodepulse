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

func (p *PersistentStore) CanAddNode(userID int64) bool {
	if userID == 1 {
		return true
	}
	plan, _ := p.GetUserPlan(userID)
	if plan == "pro" {
		return true
	}
	var count int
	p.db.QueryRow("SELECT COUNT(*) FROM node_owners WHERE user_id = ?", userID).Scan(&count)
	return count < 3
}
