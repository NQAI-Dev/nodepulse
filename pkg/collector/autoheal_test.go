package collector

import (
	"testing"
)

func TestExecuteCommandInvalid(t *testing.T) {
	err := ExecuteCommand("unknown_action:foo")
	if err == nil {
		t.Fatalf("expected error for unknown action")
	}
}

func TestExecuteCommandDockerEmpty(t *testing.T) {
	err := ExecuteCommand("restart_docker:")
	if err == nil {
		t.Fatalf("expected error for empty docker target")
	}
}
