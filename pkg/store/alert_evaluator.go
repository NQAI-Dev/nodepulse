package store

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// AlertEvaluator holds per-rule timing state (when the rule started
// breaching and which incident row owns the current open event). The
// struct is allocated once at server boot and shared across heartbeat
// goroutines; all access is serialized through evalMu.
//
// State machine per rule:
//
//	STOPPED   --(breach starts)--> PENDING (first sample observed)
//	PENDING   --(still breaching)--> PENDING
//	PENDING   --(for_seconds elapsed)--> FIRING (CreateIncident called)
//	FIRING    --(still breaching)--> FIRING (cooldown gate suppresses notify)
//	FIRING    --(sample back inside)--> STOPPED
//	STOPPED   --(sample inside)--> STOPPED
//
// We persist bookkeeping only for last_fired_at and last_cleared_at on
// the rule row; the in-memory breachStart is fine to lose on restart —
// a rule that was just about to fire will re-enter PENDING on the next
// heartbeat, which is the safer outcome (slightly delayed alert beats a
// missed one if the server restart was triggered by an underlying issue).
type AlertEvaluator struct {
	mu       sync.Mutex
	breached map[string]time.Time // ruleKey -> first sample time when breach began
	firing   map[string]int64     // ruleKey -> open incident id (empty when stopped)
	ruleByID map[int64]evalCache  // rule id -> memoised copy to avoid re-fetch
}

type evalCache struct {
	rule    protocol.MetricAlertRule
	lastTSSample time.Time
}

// NewAlertEvaluator builds an empty evaluator. The zero value is also
// usable, but the constructor exists so callers don't need to remember
// to lazy-init the maps on first use.
func NewAlertEvaluator() *AlertEvaluator {
	return &AlertEvaluator{
		breached: make(map[string]time.Time),
		firing:   make(map[string]int64),
		ruleByID: make(map[int64]evalCache),
	}
}

// ruleKey is the stable identity for an evaluator's per-rule state slot.
// Same rule id editing its threshold behaves as the same slot, so a
// threshold tweak collapses an in-flight PENDING transition cleanly
// rather than leaving a half-cleared state on the floor.
func ruleKey(ruleID int64, nodeID string) string {
	return fmt.Sprintf("%d|%s", ruleID, nodeID)
}

// EvaluateMetricAlert walks every rule that applies to nodeID, decides
// whether the latest sample crosses the threshold, and either fires an
// incident (via CreateIncident, which honours the existing cooldown and
// notification pipeline) or clears an open firing state. Called from
// the ingest path; failures are isolated per-rule so one bad rule can't
// poison the fleet's alerting.
//
// ponytail: when this becomes a hot bottleneck, batch the per-node rule
// lookup into the Ingest path itself so we only fetch the affected
// rules once per node instead of per heartbeat boundary.
func (p *PersistentStore) EvaluateMetricAlert(nodeID, ownerUserID string, sample MetricSample) {
	if p == nil || nodeID == "" {
		return
	}
	rules := p.MetricsAlertRulesForNode(nodeID, ownerUserID)
	if len(rules) == 0 {
		return
	}

	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		value, ok := metricSampleValue(sample, rule.Metric)
		if !ok {
			continue
		}

		key := ruleKey(rule.ID, nodeID)
		breach := ruleBreach(rule.Op, value, rule.Threshold)
		now := time.Now()

		ev := p.alertEvaluator()
		ev.mu.Lock()
		prevBreachStart, hasPrev := ev.breached[key]
		prevIncident := ev.firing[key]

		switch {
		case breach:
			// Establish the breach start timestamp. On the very first
			// crossing it becomes `now`; on subsequent crossings it
			// stays at the original start so for_seconds is measured
			// against the *first* crossing, not each tick.
			if !hasPrev {
				ev.breached[key] = now
				prevBreachStart = now
			}
			// Already-firing rules don't need to re-notify; the existing
			// CreateIncident cooldown gate handles dispatch suppression.
			// Otherwise, evaluate the time gate — for_seconds==0 always
			// satisfies (elapsed >= 0), so a rule declared with no
			// dwell requirement fires on its first crossing. This is
			// important: the test harness can't afford to round-trip
			// through wall-clock time to verify the path, and neither
			// can a brand-new incident landing on the first heartbeat.
			if prevIncident == 0 {
				elapsed := now.Sub(prevBreachStart)
				if int(elapsed.Seconds()) >= rule.ForSeconds {
					ev.mu.Unlock()
					// Fire! CreateIncident handles dedup (one open
					// incident per (node, title)) and routes through
					// the existing telegram + webhook dispatch.
					detail := fmt.Sprintf(
						"metric=%s value=%.2f threshold=%.2f for=%ds",
						rule.Metric, value, rule.Threshold, rule.ForSeconds,
					)
					p.CreateIncident(nodeID, rule.Severity, rule.Title, detail)
					p.MarkAlertRuleFired(rule.ID)
					ev.mu.Lock()
					ev.firing[key] = incidentIDAfterCreate(p, nodeID, rule.Title)
					ev.mu.Unlock()
				} else {
					ev.mu.Unlock()
				}
			} else {
				ev.mu.Unlock()
			}
		default:
			// Sample is back inside. Drop the pending entry; if we had a
			// firing incident, the resolve path will close it on the
			// next heartbeat that detects the service back up (or via
			// the regular incident-resolve watchdog). We also stamp
			// last_cleared_at for the audit trail.
			if hasPrev || prevIncident != 0 {
				delete(ev.breached, key)
				ev.firing[key] = 0
				ev.mu.Unlock()
				p.MarkAlertRuleCleared(rule.ID)
			} else {
				ev.mu.Unlock()
			}
		}
	}
}

// incidentIDAfterCreate returns the open incident id for the freshly
// created row. Mirrors the lookup CreateIncident uses internally; the
// alternative is teaching CreateIncident to return the id, but that's a
// larger change for one consumer, and the second SELECT is cheap (one row,
// indexed by (node_id, title, resolved)).
func incidentIDAfterCreate(p *PersistentStore, nodeID, title string) int64 {
	var id int64
	err := p.db.QueryRow(
		`SELECT id FROM incidents WHERE node_id = ? AND title = ? AND resolved = 0 ORDER BY id DESC LIMIT 1`,
		nodeID, title,
	).Scan(&id)
	if err != nil {
		return 0
	}
	return id
}

// ruleBreach is the comparison body for an op/threshold pair. Kept as a
// tiny switch so adding new ops later (e.g. "le" / "ge" aliases) is a
// one-line edit next to the canonical doc.
func ruleBreach(op string, value, threshold float64) bool {
	switch strings.TrimSpace(op) {
	case "gt":
		return value > threshold
	case "lt":
		return value < threshold
	default:
		return false
	}
}

// metricSampleValue extracts the canonical field that corresponds to
// rule.Metric from the latest heartbeat-derived MetricSample. Returns
// (value, true) when the metric is supported; (0, false) otherwise — the
// caller skips the rule rather than misfiring on a stale "metric=0".
func metricSampleValue(s MetricSample, metric string) (float64, bool) {
	switch strings.TrimSpace(metric) {
	case "cpu_pct":
		return s.CPUPercent, true
	case "mem_pct":
		return s.MemUsedPct, true
	case "disk_pct":
		return s.DiskUsedPct, true
	case "load1":
		return s.Load1, true
	default:
		return 0, false
	}
}

// alertEvaluator returns the cached evaluator; tests can use
// SetAlertEvaluator to inject a fresh one between cases.
func (p *PersistentStore) alertEvaluator() *AlertEvaluator {
	if p == nil {
		return NewAlertEvaluator()
	}
	p.alertInitOnce.Do(func() {
		p.alertEval = NewAlertEvaluator()
	})
	return p.alertEval
}

// SetAlertEvaluator lets tests reset the evaluator without bouncing the
// whole store; production code never touches this.
func (p *PersistentStore) SetAlertEvaluator(e *AlertEvaluator) {
	if p == nil {
		return
	}
	p.alertEval = e
}
