package store

import (
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func TestPublicStatus(t *testing.T) {
	st, err := NewPersistentStore(":memory:", "", 0)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	hb := &protocol.Heartbeat{
		NodeID:    "test-node-1",
		Timestamp: time.Now().Unix(),
		Node: protocol.NodeInfo{
			Hostname: "srv-01",
			Arch:     "amd64",
			OS:       "linux",
		},
		Services: []protocol.ServiceStatus{
			{Name: "docker-api", Active: true, Status: "running"},
		},
	}

	st.Ingest(hb)

	pub := st.GetPublicStatus()
	if pub.NodesTotal != 1 {
		t.Errorf("expected 1 node, got %d", pub.NodesTotal)
	}
	if pub.Status != "operational" {
		t.Errorf("expected operational status, got %s", pub.Status)
	}
	if len(pub.Services) != 1 {
		t.Errorf("expected 1 service, got %d", len(pub.Services))
	}
}
