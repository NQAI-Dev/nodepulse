package protocol


type NodeInfo struct {
	ID       string   `json:"id"`
	Hostname string   `json:"hostname"`
	OS       string   `json:"os"`
	Arch     string   `json:"arch"`
	Version  string   `json:"version"`
	// Tags are free-form key=value (or plain) labels attached by the agent:
	// typical values are env=prod, region=eu, role=db. Empty in older agents
	// so the field stays backwards-compatible on the wire and in SQLite.
	Tags []string `json:"tags,omitempty"`
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
	Name    string            `json:"name"`
	Type    string            `json:"type"` // systemd, docker, port
	Active  bool              `json:"active"`
	Status  string            `json:"status"`
	Message string            `json:"message,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
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
	Probes    []ProbeResult   `json:"probes,omitempty"`
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
	// Class is the adaptive remediation class chosen for this target
	// (db / cache / stateless / default / critical). Always populated so
	// the control plane can attribute a skip or failure to the strategy
	// that produced it.
	Class string `json:"class,omitempty"`
}

// ProbeResult is one synthetic HTTP probe executed by the agent against a
// configured target URL. Agents attach up to N results per heartbeat so the
// control plane can track external availability without running its own
// scanner fleet. URL is the canonical target identifier (the operator
// configures it once and reuses the value across all results). StatusCode
// is 0 if the request never reached a response (DNS, TCP, TLS, timeout).
// LatencyMs is wall-clock from request start to response headers read; for
// transport errors we still record the time spent failing so the latency
// series tells a useful story. Error is populated when StatusCode == 0.
//
// ponytail: this struct deliberately mirrors ServiceStatus shape so the
// public status page can render probes and docker/systemd checks with one
// widget. If we add TCP probes or DNS probes later, switch the type to a
// tagged union or add a Kind field; for now the on-the-wire cost is lower
// with a flat struct.
type ProbeResult struct {
	URL        string `json:"url"`
	// Kind is the probe type that produced this result. Empty values are
	// treated as "http" so the field stays backwards-compatible with
	// agents that pre-date the TCP-probe feature. The control plane uses
	// it to render the right widget on the public status page and to
	// skip HTTP-only fields (StatusCode) for TCP probes.
	Kind       string `json:"kind,omitempty"`
	StatusCode int    `json:"status_code"`
	LatencyMs  int64  `json:"latency_ms"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	Ts         int64  `json:"ts"`
}

// Probe kinds used by agents and the control plane. Keep this list short
// and stable — values are persisted in SQLite and rendered verbatim on the
// public status page.
const (
	ProbeKindHTTP = "http"
	ProbeKindTCP  = "tcp"
	// ProbeKindTLS checks a TLS endpoint by dialing, completing the
	// handshake, and reporting the leaf certificate's expiry window.
	// The control plane renders it as a "certificate health" widget
	// and raises an incident when the cert enters the operator's
	// configured warn window.
	ProbeKindTLS = "tls"
	// ProbeKindDNS resolves a hostname against the system resolver and
	// (optionally) asserts a substring match on the result. It catches
	// split-horizon DNS misconfigurations and stale resolver caches
	// that a TCP probe alone would miss.
	ProbeKindDNS = "dns"
	// ProbeKindICMP sends ICMPv4 echo requests and reports RTT/loss.
	// It is the only probe that exercises layer-3 reachability without
	// TCP or UDP noise — useful for diagnosing firewall rules, MTU
	// black holes, and BGP routing that a TCP connect would not see.
	// Requires CAP_NET_RAW or net.ipv4.ping_group_range on Linux; the
	// runner surfaces "permission_denied" on the ProbeResult.Error
	// field when the kernel refuses the socket.
	ProbeKindICMP = "icmp"
)
