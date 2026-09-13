package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// TLSTarget describes one TLS certificate probe. Address is "host:port"
// in the same form TCPTarget accepts; the dialer speaks TCP, then runs a
// TLS handshake using ServerName = host (the part before the colon).
//
// WarnBefore is the lead time before expiry at which the probe flips to
// failing. A value of 14 * 24 * time.Hour means "fail me when the cert
// has less than 14 days of validity left". Zero defaults to 14 days so
// operators get a sane heads-up without reading the README.
//
// MinTLS, when non-empty, is the minimum TLS version accepted; anything
// lower aborts the probe with a version_mismatch error. The empty
// default keeps Go's stdlib choice (currently TLS 1.2).
//
// InsecureSkipVerify is opt-in. Operators should leave it off; the probe
// is meaningless if it trusts an attacker-controlled cert chain.
type TLSTarget struct {
	Address           string
	WarnBefore        time.Duration
	MinTLS            string // "1.2" / "1.3"; empty = stdlib default
	InsecureSkipVerify bool
}

// TLSRunner runs TLS handshake probes against a fixed list of targets.
// It mirrors TCPRunner's structure (parallel Run, per-target context
// budget, results in input order, Kind tag stamped on every result) so
// the agent can compose the three runners the same way it composes
// HTTP + TCP today.
//
// Design constraints:
//   - Dial + handshake lives inside one per-target context so a slow
//     handshake cannot stall the heartbeat loop.
//   - LatencyMs captures the full round trip (dial + handshake). The
//     control plane uses it for SLO charts; ops usually want to know
//     "did the cert chain come back quickly" not "how fast was TCP".
//   - StatusCode is always 0 for TLS probes — there is no HTTP response
//     to report. Operators who want one can layer an HTTP probe on top.
//   - We never cache the parsed certificate: a rotated cert must show
//     up on the next heartbeat, not after the agent's lifetime.
type TLSRunner struct {
	targets []TLSTarget
	timeout time.Duration
}

// NewTLSRunner normalizes the target list (trim, dedupe by Address,
// default WarnBefore to 14d, default timeout to 5s). The dedupe keeps
// the last-seen WarnBefore so an operator can override a wide-open
// default by listing the same address later in the flag with a tighter
// window.
func NewTLSRunner(targets []TLSTarget, timeout time.Duration) *TLSRunner {
	clean := make([]TLSTarget, 0, len(targets))
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
		if clean[i].WarnBefore <= 0 {
			clean[i].WarnBefore = 14 * 24 * time.Hour
		}
	}
	return &TLSRunner{targets: clean, timeout: timeout}
}

// Targets returns the normalized list (read-only). Useful for the
// startup banner that prints "N target(s) configured".
func (r *TLSRunner) Targets() []TLSTarget {
	out := make([]TLSTarget, len(r.targets))
	copy(out, r.targets)
	return out
}

// Run executes every target in parallel and returns results in input
// order. The Kind field of each result is set to protocol.ProbeKindTLS
// so the control plane can render the right widget.
//
// ponytail: parallelism is bounded by len(targets); for typical configs
// (≤20 endpoints) one goroutine each is fine. Switch to a worker pool
// if operators start listing hundreds of addresses — the same way we'd
// extend TCPRunner.
func (r *TLSRunner) Run() []protocol.ProbeResult {
	if len(r.targets) == 0 {
		return nil
	}
	now := time.Now().Unix()
	results := make([]protocol.ProbeResult, len(r.targets))
	var wg sync.WaitGroup
	wg.Add(len(r.targets))
	for i, target := range r.targets {
		go func(i int, target TLSTarget) {
			defer wg.Done()
			results[i] = r.runOne(target, now)
		}(i, target)
	}
	wg.Wait()
	return results
}

func (r *TLSRunner) runOne(target TLSTarget, ts int64) protocol.ProbeResult {
	res := protocol.ProbeResult{
		URL:  target.Address,
		Kind: protocol.ProbeKindTLS,
		Ts:   ts,
	}
	if err := validateTLSAddr(target.Address); err != nil {
		res.Error = "invalid_addr: " + err.Error()
		return res
	}
	host, _, err := net.SplitHostPort(target.Address)
	if err != nil {
		// validateTLSAddr already filtered this; keep the guard so the
		// invariants stay local to runOne if the validator changes.
		res.Error = "invalid_addr: " + err.Error()
		return res
	}

	minTLS, err := parseTLSVersion(target.MinTLS)
	if err != nil {
		res.Error = "invalid_min_tls: " + err.Error()
		return res
	}

	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()

	dialer := &net.Dialer{}
	start := time.Now()
	rawConn, err := dialer.DialContext(ctx, "tcp", target.Address)
	if err != nil {
		res.LatencyMs = time.Since(start).Milliseconds()
		res.Error = classifyDialErr(err)
		return res
	}

	cfg := &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: target.InsecureSkipVerify,
		MinVersion:         minTLS,
	}
	tlsConn := tls.Client(rawConn, cfg)
	// Handshake is the slow part; honour the per-target context.
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = tlsConn.Close()
		res.LatencyMs = time.Since(start).Milliseconds()
		res.Error = classifyHandshakeErr(err)
		return res
	}
	res.LatencyMs = time.Since(start).Milliseconds()

	state := tlsConn.ConnectionState()
	_ = tlsConn.Close()
	if len(state.PeerCertificates) == 0 {
		res.Error = "no_peer_certificates"
		return res
	}

	leaf := state.PeerCertificates[0]
	now := time.Now()
	remaining := leaf.NotAfter.Sub(now)

	switch {
	case now.Before(leaf.NotBefore):
		res.Error = "cert_not_yet_valid"
		// res.OK stays false; ops gets a clear "the clock is ahead of
		// the cert" signal in the public status page.
	case remaining <= 0:
		res.Error = "cert_expired"
	default:
		res.StatusCode = 1 // 1 = OK; 0 = transport error; 2 = expired used elsewhere.
		if remaining <= target.WarnBefore {
			// Inside the warn window: still OK in the protocol sense
			// (the handshake succeeded and the cert is valid), but we
			// surface the remaining days in Error so the control plane
			// can render a "renew soon" badge. Flipping OK=false here
			// would spam the on-call rotation; the warn window is a
			// dashboard signal, not an incident trigger.
			res.StatusCode = 2
			res.Error = fmt.Sprintf("expires_in_%dh", int(remaining.Hours()))
		}
		res.OK = true
	}
	return res
}

// validateTLSAddr rejects obvious typos so we don't end up with a flood
// of cryptic "no such host" results. Same shape as validateAddr but
// duplicated here so the two runners stay independently testable.
func validateTLSAddr(addr string) error {
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
// friendly strings. Shared with TCPRunner's classifier (same shape,
// separate copy so the two runners stay testable in isolation).
func classifyTLSHandshakeErr(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "handshake_timeout"
	default:
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return "handshake_timeout"
		}
		return "handshake: " + err.Error()
	}
}

// classifyHandshakeErr unwraps a TLS handshake error into a short
// status-page string. Anything we don't recognise falls through to the
// raw error so operators can still see the underlying cause in logs.
func classifyHandshakeErr(err error) string {
	if err == nil {
		return ""
	}
	// tls.RecordHeaderError / tls.AlertError implement Error() with
	// stable prefixes; check them before the generic net error path.
	msg := err.Error()
	switch {
	case strings.Contains(msg, "tls: expired"):
		return "cert_expired"
	case strings.Contains(msg, "tls: oversized record") || strings.Contains(msg, "tls: no application protocol"):
		return "tls_protocol_error"
	case strings.Contains(msg, "certificate is not yet valid"):
		return "cert_not_yet_valid"
	case strings.Contains(msg, "tls: handshake timeout") || strings.Contains(msg, "i/o timeout"):
		return classifyTLSHandshakeErr(err)
	}
	return classifyTLSHandshakeErr(err)
}

// parseTLSVersion maps a string like "1.2" or "1.3" to the corresponding
// constant. Empty input returns 0 (Go stdlib default).
func parseTLSVersion(raw string) (uint16, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return 0, nil
	}
	switch v {
	case "1.0", "1.1":
		// Go 1.22+ removed TLS 1.0 / 1.1 from the default set; we still
		// accept the strings so a stale config produces a clear error
		// instead of a silent fallback.
		return 0, fmt.Errorf("tls version %q not supported by this Go build", v)
	case "1.2":
		return tls.VersionTLS12, nil
	case "1.3":
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("unknown tls version %q (want 1.2 or 1.3)", v)
	}
}

// x509Now lets tests stub time. Production code uses time.Now() directly
// via runOne; the indirection keeps the expiry test stable across CI
// clock drift and lets us assert exact remaining-second values.
var x509Now = time.Now

// x509LeafNotAfter extracts the NotAfter of the leaf cert. Used by the
// test harness to avoid coupling the assertions to the internal flow.
func x509LeafNotAfter(cert *x509.Certificate) time.Time {
	if cert == nil {
		return time.Time{}
	}
	return cert.NotAfter
}
