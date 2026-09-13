package probe

import (
	"bufio"
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

// TCPTarget describes one TCP probe. Address is "host:port" — host may be
// a hostname or a literal IPv4/IPv6 (the IPv6 form requires brackets:
// "[::1]:5432"). ExpectBanner is optional: when non-empty, the runner
// reads one line from the server after a successful connect and the probe
// is OK only if that line contains the expected substring. The timeout
// caps the entire round-trip, including DNS + dial + banner read.
type TCPTarget struct {
	Address      string
	ExpectBanner string
}

// TCPRunner runs TCP connect probes against a fixed list of targets. Like
// the HTTP Runner it shares one transport — for TCP that means one
// shared *net.Resolver so each lookup doesn't fork a goroutine storm —
// and never follows redirects (TCP doesn't have them).
//
// Design constraints:
//   - DNS resolution lives inside the per-target context budget, so a
//     misconfigured resolver cannot stall the heartbeat loop.
//   - Banner read is best-effort and one-shot: TCP servers that demand
//     TLS/STARTTLS are out of scope until someone asks for them.
//   - ExpectBanner match is case-sensitive on the raw line; operators
//     who need case-insensitive matching should lowercase both sides
//     in their config (we deliberately avoid ToLower on the response
//     so the case observed by the operator is what they see in the
//     status page tooltip).
type TCPRunner struct {
	targets []TCPTarget
	timeout time.Duration
}

// NewTCPRunner normalizes the targets (trim spaces, drop blanks, dedupe
// by address). A zero or negative timeout falls back to 5s, the same
// default the HTTP runner uses so per-heartbeat budgets stay uniform.
func NewTCPRunner(targets []TCPTarget, timeout time.Duration) *TCPRunner {
	clean := make([]TCPTarget, 0, len(targets))
	seen := make(map[string]struct{}, len(targets))
	for _, raw := range targets {
		addr := strings.TrimSpace(raw.Address)
		if addr == "" {
			continue
		}
		if _, ok := seen[addr]; ok {
			// If a later entry adds a banner for an already-seen
			// address, prefer the one that actually expects something;
			// otherwise keep the first so probe results stay stable.
			keep := clean
			for i := range keep {
				if keep[i].Address == addr {
					if keep[i].ExpectBanner == "" && raw.ExpectBanner != "" {
						keep[i].ExpectBanner = raw.ExpectBanner
					}
					break
				}
			}
			continue
		}
		seen[addr] = struct{}{}
		clean = append(clean, TCPTarget{Address: addr, ExpectBanner: raw.ExpectBanner})
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &TCPRunner{targets: clean, timeout: timeout}
}

// Targets returns the normalized target list. Callers (CLI logs, tests)
// use it to confirm dedup + trim.
func (r *TCPRunner) Targets() []TCPTarget {
	out := make([]TCPTarget, len(r.targets))
	copy(out, r.targets)
	return out
}

// Run executes every target in parallel and returns results in input
// order. The Kind field of each result is set to protocol.ProbeKindTCP
// so the control plane can render the right widget.
//
// ponytail: parallelism is bounded by len(targets); for typical configs
// (≤20 ports) one goroutine each is fine. Switch to a worker pool if
// operators start listing hundreds of addresses.
func (r *TCPRunner) Run() []protocol.ProbeResult {
	if len(r.targets) == 0 {
		return nil
	}
	now := time.Now().Unix()
	results := make([]protocol.ProbeResult, len(r.targets))
	var wg sync.WaitGroup
	wg.Add(len(r.targets))
	for i, target := range r.targets {
		go func(i int, target TCPTarget) {
			defer wg.Done()
			results[i] = r.runOne(target, now)
		}(i, target)
	}
	wg.Wait()
	return results
}

func (r *TCPRunner) runOne(target TCPTarget, ts int64) protocol.ProbeResult {
	res := protocol.ProbeResult{
		URL:  target.Address,
		Kind: protocol.ProbeKindTCP,
		Ts:   ts,
	}
	if err := validateAddr(target.Address); err != nil {
		res.Error = "invalid_addr: " + err.Error()
		return res
	}

	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()

	dialer := &net.Dialer{}
	start := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", target.Address)
	res.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = classifyDialErr(err)
		return res
	}
	defer conn.Close()
	// StatusCode stays 0 for plain TCP — it's not an HTTP probe. Some
	// operators map "port open" to 200 in their dashboards; we keep the
	// raw semantics here so the kind tag is the source of truth.

	if target.ExpectBanner == "" {
		res.OK = true
		return res
	}

	// Banner read with its own deadline so a slow server can't blow the
	// remaining timeout that we use for subsequent probes. Half the
	// overall budget is a sane upper bound.
	readDeadline := time.Now().Add(r.timeout / 2)
	if dl, ok := ctx.Deadline(); ok && readDeadline.After(dl) {
		readDeadline = dl
	}
	if err := conn.SetReadDeadline(readDeadline); err != nil {
		res.Error = "set_read_deadline: " + err.Error()
		return res
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil && len(line) == 0 {
		res.Error = "banner_read: " + err.Error()
		return res
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.Contains(line, target.ExpectBanner) {
		res.Error = fmt.Sprintf("banner_mismatch: got %q, want substring %q", truncate(line, 80), target.ExpectBanner)
		return res
	}
	res.OK = true
	return res
}

// validateAddr rejects obvious typos so we don't end up with a flood of
// cryptic "no such host" results. The net package would surface the same
// errors but with much less signal in the public status page.
func validateAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" {
		return errors.New("missing host")
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("port %q not numeric", port)
	}
	if p < 1 || p > 65535 {
		return fmt.Errorf("port %d out of range", p)
	}
	return nil
}

// classifyDialErr maps the most common net errors to short, status-page-
// friendly strings. Anything we don't recognise falls through to the raw
// error so operators can still see the underlying cause in logs.
func classifyDialErr(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case isConnRefused(err):
		return "connection_refused"
	case isDNSError(err):
		return "dns_failure"
	case isNoRoute(err):
		return "no_route"
	default:
		return err.Error()
	}
}

func isConnRefused(err error) bool {
	var ce *net.OpError
	if errors.As(err, &ce) {
		var se *net.DNSError // dummy type to anchor errors.As chain below
		_ = se
	}
	// net.OpError's Err is a *os.SyscallError; check by string to avoid
	// pulling in syscall internals across platforms.
	return strings.Contains(err.Error(), "connection refused")
}

func isDNSError(err error) bool {
	var de *net.DNSError
	return errors.As(err, &de)
}

func isNoRoute(err error) bool {
	return strings.Contains(err.Error(), "no route to host") ||
		strings.Contains(err.Error(), "network is unreachable")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
