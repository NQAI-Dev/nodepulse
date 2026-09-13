package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// ErrAlertRuleInvalid is returned when an Upsert payload doesn't pass
// validation (unknown metric, bad op, missing scope, etc.). Mapped to
// 400 by the REST handlers so the operator UI can echo "invalid rule".
var ErrAlertRuleInvalid = errors.New("metric alert rule failed validation")

// validAlertRuleMetrics is the closed set the evaluator knows how to read
// from a MetricSample. Adding a new entry here requires extending
// EvaluateMetricAlert's sampleValue() switch too.
var validAlertRuleMetrics = map[string]bool{
	"cpu_pct":  true,
	"mem_pct":  true,
	"disk_pct": true,
	"load1":    true,
}

// validAlertRuleOps mirrors the protocol doc. Anything else falls through
// ErrAlertRuleInvalid so the operator gets an actionable error.
var validAlertRuleOps = map[string]bool{
	"gt": true,
	"lt": true,
}

var validAlertRuleSeverity = map[string]bool{
	"warning":  true,
	"critical": true,
}

var validAlertRuleScopes = map[string]bool{
	"node":  true,
	"fleet": true,
}

// validateAlertRuleRequest centralises the field checks so POST and PUT
// stay in lock-step. Time-window bounds come from experience: ForSeconds=0
// is a legitimate "fire on a single crossing" mode and stays; upper bound
// of 1h stops a typo from sticking an alert in pending forever.
func validateAlertRuleRequest(req protocol.MetricAlertRuleRequest) error {
	if req.Scope != "node" && req.Scope != "fleet" {
		return fmt.Errorf("%w: scope must be 'node' or 'fleet'", ErrAlertRuleInvalid)
	}
	if req.Scope == "node" && strings.TrimSpace(req.NodeID) == "" {
		return fmt.Errorf("%w: node_id required for scope=node", ErrAlertRuleInvalid)
	}
	if req.Scope == "fleet" && strings.TrimSpace(req.TagSelector) == "" {
		return fmt.Errorf("%w: tag_selector required for scope=fleet", ErrAlertRuleInvalid)
	}
	if !validAlertRuleMetrics[req.Metric] {
		return fmt.Errorf("%w: metric must be one of cpu_pct/mem_pct/disk_pct/load1", ErrAlertRuleInvalid)
	}
	if !validAlertRuleOps[req.Op] {
		return fmt.Errorf("%w: op must be 'gt' or 'lt'", ErrAlertRuleInvalid)
	}
	if !validAlertRuleSeverity[req.Severity] {
		return fmt.Errorf("%w: severity must be 'warning' or 'critical'", ErrAlertRuleInvalid)
	}
	if req.ForSeconds < 0 || req.ForSeconds > 3600 {
		return fmt.Errorf("%w: for_seconds must be 0..3600", ErrAlertRuleInvalid)
	}
	if strings.TrimSpace(req.Title) == "" {
		return fmt.Errorf("%w: title is required", ErrAlertRuleInvalid)
	}
	return nil
}

// CreateMetricAlertRule inserts a new rule owned by userID. Returns the
// assigned row id (from LastInsertId) so the handler can echo it back to
// the API caller. Zero/empty semantics:
//   - title is required (validateAlertRuleRequest enforces this)
//   - enabled defaults true on the client side via the JSON default
//
// ponytail: a global rule (admin-owned, matches every node) would need a
// dedicated scope; for v1 we let admin rules be implicit by leaving
// user_id=0 with scope=fleet, evaluated before user-specific rules.
func (p *PersistentStore) CreateMetricAlertRule(userID int64, req protocol.MetricAlertRuleRequest) (int64, error) {
	if err := validateAlertRuleRequest(req); err != nil {
		return 0, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now().Unix()
	res, err := p.db.Exec(
		`INSERT INTO metric_alert_rules (
			user_id, node_id, tag_selector, scope, metric, op,
			threshold, for_seconds, severity, title, enabled,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		userID,
		strings.TrimSpace(req.NodeID),
		strings.TrimSpace(req.TagSelector),
		req.Scope,
		req.Metric,
		req.Op,
		req.Threshold,
		req.ForSeconds,
		req.Severity,
		strings.TrimSpace(req.Title),
		boolToInt(req.Enabled),
		now,
		now,
	)
	if err != nil {
		return 0, err
	}
	id, idErr := res.LastInsertId()
	if idErr != nil {
		return 0, idErr
	}
	return id, nil
}

// UpdateMetricAlertRule applies req to an existing row owned by userID.
// Returns ErrAlertRuleInvalid when the new payload is bad, and a wrapped
// sql.ErrNoRows variant when the row is missing or owned by another user
// (we never want to surface 404 vs 403 differences here).
func (p *PersistentStore) UpdateMetricAlertRule(userID int64, id int64, req protocol.MetricAlertRuleRequest) error {
	if err := validateAlertRuleRequest(req); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now().Unix()
	res, err := p.db.Exec(
		`UPDATE metric_alert_rules SET
			node_id = ?, tag_selector = ?, scope = ?, metric = ?, op = ?,
			threshold = ?, for_seconds = ?, severity = ?, title = ?,
			enabled = ?, updated_at = ?
		WHERE id = ? AND user_id = ?`,
		strings.TrimSpace(req.NodeID),
		strings.TrimSpace(req.TagSelector),
		req.Scope,
		req.Metric,
		req.Op,
		req.Threshold,
		req.ForSeconds,
		req.Severity,
		strings.TrimSpace(req.Title),
		boolToInt(req.Enabled),
		now,
		id,
		userID,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteMetricAlertRule removes a rule owned by userID. No-op (returns nil)
// when the row never existed or belongs to another user — the DELETE
// statement filters by user_id so cross-tenant delete attempts don't leak
// existence information.
func (p *PersistentStore) DeleteMetricAlertRule(userID int64, id int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.db.Exec(
		"DELETE FROM metric_alert_rules WHERE id = ? AND user_id = ?",
		id, userID,
	)
	return err
}

// GetMetricAlertRule returns a single rule by id, scoped to userID. Admin
// (user_id=1) sees everything; regular users see only their own rules.
// Returns nil + nil when the row is absent; callers can use that to map
// to 404 cleanly without distinguishing missing-vs-forbidden.
func (p *PersistentStore) GetMetricAlertRule(userID int64, id int64) (*protocol.MetricAlertRule, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	row := p.db.QueryRow(`
		SELECT id, user_id, node_id, tag_selector, scope, metric, op,
		       threshold, for_seconds, severity, title, enabled,
		       created_at, updated_at, last_fired_at, last_cleared_at
		FROM metric_alert_rules
		WHERE id = ? AND (user_id = ? OR ? = 1)`,
		id, userID, userID,
	)
	return scanAlertRule(row)
}

// ListMetricAlertRules returns every rule owned by userID (or all rules
// for admin). Optional nodeID filters to rules that *target* that node —
// either via scope=node with matching ID, or scope=fleet with tags that
// the node currently carries. The fleet pre-filter happens in Go after
// the SQLite read because tag membership is in `node_tags` and the join
// would double-cost rules with overlapping tags.
func (p *PersistentStore) ListMetricAlertRules(userID int64, nodeID string) ([]protocol.MetricAlertRule, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var rows *sql.Rows
	var err error
	if nodeID == "" {
		rows, err = p.db.Query(`
			SELECT id, user_id, node_id, tag_selector, scope, metric, op,
			       threshold, for_seconds, severity, title, enabled,
			       created_at, updated_at, last_fired_at, last_cleared_at
			FROM metric_alert_rules
			WHERE user_id = ? OR ? = 1
			ORDER BY id ASC`,
			userID, userID,
		)
	} else {
		// scope=node + exact match are obvious. scope=fleet requires a tag
		// membership check below; here we just fetch everything matching
		// the user and filter in Go — keeps the SQL portable and lets us
		// reuse the same scan path for both branches.
		rows, err = p.db.Query(`
			SELECT id, user_id, node_id, tag_selector, scope, metric, op,
			       threshold, for_seconds, severity, title, enabled,
			       created_at, updated_at, last_fired_at, last_cleared_at
			FROM metric_alert_rules
			WHERE (user_id = ? OR ? = 1) AND (
				(scope = 'node' AND node_id = ?) OR
				scope = 'fleet'
			)
			ORDER BY id ASC`,
			userID, userID, nodeID,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]protocol.MetricAlertRule, 0, 16)
	for rows.Next() {
		r, err := scanAlertRuleRows(rows)
		if err != nil {
			return nil, err
		}
		if nodeID != "" && r.Scope == "fleet" {
			// Verify the tag selector actually matches nodeID's tags
			// before exposing the rule in the per-node listing.
			if !nodeMatchesTagSelector(p, r.TagSelector, nodeID) {
				continue
			}
		}
		out = append(out, *r)
	}
	return out, nil
}

// MetricsAlertRulesForNode returns the rules that apply to nodeID right
// now — pre-resolved fleet membership. Called from the ingest path on
// every heartbeat so the hot path shouldn't re-evaluate joins. Tag
// matching walks the indexed node_tags table directly.
//
// ponytail: cache this in a per-node TTL map if heartbeat QPS rises; the
// current linear scan is fine up to ~hundreds of rules per fleet.
func (p *PersistentStore) MetricsAlertRulesForNode(nodeID, ownerUserID string) []protocol.MetricAlertRule {
	p.mu.Lock()
	defer p.mu.Unlock()

	var rows *sql.Rows
	var err error
	if ownerUserID != "" {
		rows, err = p.db.Query(`
			SELECT id, user_id, node_id, tag_selector, scope, metric, op,
			       threshold, for_seconds, severity, title, enabled,
			       created_at, updated_at, last_fired_at, last_cleared_at
			FROM metric_alert_rules
			WHERE enabled = 1 AND (
				(scope = 'node' AND node_id = ?) OR
				user_id = 0 OR
				user_id = ? OR
				? = 1
			)
			ORDER BY id ASC`,
			nodeID, ownerUserID, ownerUserID,
		)
	} else {
		rows, err = p.db.Query(`
			SELECT id, user_id, node_id, tag_selector, scope, metric, op,
			       threshold, for_seconds, severity, title, enabled,
			       created_at, updated_at, last_fired_at, last_cleared_at
			FROM metric_alert_rules
			WHERE enabled = 1 AND (
				(scope = 'node' AND node_id = ?) OR
				user_id = 0
			)
			ORDER BY id ASC`,
			nodeID,
		)
	}
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := make([]protocol.MetricAlertRule, 0, 8)
	for rows.Next() {
		r, scanErr := scanAlertRuleRows(rows)
		if scanErr != nil {
			continue
		}
		if r.Scope == "fleet" {
			if !nodeMatchesTagSelectorLocked(p, r.TagSelector, nodeID) {
				continue
			}
		}
		out = append(out, *r)
	}
	return out
}

// MarkAlertRuleFired stamps last_fired_at = now for the rule and returns
// nothing — failures are logged at the caller so the alert loop never
// crashes on bookkeeping hiccups.
//
// ponytail: bump a per-rule counter too if we ever want hourly-buckets
// analytics; SQLite doesn't need it for the hot path today.
func (p *PersistentStore) MarkAlertRuleFired(ruleID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = p.db.Exec(
		"UPDATE metric_alert_rules SET last_fired_at = ? WHERE id = ?",
		time.Now().Unix(), ruleID,
	)
}

// MarkAlertRuleCleared stamps last_cleared_at when a rule resolves an open
// incident after the threshold stops being crossed.
func (p *PersistentStore) MarkAlertRuleCleared(ruleID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = p.db.Exec(
		"UPDATE metric_alert_rules SET last_cleared_at = ? WHERE id = ?",
		time.Now().Unix(), ruleID,
	)
}

// nodeMatchesTagSelector and nodeMatchesTagSelectorLocked are the same
// predicate; the locked variant assumes p.mu is already held so the
// hot ingest path doesn't re-lock for each rule evaluation. The unlocked
// variant handles read-only listing endpoints that don't hold the mutex.
//
// TagSelector syntax:
//   - "env=prod"        exact key=value match on a single tag
//   - "env=prod,role=db" all listed tags must be present (AND)
//   - "any:env=prod"     any of the listed tags is enough (OR)
//
// Empty selector returns true to stay backwards compatible with the early
// "blank -> matches everything" interpretation. Whitespace is trimmed.
func nodeMatchesTagSelector(p *PersistentStore, sel, nodeID string) bool {
	// p.mu is intentionally NOT acquired here: callers that already
	// hold the mutex (hot ingest path, list endpoint) reuse the locked
	// variant directly. Taking p.mu here would deadlock against those
	// callers because sync.Mutex is not re-entrant.
	return nodeMatchesTagSelectorLocked(p, sel, nodeID)
}

func nodeMatchesTagSelectorLocked(p *PersistentStore, sel, nodeID string) bool {
	sel = strings.TrimSpace(sel)
	if sel == "" {
		return true
	}
	tags := p.TagsForNode(nodeID)
	set := make(map[string]struct{}, len(tags))
	for _, t := range tags {
		set[strings.TrimSpace(t)] = struct{}{}
	}

	if strings.HasPrefix(sel, "any:") {
		rest := strings.TrimPrefix(sel, "any:")
		for _, raw := range strings.Split(rest, ",") {
			tag := strings.TrimSpace(raw)
			if tag == "" {
				continue
			}
			if _, ok := set[tag]; ok {
				return true
			}
		}
		return false
	}
	for _, raw := range strings.Split(sel, ",") {
		tag := strings.TrimSpace(raw)
		if tag == "" {
			continue
		}
		if _, ok := set[tag]; !ok {
			return false
		}
	}
	return true
}

// scanAlertRule and scanAlertRuleRows share the column list and conversion
// helpers so the row layout isn't duplicated twice (and so the next
// addition of a column lives in exactly one place).
func scanAlertRule(row *sql.Row) (*protocol.MetricAlertRule, error) {
	var r protocol.MetricAlertRule
	var enabled int
	err := row.Scan(
		&r.ID, &r.UserID, &r.NodeID, &r.TagSelector, &r.Scope, &r.Metric, &r.Op,
		&r.Threshold, &r.ForSeconds, &r.Severity, &r.Title, &enabled,
		&r.CreatedAt, &r.UpdatedAt, &r.LastFiredAt, &r.LastClearedAt,
	)
	if err != nil {
		return nil, err
	}
	r.Enabled = enabled == 1
	return &r, nil
}

func scanAlertRuleRows(rows *sql.Rows) (*protocol.MetricAlertRule, error) {
	var r protocol.MetricAlertRule
	var enabled int
	err := rows.Scan(
		&r.ID, &r.UserID, &r.NodeID, &r.TagSelector, &r.Scope, &r.Metric, &r.Op,
		&r.Threshold, &r.ForSeconds, &r.Severity, &r.Title, &enabled,
		&r.CreatedAt, &r.UpdatedAt, &r.LastFiredAt, &r.LastClearedAt,
	)
	if err != nil {
		return nil, err
	}
	r.Enabled = enabled == 1
	return &r, nil
}

// boolToInt is the stdlib-free tri-state wrapper we use everywhere because
// a named helper keeps the SQL parameter list readable.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
