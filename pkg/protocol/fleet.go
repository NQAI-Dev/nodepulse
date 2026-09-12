package protocol

// PublicFleetSummary is the compact /api/v1/public/fleet-summary payload.
// Intentionally smaller than PublicStatusPage so external status widgets can
// poll it cheaply without pulling the full per-service / per-uptime payload.
type PublicFleetSummary struct {
	Status         string  `json:"status"`           // operational | degraded | outage
	UpdatedAt      int64   `json:"updated_at"`       // unix seconds
	NodesTotal     int     `json:"nodes_total"`
	NodesOnline    int     `json:"nodes_online"`
	NodesWarning   int     `json:"nodes_warning"`
	NodesOffline   int     `json:"nodes_offline"`
	IncidentsOpen  int     `json:"incidents_open"`
	IncidentsCrit  int     `json:"incidents_critical"`
	Uptime7dPct    float64 `json:"uptime_7d_pct"`    // weighted fleet uptime, last 7 days
}
