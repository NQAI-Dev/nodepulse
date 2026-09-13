package collector

import (
	"strings"
	"testing"
)

func TestParseCommand_Valid(t *testing.T) {
	cases := []struct {
		cmd    string
		action string
		target string
	}{
		{"restart_docker:web", "restart_docker", "web"},
		{"restart_systemd:nginx.service", "restart_systemd", "nginx.service"},
		{"  restart_docker : api  ", "restart_docker", "api"},
	}
	for _, c := range cases {
		a, tg, err := ParseCommand(c.cmd)
		if err != nil {
			t.Fatalf("ParseCommand(%q) unexpected error: %v", c.cmd, err)
		}
		if a != c.action || tg != c.target {
			t.Fatalf("ParseCommand(%q) = (%q,%q), want (%q,%q)", c.cmd, a, tg, c.action, c.target)
		}
	}
}

func TestParseCommand_RejectsBadInput(t *testing.T) {
	bad := []string{
		"",
		"   ",
		"unknown_action:x",
		"restart_docker",
		"restart_docker:",
		":only_target",
	}
	for _, c := range bad {
		if _, _, err := ParseCommand(c); err == nil {
			t.Fatalf("ParseCommand(%q) expected error, got nil", c)
		}
	}
}

func TestPlanCommand_DockerShape(t *testing.T) {
	p, err := PlanCommand("restart_docker:api")
	if err != nil {
		t.Fatalf("PlanCommand: %v", err)
	}
	if p.Action != "restart_docker" || p.Target != "api" {
		t.Fatalf("unexpected plan: %+v", p)
	}
	if p.Caller != "docker" {
		t.Fatalf("caller = %q, want docker", p.Caller)
	}
	if len(p.Args) != 2 || p.Args[0] != "restart" || p.Args[1] != "api" {
		t.Fatalf("argv = %v, want [restart api]", p.Args)
	}
	if !strings.Contains(p.Reason, "api") {
		t.Fatalf("reason should mention target, got %q", p.Reason)
	}
}

func TestPlanCommand_SystemdShape(t *testing.T) {
	p, err := PlanCommand("restart_systemd:nginx.service")
	if err != nil {
		t.Fatalf("PlanCommand: %v", err)
	}
	if p.Caller != "systemctl" {
		t.Fatalf("caller = %q, want systemctl", p.Caller)
	}
	if len(p.Args) != 2 || p.Args[0] != "restart" || p.Args[1] != "nginx.service" {
		t.Fatalf("argv = %v, want [restart nginx.service]", p.Args)
	}
}

func TestPlanCommand_RejectsUnknownAction(t *testing.T) {
	if _, err := PlanCommand("rm_rf:/"); err == nil {
		t.Fatal("expected rejection for unknown action")
	}
}

// Sanity: ExecuteCommand shares the same parser, so a malformed
// command must surface as an error from the live path too.
func TestExecuteCommand_RejectsMalformed(t *testing.T) {
	if err := ExecuteCommand("nuke:everything"); err == nil {
		t.Fatal("ExecuteCommand should reject actions outside AllowedActions")
	}
}
