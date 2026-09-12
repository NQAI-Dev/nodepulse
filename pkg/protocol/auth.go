package protocol

type RegisterNodeRequest struct {
	Token    string   `json:"token"`
	NodeID   string   `json:"node_id"`
	Hostname string   `json:"hostname"`
	OS       string   `json:"os"`
	Arch     string   `json:"arch"`
}

type RegisterNodeResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

type Incident struct {
	ID        string `json:"id"`
	NodeID    string `json:"node_id"`
	Severity  string `json:"severity"` // info, warning, critical
	Title     string `json:"title"`
	Detail    string `json:"detail"`
	StartedAt int64  `json:"started_at"`
	Resolved  bool   `json:"resolved"`
}
