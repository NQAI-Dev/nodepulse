package protocol

// MetricAlertRule is a declarative threshold bound to a node (or fleet)
// that fires incidents when the underlying metric sample crosses `Threshold`
// for at least `ForSeconds` consecutive seconds. Cooldown prevents
// flapping notifications and is enforced by the existing incident-cooldown
// gate, so this struct only owns the *evaluation* parameters, not the
// dispatch policy.
//
// Metric kinds:
//   - "cpu_pct"  : 0..100 saturation, load1 / cores * 100
//   - "mem_pct"  : 0..100 RAM utilisation
//   - "disk_pct" : 0..100 used percent on the root mount reported by agent
//   - "load1"    : raw 1-minute load average, no normalisation
//
// Op kinds:
//   - "gt"  : fires when sample > threshold
//   - "lt"  : fires when sample < threshold (use for low-watermark alarms)
//
// Scope kinds:
//   - "node" : scoped to NodeID only
//   - "fleet" : matched against TagSelector on every heartbeat (any
//               node with the supplied tags inherits the rule)
//
// NotifyChannel reuses the existing user_settings routing. Critical =
// fires faster, transport = telegram + webhook (when configured);
// Warning = same routing but slower cooldown. Operators don't override
// dispatch channels per-rule today; the per-user wiring in user_settings
// is the single source of truth.
//
// ponytail: if operators ever want per-rule channel overrides (e.g. "send
// slack alerts for disk rules even if I disabled warning notifications"),
// extend this with NotificationOverrides → alerter.Notifier selector.
type MetricAlertRule struct {
	ID           int64    `json:"id"`
	UserID       int64    `json:"user_id"`
	NodeID       string   `json:"node_id,omitempty"`     // for scope=node
	TagSelector  string   `json:"tag_selector,omitempty"` // for scope=fleet
	Scope        string   `json:"scope"`                  // "node" | "fleet"
	Metric       string   `json:"metric"`                 // "cpu_pct" | "mem_pct" | "disk_pct" | "load1"
	Op           string   `json:"op"`                     // "gt" | "lt"
	Threshold    float64  `json:"threshold"`
	ForSeconds   int      `json:"for_seconds"` // 0 = fires on single crossing
	Severity     string   `json:"severity"`    // "warning" | "critical"
	Title        string   `json:"title"`       // human-readable; rendered in alert
	Enabled      bool     `json:"enabled"`
	CreatedAt    int64    `json:"created_at"`
	UpdatedAt    int64    `json:"updated_at"`
	LastFiredAt  int64    `json:"last_fired_at,omitempty"`  // unix seconds; 0 if never
	LastClearedAt int64   `json:"last_cleared_at,omitempty"`
	NodeIDs      []string `json:"node_ids,omitempty"` // populated on read for scope=fleet
}

// MetricAlertRuleRequest is the JSON body for POST and PUT. Server stamps
// user_id from the auth context; created_at / updated_at are read-only.
type MetricAlertRuleRequest struct {
	NodeID      string  `json:"node_id"`
	TagSelector string  `json:"tag_selector"`
	Scope       string  `json:"scope"`
	Metric      string  `json:"metric"`
	Op          string  `json:"op"`
	Threshold   float64 `json:"threshold"`
	ForSeconds  int     `json:"for_seconds"`
	Severity    string  `json:"severity"`
	Title       string  `json:"title"`
	Enabled     bool    `json:"enabled"`
}
