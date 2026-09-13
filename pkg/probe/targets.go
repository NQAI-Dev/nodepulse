package probe

import (
	"fmt"
	"strconv"
	"strings"
	"time"

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

// ParseTLSTargets parses the operator-facing `-probe-tls` flag value
// into a normalized TLSTarget slice. The accepted shapes are:
//
//   - "host:port" — handshake + 14-day default warn window
//   - "host:port=Nd" — handshake + custom warn window (Nd / Nh / Nm;
//     d = days, h = hours, m = minutes). Example: "api.example.com:443=30d".
//   - "host:port=Nd:1.3" — also pin a minimum TLS version. The colon
//     separates warn window from min-tls so the value stays on one line.
//   - "host:port=Nd:1.3:insecure" — opt into InsecureSkipVerify. Off by
//     default; included for symmetry with the other knobs even though
//     we don't recommend it.
//
// Empty entries are dropped. Leading/trailing whitespace is trimmed.
//
// ponytail: same delimiter philosophy as ParseTCPTargets. If operators
// ask for a richer config (per-target label, multi-window), promote
// this to a JSON list rather than overload the flag.
func ParseTLSTargets(raw string) ([]TLSTarget, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]TLSTarget, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		addr, rest, _ := strings.Cut(p, "=")
		addr = strings.TrimSpace(addr)
		if addr == "" || !strings.Contains(addr, ":") {
			return nil, fmt.Errorf("tls probe target %q: empty host:port", p)
		}
		t := TLSTarget{Address: addr}
		if rest == "" {
			out = append(out, t)
			continue
		}
		segments := strings.Split(rest, ":")
		warnRaw := strings.TrimSpace(segments[0])
		if warnRaw != "" {
			dur, err := parseWarnWindow(warnRaw)
			if err != nil {
				return nil, fmt.Errorf("tls probe target %q: %w", p, err)
			}
			t.WarnBefore = dur
		}
		if len(segments) >= 2 {
			t.MinTLS = strings.TrimSpace(segments[1])
		}
		if len(segments) >= 3 && strings.EqualFold(strings.TrimSpace(segments[2]), "insecure") {
			t.InsecureSkipVerify = true
		}
		if len(segments) > 3 {
			return nil, fmt.Errorf("tls probe target %q: too many segments after host:port", p)
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// parseWarnWindow accepts a small suffix grammar: a positive integer
// followed by d/h/m (days / hours / minutes). Anything else returns an
// error so the agent's startup log flags the bad config instead of
// silently defaulting to 14d.
func parseWarnWindow(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, fmt.Errorf("empty warn window")
	}
	last := raw[len(raw)-1]
	num := raw[:len(raw)-1]
	n, err := strconv.Atoi(num)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("warn window %q: must be a non-negative integer followed by d/h/m", raw)
	}
	switch last {
	case 'd':
		return time.Duration(n) * 24 * time.Hour, nil
	case 'h':
		return time.Duration(n) * time.Hour, nil
	case 'm':
		return time.Duration(n) * time.Minute, nil
	default:
		return 0, fmt.Errorf("warn window %q: suffix must be d, h, or m", raw)
	}
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

// MergeProbeResultsAll concatenates HTTP, TCP, and TLS probe slices
// while preserving each result's Kind tag. Order is HTTP first, then
// TCP, then TLS, so the public status page renders them in the natural
// "synthetic, then external-dependency, then certificate health"
// reading order.
//
// If all sides are empty the function is allocation-free.
func MergeProbeResultsAll(httpResults, tcpResults, tlsResults []protocol.ProbeResult) []protocol.ProbeResult {
	if len(httpResults) == 0 && len(tcpResults) == 0 && len(tlsResults) == 0 {
		return nil
	}
	out := make([]protocol.ProbeResult, 0, len(httpResults)+len(tcpResults)+len(tlsResults))
	out = append(out, httpResults...)
	out = append(out, tcpResults...)
	out = append(out, tlsResults...)
	return out
}
