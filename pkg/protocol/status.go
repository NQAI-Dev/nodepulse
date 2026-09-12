package protocol

type PublicStatusPage struct {
	Title       string             `json:"title"`
	Description string             `json:"description"`
	Status      string             `json:"status"` // operational, degraded, outage
	UpdatedAt   int64              `json:"updated_at"`
	NodesTotal  int                `json:"nodes_total"`
	NodesOnline int                `json:"nodes_online"`
	Services    []PublicService    `json:"services"`
	Incidents   []PublicIncident   `json:"recent_incidents"`
	Uptime      []PublicNodeUptime `json:"uptime"`
}

type PublicService struct {
	Name   string `json:"name"`
	Node   string `json:"node"`
	Status string `json:"status"`
	Active bool   `json:"active"`
}

type PublicIncident struct {
	ID        string `json:"id"`
	NodeID    string `json:"node_id"`
	Title     string `json:"title"`
	Severity  string `json:"severity"`
	StartedAt int64  `json:"started_at"`
}

type PublicNodeUptime struct {
	NodeID    string   `json:"node_id"`
	Days      int      `json:"days"`
	UptimePct float64  `json:"uptime_pct"`
	Tags      []string `json:"tags,omitempty"`
}

// PublicNode is one entry on the /api/v1/public/nodes list. Combines the
// heartbeat-driven online/warning/offline status with the persisted tag
// set so a status widget can filter by env/region/role without a second
// call. Empty Tags means "agent never reported any" (older 0.3.x agents).
type PublicNode struct {
	NodeID    string   `json:"node_id"`
	Hostname  string   `json:"hostname"`
	Status    string   `json:"status"`
	Tags      []string `json:"tags,omitempty"`
	UpdatedAt int64    `json:"updated_at"`
}

// PublicUptimeRow is one UTC day in the public uptime drill-down. Matches
// store.UptimeDayBucket so the JSON shape is identical for fleet vs per-node
// endpoints — widgets can render either with the same parser.
type PublicUptimeRow struct {
	Day       string  `json:"day"`        // YYYY-MM-DD (UTC)
	TotalSecs int64   `json:"total_secs"` // seconds observed this day
	UpSecs    int64   `json:"up_secs"`    // seconds where node was online
	UptimePct float64 `json:"uptime_pct"` // 0..100; 0 when TotalSecs == 0
}

// PublicUptimeSeries is the response for both /public/uptime (fleet-wide)
// and /public/uptime/{nodeID} (single node). Days is the window length,
// UpdatedAt is server-side unix seconds for cache headers.
type PublicUptimeSeries struct {
	Scope     string           `json:"scope"`     // "fleet" or node_id
	Days      int              `json:"days"`
	UpdatedAt int64            `json:"updated_at"`
	Rows      []PublicUptimeRow `json:"rows"`
}

// PublicIncidentHistory is one row on the public status timeline.
// Severity, title, node id, and timestamps only — operator acknowledgements
// and tenant-scoped data are intentionally excluded.
type PublicIncidentHistory struct {
	ID         string `json:"id"`
	NodeID     string `json:"node_id"`
	Severity   string `json:"severity"`
	Title      string `json:"title"`
	StartedAt  int64  `json:"started_at"`
	Resolved   bool   `json:"resolved"`
	ResolvedAt int64  `json:"resolved_at"`
}
