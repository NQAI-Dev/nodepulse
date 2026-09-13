package collector

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

type dockerContainer struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Created int64             `json:"Created"`
	Labels  map[string]string `json:"Labels"`
}

func CollectDockerServices() []protocol.ServiceStatus {
	sock := "/var/run/docker.sock"
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sock)
			},
		},
		Timeout: 2 * time.Second,
	}

	resp, err := client.Get("http://localhost/containers/json?all=1")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	var raw []dockerContainer
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil
	}

	services := make([]protocol.ServiceStatus, 0, len(raw))
	for _, c := range raw {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		} else if len(c.ID) >= 12 {
			name = c.ID[:12]
		}
		active := c.State == "running"
		services = append(services, protocol.ServiceStatus{
			Name:    name,
			Type:    "docker",
			Active:  active,
			Status:  c.Status,
			Message: c.Image,
			Labels:  c.Labels,
		})
	}
	return services
}
