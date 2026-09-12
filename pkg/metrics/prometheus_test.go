package metrics

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
	"github.com/NQAI-Dev/nodepulse/pkg/store"
)

func TestWriteFleetEmpty(t *testing.T) {
	dbFile := "test_metrics_empty.db"
	defer os.Remove(dbFile)

	s, err := store.NewPersistentStore(dbFile, "", 0)
	if err != nil {
		t.Fatalf("store init: %v", err)
	}

	var buf strings.Builder
	if err := WriteFleet(&buf, s, time.Now().Add(-30*time.Second)); err != nil {
		t.Fatalf("write fleet: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "nodepulse_build_info{component=\"server\"} 1") {
		t.Fatalf("missing build_info line")
	}
	if !strings.Contains(out, "nodepulse_up_seconds") {
		t.Fatalf("missing up_seconds metric")
	}
	if !strings.Contains(out, "nodepulse_incidents_active 0") {
		t.Fatalf("expected 0 incidents on empty fleet, got: %s", out)
	}
	if strings.Contains(out, "nodepulse_node_info{") {
		t.Fatalf("did not expect per-node metrics in empty fleet")
	}
}

func TestWriteFleetWithNode(t *testing.T) {
	dbFile := "test_metrics_node.db"
	defer os.Remove(dbFile)

	s, err := store.NewPersistentStore(dbFile, "", 0)
	if err != nil {
		t.Fatalf("store init: %v", err)
	}

	hb := &protocol.Heartbeat{
		NodeID:    "gamma-vps",
		Timestamp: time.Now().Unix(),
		Node: protocol.NodeInfo{
			ID:       "gamma-vps",
			Hostname: "gamma",
			OS:       "debian/13",
			Arch:     "amd64",
			Version:  "agent-1.2.0",
		},
		CPU: protocol.CPUStats{Load1: 0.75, Cores: 4},
		Memory: protocol.MemoryStats{
			UsedPercent: 65.5,
		},
		Disks: []protocol.DiskStats{
			{MountPoint: "/", UsedPercent: 42.0},
		},
		Services: []protocol.ServiceStatus{
			{Name: "nginx", Type: "docker", Active: true},
			{Name: "valkey", Type: "docker", Active: false},
		},
	}
	s.Ingest(hb)

	var buf strings.Builder
	if err := WriteFleet(&buf, s, time.Now().Add(-90*time.Second)); err != nil {
		t.Fatalf("write fleet: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		`nodepulse_node_info{host="gamma-vps",os="debian/13",version="agent-1.2.0",arch="amd64"} 1`,
		`nodepulse_node_status{node="gamma-vps"} 1`,
		`nodepulse_node_load1{node="gamma-vps"} 0.750`,
		`nodepulse_node_cores{node="gamma-vps"} 4`,
		`nodepulse_node_memory_used_percent{node="gamma-vps"} 65.50`,
		`nodepulse_node_disk_used_percent{node="gamma-vps"} 42.00`,
		`nodepulse_node_service_up{node="gamma-vps",service="nginx",type="docker"} 1`,
		`nodepulse_node_service_up{node="gamma-vps",service="valkey",type="docker"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing line %q in:\n%s", want, out)
		}
	}
}

func TestEscape(t *testing.T) {
	cases := map[string]string{
		`plain`:       `plain`,
		`with"quote`:  `with\"quote`,
		`with\back`:    `with\\back`,
		"line\nbreak": `line\nbreak`,
		"":            "",
	}
	for in, want := range cases {
		if got := escape(in); got != want {
			t.Fatalf("escape(%q) = %q, want %q", in, got, want)
		}
	}
}
