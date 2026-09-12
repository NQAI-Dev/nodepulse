package collector

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// AutoHeal executes remediation actions returned by the control plane
func ExecuteCommand(cmd string) error {
	parts := strings.SplitN(cmd, ":", 2)
	action := parts[0]
	target := ""
	if len(parts) > 1 {
		target = parts[1]
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
	sock := "/var/run/docker.sock"
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sock)
			},
		},
		Timeout: 10 * time.Second,
	}

	url := fmt.Sprintf("http://localhost/containers/%s/restart", nameOrID)
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker restart failed with status %d", resp.StatusCode)
	}
	log.Printf("[auto-heal] Successfully restarted Docker container: %s", nameOrID)
	return nil
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
