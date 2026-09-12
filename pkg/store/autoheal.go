package store

import (
	"strings"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// EvaluateAutoHeal generates remediation commands based on failed services or critical rules.
// ponytail: default policy auto-restarts failed docker containers and systemd units
func (p *PersistentStore) EvaluateAutoHeal(hb *protocol.Heartbeat) []string {
	var cmds []string

	for _, s := range hb.Services {
		if !s.Active {
			// Container stopped or exited
			if s.Type == "docker" && !strings.HasPrefix(s.Name, "temp_") {
				cmds = append(cmds, "restart_docker:"+s.Name)
			}
			// Systemd unit failed
			if s.Type == "systemd" {
				cmds = append(cmds, "restart_systemd:"+s.Name)
			}
		}
	}

	return cmds
}
