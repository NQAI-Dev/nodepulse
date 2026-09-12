package collector

import (
	"os"
	"strconv"
	"strings"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// CollectNetworkInterfaces parses /proc/net/dev and returns per-interface
// counters. Lo and lo-only backplanes are filtered because operators
// rarely care about localhost chatter.
//
// ponytail: we keep this stateless and side-effect-free so a future
// "collect every 1s" path can hot-call it; if/when per-second rates
// become a primary view, cache the previous snapshot inside the
// collector struct and emit deltas here.
func CollectNetworkInterfaces() []protocol.NetStats {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil
	}
	defer f.Close()

	data := make([]byte, 0, 8192)
	buf := make([]byte, 4096)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			data = append(data, buf[:n]...)
		}
		if err != nil {
			break
		}
	}
	return parseProcNetDev(data)
}

// parseProcNetDev is the testable inner parser. It strips the header
// lines (everything before the first iface: row) and parses each
// "iface: rx_c1 rx_c2 ... tx_c1 ..." row.
//
// /proc/net/dev layout per iface, after the colon:
//   rx_bytes rx_packets rx_errs rx_drop rx_fifo rx_frame rx_compressed rx_multicast
//   tx_bytes tx_packets tx_errs tx_drop tx_fifo tx_colls tx_carrier tx_compressed
// We tolerate shorter rows (older kernels / containers) by leaving
// the missing tx counters at zero.
func parseProcNetDev(data []byte) []protocol.NetStats {
	out := make([]protocol.NetStats, 0, 8)
	for _, line := range strings.Split(string(data), "\n") {
		colon := strings.LastIndex(line, ":")
		if colon < 0 {
			continue
		}
		iface := strings.TrimSpace(line[:colon])
		if iface == "" || iface == "lo" {
			continue
		}
		fields := strings.Fields(line[colon+1:])
		if len(fields) < 4 {
			continue
		}
		ns := protocol.NetStats{Iface: iface}
		ns.RxBytes, _ = strconv.ParseUint(fields[0], 10, 64)
		ns.RxPackets, _ = strconv.ParseUint(fields[1], 10, 64)
		ns.RxErrors, _ = strconv.ParseUint(fields[2], 10, 64)
		ns.RxDrops, _ = strconv.ParseUint(fields[3], 10, 64)
		if len(fields) >= 16 {
			ns.TxBytes, _ = strconv.ParseUint(fields[8], 10, 64)
			ns.TxPackets, _ = strconv.ParseUint(fields[9], 10, 64)
			ns.TxErrors, _ = strconv.ParseUint(fields[10], 10, 64)
			ns.TxDrops, _ = strconv.ParseUint(fields[11], 10, 64)
		}
		out = append(out, ns)
	}
	return out
}
