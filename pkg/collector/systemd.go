package collector

import (
	"os/exec"
	"strings"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func CollectSystemdServices(units []string) []protocol.ServiceStatus {
	if len(units) == 0 {
		return nil
	}
	res := make([]protocol.ServiceStatus, 0, len(units))
	for _, u := range units {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		out, err := exec.Command("systemctl", "is-active", u).Output()
		statusStr := strings.TrimSpace(string(out))
		active := err == nil && statusStr == "active"
		res = append(res, protocol.ServiceStatus{
			Name:   u,
			Type:   "systemd",
			Active: active,
			Status: statusStr,
		})
	}
	return res
}
