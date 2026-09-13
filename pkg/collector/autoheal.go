package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// ErrPostRestartDown signals that a remediation action (typically a Docker
// restart) succeeded at the API layer but the target is no longer in the
// expected healthy state moments later. Callers should treat this as a
// crash-loop observation: the action itself did not error, but the
// underlying problem is not fixed and additional restarts are unlikely to
// help. It feeds the autoheal breaker's RecordCrash path so a flapping
// container escalates faster than a generic failure.
var ErrPostRestartDown = errors.New("post-restart target not healthy")

// dockerInspectTimeout bounds the live state check after a successful
// restart. Two seconds is long enough for a healthy container to settle
// into "running" but short enough that a crash-loop never blocks the
// heartbeat loop for noticeable wall-clock time.
const dockerInspectTimeout = 2 * time.Second

// dockerSocketPath points at the local Docker daemon socket. It is a
// variable (not a constant) so unit tests can redirect the dialer to an
// httptest server without binding a unix socket themselves. Tests set a
// bare "host:port" string; production keeps the unix path which always
// starts with "/".
var dockerSocketPath = "/var/run/docker.sock"

// dialDockerSocket opens a connection to the configured Docker endpoint.
// Production hits a unix socket (path starts with "/"); tests inject a
// TCP host:port so the rest of the request pipeline stays identical.
func dialDockerSocket(ctx context.Context, _, _ string) (net.Conn, error) {
	if strings.HasPrefix(dockerSocketPath, "/") {
		return (&net.Dialer{}).DialContext(ctx, "unix", dockerSocketPath)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", dockerSocketPath)
}

// AutoHeal executes remediation actions returned by the control plane.
// Parsing delegates to ParseCommand so the dry-run PlanCommand path and
// the live executor share one allow-list and one grammar.
func ExecuteCommand(cmd string) error {
	action, target, err := ParseCommand(cmd)
	if err != nil {
		return err
	}
	switch action {
	case "restart_docker":
		return restartDockerContainer(target)
	case "restart_systemd":
		return restartSystemdService(target)
	default:
		return fmt.Errorf("unknown remediation action: %s", action)
	}
}

func restartDockerContainer(nameOrID string) error {
	if nameOrID == "" {
		return fmt.Errorf("container name/id required")
	}
	dialer := &http.Transport{
		DialContext: dialDockerSocket,
	}

	postClient := &http.Client{Transport: dialer, Timeout: 10 * time.Second}
	url := fmt.Sprintf("http://localhost/containers/%s/restart", nameOrID)
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return err
	}

	resp, err := postClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker restart failed with status %d", resp.StatusCode)
	}
	log.Printf("[auto-heal] Successfully restarted Docker container: %s", nameOrID)

	// Post-restart live check: a flapping container that crashes within a
	// second or two of starting will report the restart as successful but
	// be back in "exited" by the time the next heartbeat arrives. Without
	// this check the breaker never sees a failure and the loop runs
	// forever. Treat any non-running state as ErrPostRestartDown so the
	// agent's autoheal breaker can short-circuit the loop.
	inspectClient := &http.Client{Transport: dialer, Timeout: dockerInspectTimeout}
	inspReq, err := http.NewRequest("GET", fmt.Sprintf("http://localhost/containers/%s/json", nameOrID), nil)
	if err != nil {
		// Best-effort inspection; do not mask a successful restart.
		return nil
	}
	inspCtx, cancel := context.WithTimeout(context.Background(), dockerInspectTimeout)
	defer cancel()
	inspReq = inspReq.WithContext(inspCtx)
	inspResp, err := inspectClient.Do(inspReq)
	if err != nil {
		return nil
	}
	defer inspResp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(inspResp.Body, 4096))
	if err != nil || inspResp.StatusCode != http.StatusOK {
		return nil
	}
	var info struct {
		State struct {
			Status            string `json:"Status"`
			Running           bool   `json:"Running"`
			Restarting        bool   `json:"Restarting"`
			ExitCode          int    `json:"ExitCode"`
			Error             string `json:"Error"`
			FinishedAt        string `json:"FinishedAt"`
			Health            struct {
				Status string `json:"Status"`
			} `json:"Health"`
		} `json:"State"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return nil
	}
	if info.State.Running {
		return nil
	}
	if info.State.Restarting {
		// Docker is actively restarting the container on its own; do not
		// double-trigger or count this as a crash for our breaker.
		return nil
	}
	state := strings.TrimSpace(info.State.Status)
	if state == "" {
		state = "exited"
	}
	return fmt.Errorf("%w: container=%s state=%s exit_code=%d err=%q finished_at=%s",
		ErrPostRestartDown, nameOrID, state, info.State.ExitCode, info.State.Error, info.State.FinishedAt)
}

func restartSystemdService(service string) error {
	if service == "" {
		return fmt.Errorf("systemd service name required")
	}
	// ponytail: relies on local systemctl binary with 5s timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "systemctl", "restart", service)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl restart %s failed: %v, out: %s", service, err, string(out))
	}
	log.Printf("[auto-heal] Successfully restarted systemd service: %s", service)
	return nil
}
