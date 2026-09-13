package collector

import (
	"fmt"
	"strings"
)

// AllowedActions is the closed set of remediation verbs the agent will
// execute. Anything outside this set is rejected before it reaches a
// shell or the Docker socket — the server-side admin UI uses the same
// allow-list to preview and authorize commands.
var AllowedActions = map[string]struct{}{
	"restart_docker":  {},
	"restart_systemd": {},
}

// Plan is a parsed, dry-runnable view of a remediation command. The
// Caller field names the executable that would run; Args is the
// exact argv (no shell interpolation); WorkingDir is informational.
type Plan struct {
	Action string
	Target string
	Caller string
	Args   []string
	Reason string // human-readable summary, safe to surface in UI
}

// ParseCommand splits a command into action:target. It is the same
// split ExecuteCommand performs, but exposed for the preview path so
// callers can validate before queuing.
func ParseCommand(cmd string) (action, target string, err error) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return "", "", fmt.Errorf("empty command")
	}
	parts := strings.SplitN(cmd, ":", 2)
	action = strings.TrimSpace(parts[0])
	if action == "" {
		return "", "", fmt.Errorf("missing action")
	}
	if _, ok := AllowedActions[action]; !ok {
		return "", "", fmt.Errorf("action %q not in allow-list", action)
	}
	if len(parts) == 2 {
		target = strings.TrimSpace(parts[1])
	}
	if target == "" {
		return "", "", fmt.Errorf("action %q requires a target", action)
	}
	return action, target, nil
}

// PlanCommand produces a dry-run Plan without executing anything. The
// Caller/Args describe exactly what ExecuteCommand would invoke; the UI
// shows this as "would run: systemctl restart nginx" before the operator
// confirms.
func PlanCommand(cmd string) (Plan, error) {
	action, target, err := ParseCommand(cmd)
	if err != nil {
		return Plan{}, err
	}
	switch action {
	case "restart_docker":
		return Plan{
			Action: action,
			Target: target,
			Caller: "docker",
			Args:   []string{"restart", target},
			Reason: fmt.Sprintf("POST /containers/%s/restart via docker.sock", target),
		}, nil
	case "restart_systemd":
		return Plan{
			Action: action,
			Target: target,
			Caller: "systemctl",
			Args:   []string{"restart", target},
			Reason: fmt.Sprintf("systemctl restart %s (5s timeout)", target),
		}, nil
	}
	return Plan{}, fmt.Errorf("action %q not in allow-list", action)
}

// ponytail: AllowedActions is a static map. Promote to per-node config
// when ops needs to grant a single edge node a custom verb (e.g.
// "reload_nginx") without redeploying the agent binary.
