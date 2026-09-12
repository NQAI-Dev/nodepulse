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
