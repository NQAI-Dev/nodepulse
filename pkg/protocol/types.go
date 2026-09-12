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

// NetStats captures the cumulative byte/packet counters for a single
// network interface at heartbeat time. Rates (bytes/sec) are derived on
// the server by subtracting the previous sample's counters.
type NetStats struct {
	Iface       string `json:"iface"`
	RxBytes     uint64 `json:"rx_bytes"`
	TxBytes     uint64 `json:"tx_bytes"`
	RxPackets   uint64 `json:"rx_packets"`
	TxPackets   uint64 `json:"tx_packets"`
	RxErrors    uint64 `json:"rx_errors"`
	TxErrors    uint64 `json:"tx_errors"`
	RxDrops     uint64 `json:"rx_drops"`
	TxDrops     uint64 `json:"tx_drops"`
	SpeedMbps   uint64 `json:"speed_mbps,omitempty"` // nominal link speed, 0 if unknown
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
	Network   []NetStats      `json:"network,omitempty"`
	Services  []ServiceStatus `json:"services,omitempty"`
}

type HeartbeatResponse struct {
	Acknowledged bool     `json:"ack"`
	Commands     []string `json:"commands,omitempty"` // auto-heal / action triggers
}

// AutoHealLog describes a single auto-heal action attempt reported by an
// agent back to the control plane. Status values: "ok", "failed", "skipped".
// Reason is populated when Status == "skipped" with the breaker rationale
// ("cooldown" or "circuit_open"). RetrySec is the breaker-suggested delay
// before the next attempt when Status == "skipped".
type AutoHealLog struct {
	Command  string `json:"command"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Error    string `json:"error,omitempty"`
	Ts       int64  `json:"ts"`
	RetrySec int64  `json:"retry_sec,omitempty"`
}
