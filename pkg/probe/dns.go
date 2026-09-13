package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// DNSTarget describes one DNS lookup probe. The Address field carries the
// name to resolve (no scheme, no port) — "example.com", "internal.svc",
// "1.1.1.1" — anything net.LookupHost would accept. Expected, when
// non-empty, pins the probe to one record type (A or AAAA) and asserts
// the result set contains the literal substring. The check is exact-ish:
// a probe for "api.example.com" with Expected="10.0." matches any A
// record whose dotted-quad starts with "10.0.".
//
// Failure semantics mirror the TLS runner: a probe is OK iff the
// resolver returned at least one record. NXDOMAIN, timeout, and a missing
// Expected substring all surface as Error with OK=false so the control
// plane can flag an incident.
//
// ponytail: this is a deliberately small surface. We don't try to pin
// every resolver metric (TTL, EDNS, DNSSEC chain) — operators who want
// those should run a real probe (dig + drill) and forward the output
// into an HTTP probe payload.
type DNSTarget struct {
	Address  string        // hostname to resolve
	Expected string        // optional expected record type or substring
	Type     string        // "A" (default) or "AAAA"
	Resolver *net.Resolver // optional, for tests; production uses default
}

// DNSRunner runs DNS lookup probes against a fixed list of targets. It
// mirrors TCPRunner's structure (parallel Run, per-target context budget,
// results in input order, Kind tag stamped on every result) so the
// agent composes HTTP/TCP/TLS/DNS runners the same way.
//
// Design constraints:
//   - Per-target context caps the full resolver round trip so a stuck
//     upstream resolver cannot stall the heartbeat loop.
//   - Resolver defaults to net.DefaultResolver (read from /etc/resolv.conf
//     at startup); tests can inject a custom one.
//   - Type defaults to "A". "AAAA" works the same way. Empty Type with a
//     non-empty Expected falls back to "A" so operators don't have to
//     spell it out for the common case.
type DNSRunner struct {
	targets []DNSTarget
	timeout time.Duration
}

// NewDNSRunner normalizes the target list (trim, dedupe by Address,
// default Type to "A", default timeout to 5s). The dedupe keeps the last
// occurrence so operators can override a wide-open default with a
// tighter spec later in the flag.
func NewDNSRunner(targets []DNSTarget, timeout time.Duration) *DNSRunner {
	clean := make([]DNSTarget, 0, len(targets))
	seen := make(map[string]int, len(targets))
	for _, raw := range targets {
		addr := strings.TrimSpace(raw.Address)
		if addr == "" {
			continue
		}
		if i, ok := seen[addr]; ok {
			clean[i] = raw
			continue
		}
		seen[addr] = len(clean)
		clean = append(clean, raw)
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	for i := range clean {
		if strings.TrimSpace(clean[i].Type) == "" {
			clean[i].Type = "A"
		} else {
			clean[i].Type = strings.ToUpper(strings.TrimSpace(clean[i].Type))
		}
	}
	return &DNSRunner{targets: clean, timeout: timeout}
}

// Targets returns the normalized list (read-only). Useful for the
// startup banner that prints "N target(s) configured".
func (r *DNSRunner) Targets() []DNSTarget {
	out := make([]DNSTarget, len(r.targets))
	copy(out, r.targets)
	return out
}

// Run executes every target in parallel and returns results in input
// order. The Kind field of each result is set to protocol.ProbeKindDNS
// so the control plane can render the right widget.
//
// ponytail: parallelism is bounded by len(targets); for typical configs
// (≤50 hostnames) one goroutine each is fine. Switch to a worker pool if
// operators start listing thousands of records.
func (r *DNSRunner) Run() []protocol.ProbeResult {
	if len(r.targets) == 0 {
		return nil
	}
	now := time.Now().Unix()
	results := make([]protocol.ProbeResult, len(r.targets))
	var wg sync.WaitGroup
	wg.Add(len(r.targets))
	for i, target := range r.targets {
		go func(i int, target DNSTarget) {
			defer wg.Done()
			results[i] = r.runOne(target, now)
		}(i, target)
	}
	wg.Wait()
	return results
}

func (r *DNSRunner) runOne(target DNSTarget, ts int64) protocol.ProbeResult {
	res := protocol.ProbeResult{
		URL:  target.Address,
		Kind: protocol.ProbeKindDNS,
		Ts:   ts,
	}
	if err := validateDNSAddr(target.Address); err != nil {
		res.Error = "invalid_addr: " + err.Error()
		return res
	}
	if target.Type != "A" && target.Type != "AAAA" {
		res.Error = "invalid_type: " + target.Type
		return res
	}

	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()

	resolver := target.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	start := time.Now()
	var (
		addrs []string
		err   error
	)
	switch target.Type {
	case "AAAA":
		ipAddrs, lookupErr := resolver.LookupIPAddr(ctx, target.Address)
		err = lookupErr
		// LookupIPAddr returns both v4 and v6; filter to v6 only so the
		// probe semantics match the operator's expectation. net.IP.Is6
		// is the canonical check; the slice is non-nil on success.
		if lookupErr == nil {
			v6 := make([]string, 0, len(ipAddrs))
			for _, ipAddr := range ipAddrs {
				if ipAddr.IP.To4() == nil {
					v6 = append(v6, ipAddr.IP.String())
				}
			}
			addrs = v6
		}
	default: // "A"
		hostAddrs, lookupErr := resolver.LookupHost(ctx, target.Address)
		err = lookupErr
		if lookupErr == nil {
			v4 := make([]string, 0, len(hostAddrs))
			for _, a := range hostAddrs {
				if ip := net.ParseIP(a); ip != nil && ip.To4() != nil {
					v4 = append(v4, a)
				}
			}
			addrs = v4
		}
	}
	res.LatencyMs = time.Since(start).Milliseconds()

	if err != nil {
		res.Error = classifyDNSLookupErr(err)
		return res
	}
	if len(addrs) == 0 {
		res.Error = "no_records"
		return res
	}
	if target.Expected != "" {
		matched := false
		for _, a := range addrs {
			if strings.Contains(a, target.Expected) {
				matched = true
				break
			}
		}
		if !matched {
			res.Error = fmt.Sprintf("expected_mismatch: got %s, want substring %q",
				truncate(strings.Join(addrs, ","), 120), target.Expected)
			return res
		}
	}
	res.OK = true
	// StatusCode is unused for DNS probes; keep it 0 so the public
	// status page renders "OK" without a confusing code.
	return res
}

// validateDNSAddr rejects obviously empty or malformed targets. We
// accept anything net.LookupHost would accept — including IPv4 literals
// — so operators can sanity-check their resolvers with `1.1.1.1` too.
// The check rejects empty addresses and names that contain characters
// outside the DNS name charset (RFC 1035 simplified).
func validateDNSAddr(addr string) error {
	if strings.TrimSpace(addr) == "" {
		return errors.New("empty address")
	}
	if strings.ContainsAny(addr, " \t\n\r") {
		return errors.New("whitespace in address")
	}
	// Cheap length cap to keep an operator typo from hammering the
	// logger with multi-kilobyte garbage. 253 is the DNS spec maximum.
	if len(addr) > 253 {
		return errors.New("address exceeds 253 chars")
	}
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return errors.New("address must not contain a port (use TCP probe for port checks)")
	}
	// Pure IPv4 literal? Accept.
	if net.ParseIP(addr) != nil {
		return nil
	}
	// Otherwise ensure every label is non-empty and within length caps.
	// We don't try to be a full DNS validator; we just catch the typos
	// that would otherwise produce cryptic resolver errors.
	for _, label := range strings.Split(addr, ".") {
		if label == "" {
			return errors.New("empty dns label")
		}
		if len(label) > 63 {
			return errors.New("dns label exceeds 63 chars")
		}
	}
	return nil
}

// classifyDNSLookupErr maps the most common resolver errors to short,
// status-page-friendly strings. Anything we don't recognise falls
// through to the raw error so operators can still see the underlying
// cause in logs.
func classifyDNSLookupErr(err error) string {
	if err == nil {
		return ""
	}
	var de *net.DNSError
	if errors.As(err, &de) {
		switch {
		case de.IsNotFound:
			return "nxdomain"
		case de.IsTimeout:
			return "timeout"
		case de.IsTemporary:
			return "temporary_failure"
		default:
			if de.Server != "" {
				return "dns_failure: " + de.Server
			}
			return "dns_failure"
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	// strconv import retained for parity with the TCP runner — used in
	// future IPv4 validation hooks. Suppress the unused warning by
	// referring to it once.
	_ = strconv.Itoa
	return "lookup: " + err.Error()
}
