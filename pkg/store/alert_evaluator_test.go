package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/alerter"
	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func newAlertEvalTestStore(t *testing.T) (*PersistentStore, *alerter.RecordingNotifier) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "alert-eval.db")
	p, err := NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}
	t.Cleanup(func() { p.db.Close() })

	// Replace the real alerter with a recording stub so we can assert
	// on dispatch without touching Telegram.
	rec := &alerter.RecordingNotifier{}
	p.SetNotifier(rec)
	return p, rec
}

// TestEvaluateMetricAlert_FiresOnSustainedBreach covers the happy path:
// the first sample seeds PENDING, subsequent samples within for_seconds
// stay PENDING, then once for_seconds elapses an incident is created
// via CreateIncident and the recorder captures the notification.
func TestEvaluateMetricAlert_FiresOnSustainedBreach(t *testing.T) {
	p, rec := newAlertEvalTestStore(t)
	id, err := p.CreateMetricAlertRule(1, protocol.MetricAlertRuleRequest{
		NodeID: "node-a", Scope: "node",
		Metric: "cpu_pct", Op: "gt", Threshold: 80,
		ForSeconds: 30, Severity: "warning",
		Title: "CPU sustained high", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	// Bind owner so the ingest hook resolves the rule list.
	_ = p.BindNode("node-a", 1)

	now := time.Now().Unix()

	// First sample crosses, but breach_start == now → for_seconds not yet elapsed.
	p.EvaluateMetricAlert("node-a", "1", MetricSample{
		NodeID: "node-a", Timestamp: now, CPUPercent: 95, MemUsedPct: 10, DiskUsedPct: 10,
	})
	if got := len(rec.Incidents); got != 0 {
		t.Fatalf("expected no incident immediately, got %d", got)
	}

	// Second sample crosses again, but advance the breach_start into the
	// past so for_seconds is satisfied. We do that by stomping the
	// in-memory map directly — the test's job is to validate the
	// threshold-gate logic, not the wall-clock advance.
	ev := p.alertEvaluator()
	ev.mu.Lock()
	ev.breached[ruleKey(id, "node-a")] = time.Unix(now-31, 0)
	ev.mu.Unlock()

	p.EvaluateMetricAlert("node-a", "1", MetricSample{
		NodeID: "node-a", Timestamp: now, CPUPercent: 96, MemUsedPct: 10, DiskUsedPct: 10,
	})
	if got := len(rec.Incidents); got != 1 {
		t.Fatalf("expected exactly one incident after for_seconds elapsed, got %d", got)
	}
	if rec.Incidents[0].Severity != "warning" {
		t.Fatalf("severity wrong: %s", rec.Incidents[0].Severity)
	}

	// Subsequent crossing while the incident is still open: dedup via
	// existing CreateIncident logic means we don't get a second notify
	// immediately (cooldown gate). Re-evaluating now should be a no-op
	// for dispatch.
	p.EvaluateMetricAlert("node-a", "1", MetricSample{
		NodeID: "node-a", Timestamp: now, CPUPercent: 97, MemUsedPct: 10, DiskUsedPct: 10,
	})
	if got := len(rec.Incidents); got != 1 {
		t.Fatalf("expected dedup, still %d incidents", got)
	}
}

// TestEvaluateMetricAlert_ClearsOnRecovery walks the recovery path: a
// previously-firing rule that drops back below threshold should erase
// the in-memory breach start so the next crossing has to wait the
// full for_seconds again.
func TestEvaluateMetricAlert_ClearsOnRecovery(t *testing.T) {
	p, _ := newAlertEvalTestStore(t)
	id, err := p.CreateMetricAlertRule(1, protocol.MetricAlertRuleRequest{
		NodeID: "node-a", Scope: "node",
		Metric: "mem_pct", Op: "gt", Threshold: 90,
		ForSeconds: 0, Severity: "critical",
		Title: "Memory high", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	_ = p.BindNode("node-a", 1)

	now := time.Now().Unix()
	p.EvaluateMetricAlert("node-a", "1", MetricSample{
		NodeID: "node-a", Timestamp: now, CPUPercent: 10, MemUsedPct: 95, DiskUsedPct: 10,
	})

	ev := p.alertEvaluator()
	ev.mu.Lock()
	_, hasPrev := ev.breached[ruleKey(id, "node-a")]
	firing := ev.firing[ruleKey(id, "node-a")]
	ev.mu.Unlock()
	if !hasPrev || firing == 0 {
		t.Fatalf("expected pending+firing state, got hasPrev=%v firing=%d", hasPrev, firing)
	}

	// Recovery: mem now 50% (under 90). State should clear.
	p.EvaluateMetricAlert("node-a", "1", MetricSample{
		NodeID: "node-a", Timestamp: now + 1, CPUPercent: 10, MemUsedPct: 50, DiskUsedPct: 10,
	})

	ev.mu.Lock()
	_, hasPrev = ev.breached[ruleKey(id, "node-a")]
	firing = ev.firing[ruleKey(id, "node-a")]
	ev.mu.Unlock()
	if hasPrev || firing != 0 {
		t.Fatalf("expected cleared state, got hasPrev=%v firing=%d", hasPrev, firing)
	}
}

// TestEvaluateMetricAlert_IgnoresDisabledRule makes sure disabled rules
// stay dormant even when a sample crosses their threshold forever —
// the toggle has to actually take effect.
func TestEvaluateMetricAlert_IgnoresDisabledRule(t *testing.T) {
	p, rec := newAlertEvalTestStore(t)
	id, err := p.CreateMetricAlertRule(1, protocol.MetricAlertRuleRequest{
		NodeID: "node-a", Scope: "node",
		Metric: "cpu_pct", Op: "gt", Threshold: 50,
		ForSeconds: 0, Severity: "warning",
		Title: "should not fire", Enabled: false,
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	_ = p.BindNode("node-a", 1)

	// Stomp the breach start so the threshold logic doesn't matter.
	ev := p.alertEvaluator()
	ev.mu.Lock()
	ev.breached[ruleKey(id, "node-a")] = time.Unix(time.Now().Unix()-3600, 0)
	ev.mu.Unlock()

	p.EvaluateMetricAlert("node-a", "1", MetricSample{
		NodeID: "node-a", CPUPercent: 95, MemUsedPct: 10, DiskUsedPct: 10,
	})
	if got := len(rec.Incidents); got != 0 {
		t.Fatalf("disabled rule still fired: %d incidents", got)
	}
}

// TestEvaluateMetricAlert_LTOpExercises the lower-watermark variant so
// the ruleBreach switch stays symmetrical — easy regression point if
// someone refactors "lt" away.
func TestEvaluateMetricAlert_LTOpExercises(t *testing.T) {
	p, rec := newAlertEvalTestStore(t)
	if _, err := p.CreateMetricAlertRule(1, protocol.MetricAlertRuleRequest{
		NodeID: "node-a", Scope: "node",
		Metric: "cpu_pct", Op: "lt", Threshold: 10,
		ForSeconds: 0, Severity: "warning",
		Title: "Node idle for too long", Enabled: true,
	}); err != nil {
		t.Fatalf("create rule: %v", err)
	}
	_ = p.BindNode("node-a", 1)

	p.EvaluateMetricAlert("node-a", "1", MetricSample{
		NodeID: "node-a", CPUPercent: 2, MemUsedPct: 10, DiskUsedPct: 10,
	})
	if got := len(rec.Incidents); got != 1 {
		t.Fatalf("lt rule should fire when value below threshold, got %d", got)
	}
}
