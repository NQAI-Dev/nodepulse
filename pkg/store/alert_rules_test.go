package store

import (
	"path/filepath"
	"testing"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func newAlertRuleTestStore(t *testing.T) *PersistentStore {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "alert.db")
	p, err := NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}
	t.Cleanup(func() { p.db.Close() })
	return p
}

// TestCreateMetricAlertRule_BasicScopeNode exercises the happy path for a
// node-scoped rule and confirms the returned id round-trips through a
// follow-up ListMetricAlertRules.
func TestCreateMetricAlertRule_BasicScopeNode(t *testing.T) {
	p := newAlertRuleTestStore(t)

	id, err := p.CreateMetricAlertRule(7, protocol.MetricAlertRuleRequest{
		NodeID:     "node-a",
		Scope:      "node",
		Metric:     "cpu_pct",
		Op:         "gt",
		Threshold:  85.0,
		ForSeconds: 60,
		Severity:   "warning",
		Title:      "High CPU on node-a",
		Enabled:    true,
	})
	if err != nil {
		t.Fatalf("CreateMetricAlertRule: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive id, got %d", id)
	}

	rules, err := p.ListMetricAlertRules(7, "")
	if err != nil {
		t.Fatalf("ListMetricAlertRules: %v", err)
	}
	if len(rules) != 1 || rules[0].ID != id {
		t.Fatalf("expected one rule with id=%d, got %#v", id, rules)
	}
	if rules[0].UserID != 7 {
		t.Fatalf("user_id lost: got %d", rules[0].UserID)
	}
	if rules[0].Metric != "cpu_pct" || rules[0].Op != "gt" || rules[0].Threshold != 85.0 {
		t.Fatalf("rule fields wrong: %+v", rules[0])
	}
	if !rules[0].Enabled {
		t.Fatalf("enabled bit lost: %+v", rules[0])
	}
}

// TestCreateMetricAlertRule_RejectsBadInputs walks every documented
// validation gate so the field-by-field checks don't regress silently
// when the protocol struct grows new fields.
func TestCreateMetricAlertRule_RejectsBadInputs(t *testing.T) {
	p := newAlertRuleTestStore(t)

	base := protocol.MetricAlertRuleRequest{
		NodeID:     "n",
		Scope:      "node",
		Metric:     "cpu_pct",
		Op:         "gt",
		Threshold:  1,
		ForSeconds: 0,
		Severity:   "warning",
		Title:      "x",
		Enabled:    true,
	}

	cases := []struct {
		name string
		mut  func(*protocol.MetricAlertRuleRequest)
	}{
		{"bad_scope", func(r *protocol.MetricAlertRuleRequest) { r.Scope = "all" }},
		{"node_scope_missing_id", func(r *protocol.MetricAlertRuleRequest) {
			r.Scope = "node"
			r.NodeID = ""
		}},
		{"fleet_scope_missing_selector", func(r *protocol.MetricAlertRuleRequest) {
			r.Scope = "fleet"
			r.TagSelector = ""
		}},
		{"bad_metric", func(r *protocol.MetricAlertRuleRequest) { r.Metric = "watts" }},
		{"bad_op", func(r *protocol.MetricAlertRuleRequest) { r.Op = "ge" }},
		{"bad_severity", func(r *protocol.MetricAlertRuleRequest) { r.Severity = "info" }},
		{"for_seconds_overflow", func(r *protocol.MetricAlertRuleRequest) { r.ForSeconds = 99999 }},
		{"blank_title", func(r *protocol.MetricAlertRuleRequest) { r.Title = "   " }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := base
			c.mut(&req)
			if _, err := p.CreateMetricAlertRule(1, req); err == nil {
				t.Fatalf("expected validation error for %s", c.name)
			}
		})
	}
}

// TestUpdateMetricAlertRule_OwnerScoped ensures cross-tenant updates fail
// silently (sql.ErrNoRows bubbling up as 404 in the handler) so the
// evaluator can't be tricked into editing another user's rule.
func TestUpdateMetricAlertRule_OwnerScoped(t *testing.T) {
	p := newAlertRuleTestStore(t)
	id, err := p.CreateMetricAlertRule(11, protocol.MetricAlertRuleRequest{
		NodeID: "n", Scope: "node", Metric: "mem_pct", Op: "gt",
		Threshold: 90, ForSeconds: 30, Severity: "critical",
		Title: "OOM imminent", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Owner update succeeds.
	if err := p.UpdateMetricAlertRule(11, id, protocol.MetricAlertRuleRequest{
		NodeID: "n", Scope: "node", Metric: "mem_pct", Op: "gt",
		Threshold: 95, ForSeconds: 30, Severity: "critical",
		Title: "OOM imminent", Enabled: true,
	}); err != nil {
		t.Fatalf("owner update: %v", err)
	}

	// Stranger's update is rejected.
	err = p.UpdateMetricAlertRule(99, id, protocol.MetricAlertRuleRequest{
		NodeID: "n", Scope: "node", Metric: "mem_pct", Op: "gt",
		Threshold: 50, ForSeconds: 30, Severity: "critical",
		Title: "pwn", Enabled: true,
	})
	if err == nil {
		t.Fatalf("expected cross-tenant update to fail")
	}

	// Confirm threshold stayed at the owner's last setting.
	rules, _ := p.ListMetricAlertRules(11, "")
	if len(rules) != 1 || rules[0].Threshold != 95 {
		t.Fatalf("threshold unexpectedly modified: %+v", rules)
	}
}

// TestDeleteMetricAlertRule_OwnerScoped mirrors the Update test: delete by
// the wrong owner is a no-op (no error so the REST handler doesn't leak
// existence), the row stays alive for the real owner.
func TestDeleteMetricAlertRule_OwnerScoped(t *testing.T) {
	p := newAlertRuleTestStore(t)
	id, err := p.CreateMetricAlertRule(22, protocol.MetricAlertRuleRequest{
		NodeID: "n", Scope: "node", Metric: "disk_pct", Op: "gt",
		Threshold: 85, ForSeconds: 0, Severity: "warning",
		Title: "Disk filling up", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := p.DeleteMetricAlertRule(99, id); err != nil {
		t.Fatalf("stranger delete should be silent, got %v", err)
	}
	if err := p.DeleteMetricAlertRule(22, id); err != nil {
		t.Fatalf("owner delete should succeed: %v", err)
	}
	rules, _ := p.ListMetricAlertRules(22, "")
	if len(rules) != 0 {
		t.Fatalf("expected empty list, got %d rules", len(rules))
	}
}

// TestListMetricAlertRules_NodeFilter_DropsFleetWithoutTag confirms the
// per-node filter doesn't expose a fleet-scoped rule to a node whose
// tags don't satisfy the selector.
func TestListMetricAlertRules_NodeFilter_DropsFleetWithoutTag(t *testing.T) {
	p := newAlertRuleTestStore(t)
	if _, err := p.CreateMetricAlertRule(33, protocol.MetricAlertRuleRequest{
		NodeID: "explicit", Scope: "node", Metric: "cpu_pct", Op: "gt",
		Threshold: 80, ForSeconds: 0, Severity: "warning",
		Title: "explicit node", Enabled: true,
	}); err != nil {
		t.Fatalf("create node rule: %v", err)
	}
	if _, err := p.CreateMetricAlertRule(33, protocol.MetricAlertRuleRequest{
		TagSelector: "env=prod,role=db",
		Scope:       "fleet", Metric: "mem_pct", Op: "gt",
		Threshold: 90, ForSeconds: 0, Severity: "critical",
		Title: "tag rule", Enabled: true,
	}); err != nil {
		t.Fatalf("create fleet rule: %v", err)
	}

	// Node with no tags → fleet rule filtered out.
	rules, _ := p.ListMetricAlertRules(33, "explicit")
	if len(rules) != 1 || rules[0].Scope != "node" {
		t.Fatalf("expected only node rule, got %+v", rules)
	}

	// Tagged node `db-1` is *not* the explicit rule's target, so only the
	// fleet rule applies; it must pass the tag selector and reach the
	// listing. Adding the explicit rule on db-1 itself would change the
	// count, but that's covered by the dedicated cross-tenant test.
	if err := p.UpsertNodeTags("db-1", []string{"env=prod", "role=db"}); err != nil {
		t.Fatalf("UpsertNodeTags: %v", err)
	}
	rules, _ = p.ListMetricAlertRules(33, "db-1")
	if len(rules) != 1 || rules[0].Scope != "fleet" || rules[0].Title != "tag rule" {
		t.Fatalf("expected fleet rule only, got %+v", rules)
	}
}

// TestMetricsAlertRulesForNode_TagMatching covers the hot-path lookup that
// runs from the ingest handler on every heartbeat, including the AND
// semantics and the `any:` prefix.
func TestMetricsAlertRulesForNode_TagMatching(t *testing.T) {
	p := newAlertRuleTestStore(t)

	// AND rule.
	if _, err := p.CreateMetricAlertRule(44, protocol.MetricAlertRuleRequest{
		TagSelector: "env=prod,role=db",
		Scope:       "fleet", Metric: "load1", Op: "gt",
		Threshold: 4, ForSeconds: 0, Severity: "warning",
		Title: "db load", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	// OR rule.
	if _, err := p.CreateMetricAlertRule(44, protocol.MetricAlertRuleRequest{
		TagSelector: "any:role=cache,role=db",
		Scope:       "fleet", Metric: "mem_pct", Op: "gt",
		Threshold: 80, ForSeconds: 0, Severity: "warning",
		Title: "memory", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	// Disabled rule — must be excluded even if tags match.
	if _, err := p.CreateMetricAlertRule(44, protocol.MetricAlertRuleRequest{
		NodeID: "explicit", Scope: "node", Metric: "cpu_pct", Op: "gt",
		Threshold: 99, ForSeconds: 0, Severity: "warning",
		Title: "off", Enabled: false,
	}); err != nil {
		t.Fatal(err)
	}

	if err := p.UpsertNodeTags("db-1", []string{"env=prod", "role=db"}); err != nil {
		t.Fatal(err)
	}

	rules := p.MetricsAlertRulesForNode("db-1", "44")
	if len(rules) != 2 {
		t.Fatalf("expected 2 enabled rules for db-1, got %d", len(rules))
	}

	// Node with only env=prod → AND miss, OR miss, owner scope miss.
	if err := p.UpsertNodeTags("web-1", []string{"env=prod"}); err != nil {
		t.Fatal(err)
	}
	rules = p.MetricsAlertRulesForNode("web-1", "44")
	if len(rules) != 0 {
		t.Fatalf("expected 0 enabled rules for web-1, got %+v", rules)
	}

	// Cache node → only OR rule matches.
	if err := p.UpsertNodeTags("cache-1", []string{"role=cache"}); err != nil {
		t.Fatal(err)
	}
	rules = p.MetricsAlertRulesForNode("cache-1", "44")
	if len(rules) != 1 || rules[0].Title != "memory" {
		t.Fatalf("expected only OR rule, got %+v", rules)
	}
}
