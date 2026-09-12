package store

import (
	"testing"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func TestEvaluateAutoHeal(t *testing.T) {
	st, err := NewPersistentStore(":memory:", "", 0)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	hb := &protocol.Heartbeat{
		NodeID: "node-1",
		Services: []protocol.ServiceStatus{
			{Name: "web-nginx", Type: "docker", Active: false},
			{Name: "redis", Type: "docker", Active: true},
			{Name: "angie", Type: "systemd", Active: false},
		},
	}

	cmds := st.EvaluateAutoHeal(hb)
	if len(cmds) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(cmds))
	}

	if cmds[0] != "restart_docker:web-nginx" {
		t.Errorf("expected restart_docker:web-nginx, got %s", cmds[0])
	}
	if cmds[1] != "restart_systemd:angie" {
		t.Errorf("expected restart_systemd:angie, got %s", cmds[1])
	}
}
