package protocol

// PublicMaintenanceNotice is a public view of an active or upcoming maintenance window.
// Excludes user/tenant identifiers and internal audit details.
type PublicMaintenanceNotice struct {
	ID        int64    `json:"id"`
	Scope     string   `json:"scope"`
	NodeIDs   []string `json:"node_ids,omitempty"`
	Reason    string   `json:"reason"`
	StartUnix int64    `json:"start_unix"`
	EndUnix   int64    `json:"end_unix"` // 0 = until further notice
	Active    bool     `json:"active"`    // true if currently within the window
}
