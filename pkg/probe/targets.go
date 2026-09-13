package probe

import (
	"fmt"
	"strings"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// ParseTCPTargets parses the operator-facing `-probe-tcp` flag value into
// a normalized TCPTarget slice. The accepted shapes are:
//
//   - "host:port" — open port check
//   - "host:port=banner" — also require the first banner line to contain
//     "banner". Use "\n" inside the value if a multi-line banner is ever
//     needed (not implemented today: we read one line, full stop).
//
// Empty entries are dropped. Leading/trailing whitespace is trimmed.
// Duplicate host:port pairs are deduped by ParseTCPTargets callers
// (NewTCPRunner) so the banner-wins-on-duplicate rule stays in one
// place.
//
// ponytail: this is intentionally stricter than the HTTP probe parser —
// the value can carry a banner spec, which means we need to be picky
// about the delimiter ("=") and the syntax. If operators ask for a
// less-cryptic format later, add a sibling helper rather than overload
// this one.
func ParseTCPTargets(raw string) ([]TCPTarget, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]TCPTarget, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		addr, banner, _ := strings.Cut(p, "=")
		addr = strings.TrimSpace(addr)
		banner = strings.TrimSpace(banner)
		if addr == "" {
			return nil, fmt.Errorf("probe target %q: empty host:port", p)
		}
		out = append(out, TCPTarget{Address: addr, ExpectBanner: banner})
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// MergeProbeResults concatenates HTTP and TCP probe slices while
// preserving each result's Kind tag. Callers (the agent) hand both
// slices to the control plane on every heartbeat. Order is HTTP first,
// then TCP, so the public status page renders them in the natural
// "synthetic, then external-dependency" reading order.
//
// If either side is empty the function is allocation-free.
func MergeProbeResults(httpResults, tcpResults []protocol.ProbeResult) []protocol.ProbeResult {
	if len(httpResults) == 0 && len(tcpResults) == 0 {
		return nil
	}
	out := make([]protocol.ProbeResult, 0, len(httpResults)+len(tcpResults))
	out = append(out, httpResults...)
	out = append(out, tcpResults...)
	return out
}
