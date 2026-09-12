package protocol


type NodeInfo struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Version  string `json:"version"`
}

type CPUStats struct {
	UsagePercent float64 `json:"usage_pct"`
	Load1        float64 `json:"load_1"`
	Load5        float64 `json:"load_5"`
	Load15       float64 `json:"load_15"`
	Cores        int     `json:"cores"`
}

type MemoryStats struct {
	TotalBytes     uint64  `json:"total_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	UsedPercent    float64 `json:"used_pct"`
}

type DiskStats struct {
	MountPoint  string  `json:"mount"`
	TotalBytes  uint64  `json:"total_bytes"`
	FreeBytes   uint64  `json:"free_bytes"`
	UsedPercent float64 `json:"used_pct"`
}

type ServiceStatus struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // systemd, docker, port
	Active  bool   `json:"active"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type Heartbeat struct {
	NodeID    string          `json:"node_id"`
	Timestamp int64           `json:"timestamp"`
	Node      NodeInfo        `json:"node"`
	CPU       CPUStats        `json:"cpu"`
	Memory    MemoryStats     `json:"memory"`
	Disks     []DiskStats     `json:"disks"`
	Services  []ServiceStatus `json:"services,omitempty"`
}

type HeartbeatResponse struct {
	Acknowledged bool     `json:"ack"`
	Commands     []string `json:"commands,omitempty"` // auto-heal / action triggers
}
