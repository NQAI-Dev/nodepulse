package protocol

import (
	"encoding/json"
	"testing"
)

func TestHeartbeatSerialization(t *testing.T) {
	hb := Heartbeat{
		NodeID:    "node-123",
		Timestamp: 1726950000,
		Node: NodeInfo{
			ID:       "node-123",
			Hostname: "worker-1",
			OS:       "linux",
			Arch:     "amd64",
			Version:  "v1.0.0",
			Tags:     []string{"env=prod", "region=eu"},
		},
		CPU: CPUStats{
			UsagePercent: 45.2,
			Load1:        1.2,
			Load5:        0.8,
			Load15:       0.5,
			Cores:        4,
		},
		Memory: MemoryStats{
			TotalBytes:     16000000000,
			AvailableBytes: 8000000000,
			UsedBytes:      8000000000,
			UsedPercent:    50.0,
		},
		Disks: []DiskStats{
			{
				MountPoint:  "/",
				TotalBytes:  100000000000,
				FreeBytes:   60000000000,
				UsedPercent: 40.0,
			},
		},
		Network: []NetStats{
			{
				Iface:     "eth0",
				RxBytes:   1024,
				TxBytes:   2048,
				SpeedMbps: 1000,
			},
		},
		Services: []ServiceStatus{
			{
				Name:   "nginx",
				Type:   "systemd",
				Active: true,
				Status: "running",
			},
		},
	}

	data, err := json.Marshal(hb)
	if err != nil {
		t.Fatalf("failed to marshal Heartbeat: %v", err)
	}

	var decoded Heartbeat
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal Heartbeat: %v", err)
	}

	if decoded.NodeID != hb.NodeID {
		t.Errorf("NodeID mismatch: got %s, want %s", decoded.NodeID, hb.NodeID)
	}
	if len(decoded.Node.Tags) != 2 || decoded.Node.Tags[0] != "env=prod" {
		t.Errorf("Tags mismatch: got %v, want %v", decoded.Node.Tags, hb.Node.Tags)
	}
	if len(decoded.Services) != 1 || decoded.Services[0].Name != "nginx" {
		t.Errorf("Services mismatch: got %v", decoded.Services)
	}
}

func TestHeartbeatSerializationUsesWireFieldNames(t *testing.T) {
	hb := Heartbeat{
		NodeID:    "node-1",
		Timestamp: 1726950000,
		CPU: CPUStats{
			UsagePercent: 12.5,
		},
		Memory: MemoryStats{
			UsedPercent: 25,
		},
		Disks: []DiskStats{{MountPoint: "/", UsedPercent: 50}},
	}

	data, err := json.Marshal(hb)
	if err != nil {
		t.Fatalf("failed to marshal Heartbeat: %v", err)
	}

	var wire struct {
		NodeID string `json:"node_id"`
		CPU    struct {
			UsagePercent float64 `json:"usage_pct"`
		} `json:"cpu"`
		Memory struct {
			UsedPercent float64 `json:"used_pct"`
		} `json:"memory"`
		Disks []struct {
			MountPoint  string  `json:"mount"`
			UsedPercent float64 `json:"used_pct"`
		} `json:"disks"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("failed to decode wire payload: %v", err)
	}

	if wire.NodeID != hb.NodeID || wire.CPU.UsagePercent != hb.CPU.UsagePercent ||
		wire.Memory.UsedPercent != hb.Memory.UsedPercent {
		t.Fatalf("wire field names did not preserve values: %s", data)
	}
	if len(wire.Disks) != 1 || wire.Disks[0].MountPoint != "/" || wire.Disks[0].UsedPercent != 50 {
		t.Fatalf("wire disk fields did not preserve values: %s", data)
	}
}

func TestIncidentSerialization(t *testing.T) {
	inc := Incident{
		ID:        "inc-1",
		NodeID:    "node-1",
		Severity:  "critical",
		Title:     "Disk Full",
		Detail:    "/ mountpoint is 99% full",
		StartedAt: 1726950000,
		Resolved:  false,
	}

	data, err := json.Marshal(inc)
	if err != nil {
		t.Fatalf("failed to marshal Incident: %v", err)
	}

	var decoded Incident
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal Incident: %v", err)
	}

	if decoded.ID != inc.ID || decoded.Severity != "critical" {
		t.Errorf("unexpected incident unmarshal result: %+v", decoded)
	}
}
