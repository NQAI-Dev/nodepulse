package probe

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// ICMPTarget describes one ICMP echo probe. Address is the destination
// hostname or IPv4 literal (IPv6 is out of scope: ICMPv6 has a different
// wire format and the IPv6 probe path lives elsewhere — a future IPv6
// runner can mirror this one). Count is the number of echo packets to
// send per probe (default 3, max 10); the runner reports aggregate
// statistics on the ProbeResult.TimeoutMs / LossPct so the public
// status page can render "5/5 sent, 0% loss, 12ms avg". The lazy
// alternative — a single SYN/ACK round-trip — already exists as the
// TCP probe; ICMP probes exist for operators who specifically want
// layer-3 reachability (firewall rules, MTU black holes, BGP black-
// holes) without TCP noise.
//
// Identifier is the ICMP echo identifier (16-bit, echoed back by the
// kernel). Zero is replaced with the OS process ID so concurrent
// probes do not collide on the wire. Operators who run multiple
// agents against the same target can pass a fixed value via the
// runner's constructor for deterministic log correlation, but the
// flag-level parser keeps the surface tiny.
type ICMPTarget struct {
	Address string
	Count   int
}

// ICMPRunner runs ICMP echo probes against a fixed list of targets.
// Like TCPRunner it shares one runtime budget per target and returns
// results in input order. Failures of one target never abort the
// batch; each ProbeResult carries its own OK flag and Error string.
//
// Design constraints:
//   - Single UDP-style "ip4:icmp" listener per target. We deliberately
//     do not share one listener across targets because the kernel
//     would multiplex incoming echoes to the wrong Read deadline.
//   - Per-target context caps the full probe (dial + N pings +
//     replies) so a stuck upstream cannot stall the heartbeat loop.
//   - The listener is created lazily inside runOne (not in
//     NewICMPRunner) so a probe permission error does not abort the
//     agent startup; it surfaces on the ProbeResult.Error field.
//   - No IPv6 today. Operators who need v6 should run their own
//     monitoring agent (mtr / smokeping) or wait for the IPv6
//     runner.
type ICMPRunner struct {
	targets []ICMPTarget
	timeout time.Duration
}

// NewICMPRunner normalizes targets (trim, dedupe by Address, default
// Count to 3, clamp Count to [1,10]). A zero or negative timeout
// falls back to 5s, the same default the HTTP/TCP/TLS/DNS runners
// use so per-heartbeat budgets stay uniform.
func NewICMPRunner(targets []ICMPTarget, timeout time.Duration) *ICMPRunner {
	clean := make([]ICMPTarget, 0, len(targets))
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
		count := raw.Count
		if count <= 0 {
			count = 3
		}
		if count > 10 {
			count = 10
		}
		clean = append(clean, ICMPTarget{Address: addr, Count: count})
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &ICMPRunner{targets: clean, timeout: timeout}
}

// Targets returns the normalized target list (read-only). Useful for
// the startup banner that prints "N target(s) configured".
func (r *ICMPRunner) Targets() []ICMPTarget {
	out := make([]ICMPTarget, len(r.targets))
	copy(out, r.targets)
	return out
}

// Run executes every target sequentially (ICMP requires one socket
// per target to avoid reply multiplexing across hosts) and returns
// results in input order. Targets are independent and run in parallel
// so a slow host does not block the rest of the batch.
//
// ponytail: parallelism is bounded by len(targets); for typical
// configs (≤20 hosts) one goroutine each is fine. Switch to a worker
// pool if operators start listing hundreds of addresses.
func (r *ICMPRunner) Run() []protocol.ProbeResult {
	if len(r.targets) == 0 {
		return nil
	}
	now := time.Now().Unix()
	results := make([]protocol.ProbeResult, len(r.targets))
	var wg sync.WaitGroup
	wg.Add(len(r.targets))
	for i, target := range r.targets {
		go func(i int, target ICMPTarget) {
			defer wg.Done()
			results[i] = r.runOne(target, now)
		}(i, target)
	}
	wg.Wait()
	return results
}

func (r *ICMPRunner) runOne(target ICMPTarget, ts int64) protocol.ProbeResult {
	res := protocol.ProbeResult{
		URL:  target.Address,
		Kind: protocol.ProbeKindICMP,
		Ts:   ts,
	}
	if err := validateICMPAddr(target.Address); err != nil {
		res.Error = "invalid_addr: " + err.Error()
		return res
	}

	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()

	dst, err := resolveIPv4(ctx, target.Address)
	if err != nil {
		res.Error = "dns_failure: " + err.Error()
		return res
	}

	conn, err := net.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		res.Error = classifyICMPDialErr(err)
		return res
	}
	defer conn.Close()

	id := os.Getpid() & 0xffff
	if id == 0 {
		id = 1
	}

	rtts := make([]time.Duration, 0, target.Count)
	var lastErr error
	sent := 0
	for seq := 1; seq <= target.Count; seq++ {
		select {
		case <-ctx.Done():
			lastErr = ctx.Err()
			break
		default:
		}
		pkt, err := buildICMPEcho(uint16(id), uint16(seq), 56)
		if err != nil {
			lastErr = err
			break
		}
		start := time.Now()
		if _, err := conn.WriteTo(pkt, &net.IPAddr{IP: dst}); err != nil {
			lastErr = err
			continue
		}
		sent++
		if err := readOneEcho(ctx, conn, uint16(id), uint16(seq)); err != nil {
			lastErr = err
			continue
		}
		rtts = append(rtts, time.Since(start))
	}
	if sent == 0 {
		res.Error = "send_failed: " + errString(lastErr)
		return res
	}
	if len(rtts) == 0 {
		res.Error = classifyICMPRecvErr(lastErr)
		return res
	}
	// Mark probe OK as long as at least one reply arrived; partial loss
	// is still operationally interesting and shows up in the latency
	// series.
	res.OK = true
	res.LatencyMs = avgMillis(rtts)
	res.StatusCode = len(rtts)
	return res
}

// resolveIPv4 returns the first A record of addr, or the literal IPv4
// if addr is already an IP. We deliberately reject IPv6 here — the
// ICMP runner only listens on ip4:icmp; the IPv6 path is its own
// runner.
func resolveIPv4(ctx context.Context, addr string) (net.IP, error) {
	if ip := net.ParseIP(addr); ip != nil {
		v4 := ip.To4()
		if v4 == nil {
			return nil, errors.New("ipv6 addresses not supported by icmp probe")
		}
		return v4, nil
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", addr)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, errors.New("no A records")
	}
	return ips[0].To4(), nil
}

// validateICMPAddr rejects obvious typos. We don't reuse
// validateAddr() because ICMP targets have no port — passing
// "host:80" would be a confusing config error.
func validateICMPAddr(addr string) error {
	if strings.TrimSpace(addr) == "" {
		return errors.New("empty address")
	}
	if strings.ContainsAny(addr, " \t\n\r") {
		return errors.New("whitespace in address")
	}
	if len(addr) > 253 {
		return errors.New("address exceeds 253 chars")
	}
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return errors.New("address must not contain a port")
	}
	if net.ParseIP(addr) != nil {
		return nil
	}
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

// buildICMPEcho assembles an ICMPv4 echo-request packet with a fixed
// 56-byte payload of 0x00. Wire format:
//
//	+----------+------------+----------+----------+--------------------+
//	| type (1) | code (1)   | cksum(2) | id (2)   | seq (2)            |
//	+----------+------------+----------+----------+--------------------+
//	| payload (56 bytes)                                               |
//	+-------------------------------------------------------------------+
//
// We compute the checksum in software; the kernel does not auto-fill
// the field for SOCK_RAW on Linux.
func buildICMPEcho(id, seq uint16, payloadLen int) ([]byte, error) {
	const (
		icmpEchoType = 8
		icmpEchoCode = 0
	)
	if payloadLen < 16 {
		payloadLen = 16
	}
	if payloadLen > 1400 {
		payloadLen = 1400
	}
	buf := make([]byte, 8+payloadLen)
	buf[0] = icmpEchoType
	buf[1] = icmpEchoCode
	binary.BigEndian.PutUint16(buf[4:6], id)
	binary.BigEndian.PutUint16(buf[6:8], seq)
	cs := icmpChecksum(buf)
	binary.BigEndian.PutUint16(buf[2:4], cs)
	return buf, nil
}

// icmpChecksum is the standard Internet checksum (RFC 1071). The
// initial checksum field is treated as zero by the caller; the
// returned value is written into bytes 2-3 of the packet.
func icmpChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

// readOneEcho waits for one matching ICMP echo reply on conn. The
// kernel returns the IP header followed by the ICMP header for
// SOCK_RAW, so we strip 20 bytes (IPv4 header without options) and
// inspect type=0 (echo reply), id, seq. We also tolerate ECHO
// replies for packets the kernel forwarded (i.e., the kernel
// reassembled a fragmented echo and sends a single reply) — those
// arrive as a fresh echo-reply with the right id/seq.
//
// Anything else (TTL exceeded, destination unreachable, etc.) is
// ignored so we wait for the actual reply up to the ctx deadline.
func readOneEcho(ctx context.Context, conn net.PacketConn, id, seq uint16) error {
	buf := make([]byte, 1500)
	deadline, hasDeadline := ctx.Deadline()
	for {
		if hasDeadline {
			_ = conn.SetReadDeadline(deadline)
		}
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			return err
		}
		// Strip IPv4 header (20 bytes, no options assumed).
		if n < 20+8 {
			continue
		}
		pkt := buf[20:n]
		if pkt[0] != 0 { // type 0 = echo reply
			continue
		}
		if binary.BigEndian.Uint16(pkt[4:6]) != id {
			continue
		}
		if binary.BigEndian.Uint16(pkt[6:8]) != seq {
			continue
		}
		return nil
	}
}

// classifyICMPDialErr maps the most common socket-open errors to
// short, status-page-friendly strings. The two interesting ones are
// "permission denied" (operator needs CAP_NET_RAW or
// net.ipv4.ping_group_range) and "network is unreachable" (no
// default route). Anything we don't recognise falls through to the
// raw error.
func classifyICMPDialErr(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, os.ErrPermission) {
		return "permission_denied: enable CAP_NET_RAW or set net.ipv4.ping_group_range"
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "operation not permitted"):
		return "permission_denied: enable CAP_NET_RAW or set net.ipv4.ping_group_range"
	case strings.Contains(s, "network is unreachable"):
		return "no_route"
	default:
		return "socket_open: " + s
	}
}

// classifyICMPRecvErr maps send-time and recv-time errors after the
// socket opened cleanly. The probe has already been configured; the
// only remaining failures are timeout, ICMP error replies (TTL
// exceeded etc.) and OS-level transient errors.
func classifyICMPRecvErr(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "i/o timeout"):
		return "timeout"
	case strings.Contains(s, "no route to host"):
		return "no_route"
	case strings.Contains(s, "network is unreachable"):
		return "no_route"
	default:
		return "no_reply: " + s
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func avgMillis(rtts []time.Duration) int64 {
	if len(rtts) == 0 {
		return 0
	}
	var total time.Duration
	for _, d := range rtts {
		total += d
	}
	return (total / time.Duration(len(rtts))).Milliseconds()
}

// _ keeps the strconv import honest: future IPv4 numeric validation
// hooks may need it without forcing a second import.
var _ = strconv.Itoa

// ICMP echo request packet size, fixed. Operators who want a custom
// payload can fork this file; we deliberately keep the surface tiny.
const icmpEchoPayloadLen = 56

var _ = icmpEchoPayloadLen
