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
	NodeID    string  `json:"node_id"`
	Days      int     `json:"days"`
	UptimePct float64 `json:"uptime_pct"`
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
