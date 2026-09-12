// Package metrics renders the server fleet's snapshot as Prometheus exposition
// format. No external deps: plain fmt.Fprintf against text/plain spec.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/store"
)

// WriteFleet renders every node + open incident as Prometheus metrics.
// ponytail: zero-allocation paths here would matter only at >10k heartbeats/s;
// current scale is single-digit nodes. Add sync.Pool of byte buffers when
// fleet crosses 200 nodes or scrape interval drops below 5s.
func WriteFleet(w io.Writer, s *store.PersistentStore, serverStart time.Time) error {
	all := s.GetAll()
	nodes := make([]string, 0, len(all))
	for id := range all {
		nodes = append(nodes, id)
	}
	sort.Strings(nodes)

	if _, err := fmt.Fprintln(w, "# HELP nodepulse_build_info NodePulse platform build info"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "# TYPE nodepulse_build_info gauge\nnodepulse_build_info{component=\"server\"} 1\n"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "# HELP nodepulse_up_seconds Seconds since the server process started\n"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "# TYPE nodepulse_up_seconds gauge\nnodepulse_up_seconds %d\n", int64(time.Since(serverStart).Seconds())); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(w, "# HELP nodepulse_node_info Per-node static info"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# TYPE nodepulse_node_info gauge"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# HELP nodepulse_node_status Node status: 1=online, 2=warning, 3=offline"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# TYPE nodepulse_node_status gauge"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# HELP nodepulse_node_last_heartbeat_timestamp_seconds Unix time of the last heartbeat"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# TYPE nodepulse_node_last_heartbeat_timestamp_seconds gauge"); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(w, "# HELP nodepulse_node_load1 1-minute load average"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# TYPE nodepulse_node_load1 gauge"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# HELP nodepulse_node_cores CPU core count"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# TYPE nodepulse_node_cores gauge"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# HELP nodepulse_node_memory_used_percent Node RAM used percent (0-100)"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# TYPE nodepulse_node_memory_used_percent gauge"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# HELP nodepulse_node_disk_used_percent Primary filesystem used percent"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# TYPE nodepulse_node_disk_used_percent gauge"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# HELP nodepulse_node_service_up Container/service status (1=up, 0=down)"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# TYPE nodepulse_node_service_up gauge"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# HELP nodepulse_incidents_active Total unresolved incidents in the fleet"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# TYPE nodepulse_incidents_active gauge"); err != nil {
		return err
	}

	for _, id := range nodes {
		st := all[id]
		status := statusToCode(st.Status)
		ts := st.LastHeartbeat.Unix()
		memPct := st.Latest.Memory.UsedPercent
		diskPct := 0.0
		if len(st.Latest.Disks) > 0 {
			diskPct = st.Latest.Disks[0].UsedPercent
		}
		infoLabels := fmt.Sprintf(`host="%s",os="%s",version="%s",arch="%s"`,
			escape(id), escape(st.Info.OS), escape(st.Info.Version), escape(st.Info.Arch))

		fmt.Fprintf(w, "nodepulse_node_info{%s} 1\n", infoLabels)
		fmt.Fprintf(w, "nodepulse_node_status{node=%q} %d\n", id, status)
		fmt.Fprintf(w, "nodepulse_node_last_heartbeat_timestamp_seconds{node=%q} %d\n", id, ts)
		fmt.Fprintf(w, "nodepulse_node_load1{node=%q} %.3f\n", id, st.Latest.CPU.Load1)
		fmt.Fprintf(w, "nodepulse_node_cores{node=%q} %d\n", id, st.Latest.CPU.Cores)
		fmt.Fprintf(w, "nodepulse_node_memory_used_percent{node=%q} %.2f\n", id, memPct)
		fmt.Fprintf(w, "nodepulse_node_disk_used_percent{node=%q} %.2f\n", id, diskPct)

		svcNames := make([]string, 0, len(st.Latest.Services))
		for _, s := range st.Latest.Services {
			svcNames = append(svcNames, s.Name)
		}
		sort.Strings(svcNames)
		for _, sname := range svcNames {
			for _, s := range st.Latest.Services {
				if s.Name != sname {
					continue
				}
				up := 0
				if s.Active {
					up = 1
				}
				labels := fmt.Sprintf(`node=%q,service=%q,type="%s"`, id, sname, escape(s.Type))
				fmt.Fprintf(w, "nodepulse_node_service_up{%s} %d\n", labels, up)
			}
		}
	}

	inc := s.GetActiveIncidents(1)
	fmt.Fprintf(w, "nodepulse_incidents_active %d\n", len(inc))
	return nil
}

func statusToCode(s string) int {
	switch s {
	case "online":
		return 1
	case "warning":
		return 2
	case "offline":
		return 3
	}
	return 0
}

func escape(v string) string {
	out := make([]byte, 0, len(v))
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch c {
		case '\\':
			out = append(out, '\\', '\\')
		case '"':
			out = append(out, '\\', '"')
		case '\n':
			out = append(out, '\\', 'n')
		default:
			out = append(out, c)
		}
	}
	return string(out)
}
