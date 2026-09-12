package collector

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

type Collector struct {
	nodeID        string
	monitoredUnits []string
}

func New(nodeID string, monitoredUnits []string) *Collector {
	return &Collector{
		nodeID:        nodeID,
		monitoredUnits: monitoredUnits,
	}
}

func (c *Collector) Collect() (*protocol.Heartbeat, error) {
	hostname, _ := os.Hostname()

	hb := &protocol.Heartbeat{
		NodeID:    c.nodeID,
		Timestamp: time.Now().Unix(),
		Node: protocol.NodeInfo{
			ID:       c.nodeID,
			Hostname: hostname,
			OS:       runtime.GOOS,
			Arch:     runtime.GOARCH,
			Version:  "0.3.0-enterprise",
		},
		CPU: protocol.CPUStats{
			Cores: runtime.NumCPU(),
		},
	}

	var si syscall.Sysinfo_t
	if err := syscall.Sysinfo(&si); err == nil {
		hb.CPU.Load1 = float64(si.Loads[0]) / 65536.0
		hb.CPU.Load5 = float64(si.Loads[1]) / 65536.0
		hb.CPU.Load15 = float64(si.Loads[2]) / 65536.0
	}

	if f, err := os.Open("/proc/meminfo"); err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) >= 2 {
				val, _ := strconv.ParseUint(fields[1], 10, 64)
				valBytes := val * 1024
				switch fields[0] {
				case "MemTotal:":
					hb.Memory.TotalBytes = valBytes
				case "MemAvailable:":
					hb.Memory.AvailableBytes = valBytes
				}
			}
		}
		if hb.Memory.TotalBytes > 0 {
			hb.Memory.UsedBytes = hb.Memory.TotalBytes - hb.Memory.AvailableBytes
			hb.Memory.UsedPercent = (float64(hb.Memory.UsedBytes) / float64(hb.Memory.TotalBytes)) * 100.0
		}
	}

	for _, mnt := range []string{"/"} {
		var fs syscall.Statfs_t
		if err := syscall.Statfs(mnt, &fs); err == nil {
			total := fs.Blocks * uint64(fs.Bsize)
			free := fs.Bavail * uint64(fs.Bsize)
			var usedPct float64
			if total > 0 {
				usedPct = (float64(total-free) / float64(total)) * 100.0
			}
			hb.Disks = append(hb.Disks, protocol.DiskStats{
				MountPoint:  mnt,
				TotalBytes:  total,
				FreeBytes:   free,
				UsedPercent: usedPct,
			})
		}
	}

	// Collect Docker containers
	if dockerServices := CollectDockerServices(); len(dockerServices) > 0 {
		hb.Services = append(hb.Services, dockerServices...)
	}

	// Collect network interface counters (lo filtered).
	if net := CollectNetworkInterfaces(); len(net) > 0 {
		hb.Network = net
	}

	// Collect requested systemd units
	if systemdServices := CollectSystemdServices(c.monitoredUnits); len(systemdServices) > 0 {
		hb.Services = append(hb.Services, systemdServices...)
	}

	return hb, nil
}
