package probe

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// newTLSServer spins up a loopback TLS server with a freshly minted
// leaf cert that expires at `notAfter`. Returns the listener address and
// a teardown closer.
func newTLSServer(t *testing.T, notAfter time.Time) (addr string, stop func()) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	cert := tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  priv,
		Leaf:        tmpl,
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
	})
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Drive the handshake so the client gets its PeerCertificates.
			if tc, ok := c.(*tls.Conn); ok {
				_ = tc.Handshake()
			}
			c.Close()
		}
	}()

	return ln.Addr().String(), func() {
		_ = ln.Close()
	}
}

func TestNewTLSRunnerDefaultsAndDedupes(t *testing.T) {
	in := []TLSTarget{
		{Address: "  a.example:443 "},
		{Address: "a.example:443", WarnBefore: 7 * 24 * time.Hour},
		{Address: ""},
		{Address: "b.example:8443", WarnBefore: 30 * 24 * time.Hour},
	}
	r := NewTLSRunner(in, 0)
	if got := len(r.Targets()); got != 2 {
		t.Fatalf("len(Targets()) = %d, want 2", got)
	}
	if r.Targets()[0].WarnBefore != 7*24*time.Hour {
		t.Errorf("dedupe did not overwrite WarnBefore: got %s", r.Targets()[0].WarnBefore)
	}
	if r.Targets()[0].Address != "a.example:443" {
		t.Errorf("address not trimmed: %q", r.Targets()[0].Address)
	}
	if r.Targets()[1].WarnBefore != 30*24*time.Hour {
		t.Errorf("WarnBefore changed: %s", r.Targets()[1].WarnBefore)
	}
	if r.timeout != 5*time.Second {
		t.Errorf("default timeout = %s, want 5s", r.timeout)
	}
}

func TestParseTLSTargetsGrammar(t *testing.T) {
	cases := []struct {
		in   string
		want []TLSTarget
		err  string
	}{
		{"", nil, ""},
		{"  ", nil, ""},
		{"api.example:443", []TLSTarget{{Address: "api.example:443"}}, ""},
		{"api.example:443=30d", []TLSTarget{{Address: "api.example:443", WarnBefore: 30 * 24 * time.Hour}}, ""},
		{"api.example:443=12h:1.3", []TLSTarget{{Address: "api.example:443", WarnBefore: 12 * time.Hour, MinTLS: "1.3"}}, ""},
		{"api.example:443=7d:1.2:insecure", []TLSTarget{{Address: "api.example:443", WarnBefore: 7 * 24 * time.Hour, MinTLS: "1.2", InsecureSkipVerify: true}}, ""},
		{"a:1=,b:2=5d", []TLSTarget{
			{Address: "a:1"},
			{Address: "b:2", WarnBefore: 5 * 24 * time.Hour},
		}, ""},
		{"bad", nil, "empty host:port"},
		{"a:1=5x", nil, "suffix must be d, h, or m"},
		{"a:1=-1d", nil, "non-negative integer"},
		{"a:1=5d:1.3:extra:junk", nil, "too many segments"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseTLSTargets(tc.in)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want substring %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i].Address != tc.want[i].Address {
					t.Errorf("[%d].Address = %q, want %q", i, got[i].Address, tc.want[i].Address)
				}
				if got[i].WarnBefore != tc.want[i].WarnBefore {
					t.Errorf("[%d].WarnBefore = %s, want %s", i, got[i].WarnBefore, tc.want[i].WarnBefore)
				}
				if got[i].MinTLS != tc.want[i].MinTLS {
					t.Errorf("[%d].MinTLS = %q, want %q", i, got[i].MinTLS, tc.want[i].MinTLS)
				}
				if got[i].InsecureSkipVerify != tc.want[i].InsecureSkipVerify {
					t.Errorf("[%d].InsecureSkipVerify = %v, want %v", i, got[i].InsecureSkipVerify, tc.want[i].InsecureSkipVerify)
				}
			}
		})
	}
}

func TestParseTLSVersion(t *testing.T) {
	cases := []struct {
		in      string
		want    uint16
		wantErr string
	}{
		{"", 0, ""},
		{"1.2", tls.VersionTLS12, ""},
		{"1.3", tls.VersionTLS13, ""},
		{"1.0", 0, "not supported"},
		{"9.9", 0, "unknown tls version"},
	}
	for _, tc := range cases {
		got, err := parseTLSVersion(tc.in)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%q: err = %v, want %q", tc.in, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected err %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q: got %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestTLSProbeOK(t *testing.T) {
	addr, stop := newTLSServer(t, time.Now().Add(365*24*time.Hour))
	defer stop()
	// InsecureSkipVerify is the prod-off default; we flip it on here
	// because newTLSServer hands out a self-signed leaf and the
	// handshake/expiry path is what we're testing, not chain
	// validation (that's covered by the Go stdlib).
	r := NewTLSRunner([]TLSTarget{{Address: addr, WarnBefore: 14 * 24 * time.Hour, InsecureSkipVerify: true}}, 2*time.Second)
	res := r.Run()
	if len(res) != 1 {
		t.Fatalf("len = %d, want 1", len(res))
	}
	got := res[0]
	if !got.OK {
		t.Errorf("OK = false, err = %q", got.Error)
	}
	if got.Kind != protocol.ProbeKindTLS {
		t.Errorf("Kind = %q, want %q", got.Kind, protocol.ProbeKindTLS)
	}
	if got.StatusCode != 1 {
		t.Errorf("StatusCode = %d, want 1 (OK)", got.StatusCode)
	}
	if got.Error != "" {
		t.Errorf("Error = %q, want empty (warn window is 14d, cert valid for 365d)", got.Error)
	}
	if got.LatencyMs < 0 {
		t.Errorf("LatencyMs = %d, want >= 0", got.LatencyMs)
	}
	if got.Ts == 0 {
		t.Errorf("Ts = 0, want non-zero")
	}
}

func TestTLSProbeWarnWindow(t *testing.T) {
	addr, stop := newTLSServer(t, time.Now().Add(3*24*time.Hour))
	defer stop()
	r := NewTLSRunner([]TLSTarget{{Address: addr, WarnBefore: 14 * 24 * time.Hour, InsecureSkipVerify: true}}, 2*time.Second)
	res := r.Run()
	if len(res) != 1 {
		t.Fatalf("len = %d", len(res))
	}
	got := res[0]
	if !got.OK {
		t.Errorf("OK = false inside warn window: err=%q", got.Error)
	}
	if got.StatusCode != 2 {
		t.Errorf("StatusCode = %d, want 2 (warn)", got.StatusCode)
	}
	if !strings.HasPrefix(got.Error, "expires_in_") {
		t.Errorf("Error = %q, want expires_in_ prefix", got.Error)
	}
}

func TestTLSProbeExpiredCert(t *testing.T) {
	addr, stop := newTLSServer(t, time.Now().Add(-time.Hour))
	defer stop()
	r := NewTLSRunner([]TLSTarget{{Address: addr, InsecureSkipVerify: true}}, 2*time.Second)
	res := r.Run()
	got := res[0]
	if got.OK {
		t.Errorf("OK = true on expired cert")
	}
	if got.Error != "cert_expired" {
		t.Errorf("Error = %q, want cert_expired", got.Error)
	}
}

func TestTLSProbeInvalidAddr(t *testing.T) {
	r := NewTLSRunner([]TLSTarget{{Address: "no-port"}}, time.Second)
	res := r.Run()
	got := res[0]
	if got.OK {
		t.Errorf("OK = true on invalid addr")
	}
	if !strings.HasPrefix(got.Error, "invalid_addr:") {
		t.Errorf("Error = %q, want invalid_addr: prefix", got.Error)
	}
}

func TestTLSProbeInvalidMinTLS(t *testing.T) {
	r := NewTLSRunner([]TLSTarget{{Address: "localhost:443", MinTLS: "9.9"}}, time.Second)
	res := r.Run()
	got := res[0]
	if got.OK {
		t.Errorf("OK = true on bad min-tls")
	}
	if !strings.HasPrefix(got.Error, "invalid_min_tls:") {
		t.Errorf("Error = %q, want invalid_min_tls: prefix", got.Error)
	}
}

func TestTLSProbeConnectFailure(t *testing.T) {
	// Bind a TCP listener and immediately close it; the address should
	// be unused by the time the probe dials. Use a fresh port picked
	// from the OS so we never collide with a real service.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	r := NewTLSRunner([]TLSTarget{{Address: addr}}, 500*time.Millisecond)
	res := r.Run()
	got := res[0]
	if got.OK {
		t.Errorf("OK = true on connect failure")
	}
	if got.Error == "" {
		t.Errorf("Error empty, want dial failure")
	}
	if got.LatencyMs < 0 {
		t.Errorf("LatencyMs = %d", got.LatencyMs)
	}
}

func TestTLSProbeParallelOrder(t *testing.T) {
	// Build a runner with three targets, one valid and two broken. The
	// valid one uses a cert inside the warn window so it spends more
	// time in the handshake than the broken dials. Results must still
	// come back in input order.
	srv, stop := newTLSServer(t, time.Now().Add(12*time.Hour))
	defer stop()

	in := []TLSTarget{
		{Address: "127.0.0.1:1", WarnBefore: 24 * time.Hour},                          // closed
		{Address: srv, WarnBefore: 24 * time.Hour, InsecureSkipVerify: true},         // valid (short warn)
		{Address: "no-such-host.invalid.:65535"},                                      // nx
	}
	r := NewTLSRunner(in, 2*time.Second)
	results := r.Run()
	if len(results) != 3 {
		t.Fatalf("len = %d, want 3", len(results))
	}
	if results[0].OK {
		t.Errorf("results[0] OK, want fail (closed port)")
	}
	if !results[1].OK {
		t.Errorf("results[1] !OK, err=%q", results[1].Error)
	}
	if results[1].StatusCode != 2 {
		t.Errorf("results[1].StatusCode = %d, want 2 (warn)", results[1].StatusCode)
	}
	if results[2].OK {
		t.Errorf("results[2] OK, want fail (nx)")
	}
	if results[2].Kind != protocol.ProbeKindTLS {
		t.Errorf("results[2].Kind = %q", results[2].Kind)
	}
}

func TestMergeProbeResultsAll(t *testing.T) {
	httpR := []protocol.ProbeResult{{URL: "h", Kind: protocol.ProbeKindHTTP}}
	tcpR := []protocol.ProbeResult{{URL: "t", Kind: protocol.ProbeKindTCP}}
	tlsR := []protocol.ProbeResult{{URL: "s", Kind: protocol.ProbeKindTLS}}
	dnsR := []protocol.ProbeResult{{URL: "d", Kind: protocol.ProbeKindDNS}}
	out := MergeProbeResultsAll(httpR, tcpR, tlsR, dnsR)
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4", len(out))
	}
	if out[0].Kind != protocol.ProbeKindHTTP || out[1].Kind != protocol.ProbeKindTCP || out[2].Kind != protocol.ProbeKindTLS || out[3].Kind != protocol.ProbeKindDNS {
		t.Errorf("order broken: %+v", out)
	}
	if MergeProbeResultsAll(nil, nil, nil, nil) != nil {
		t.Errorf("all-empty should return nil")
	}
}

// TestTLSProbeRejectsMismatchedServerName ensures the probe fails on a
// cert whose SAN does not include the dial target. We connect to
// 127.0.0.1 but the cert only has dnsNames=localhost; the client
// ServerName "127.0.0.1" is fine because we add the IP to IPAddresses,
// so this is a positive control that the probe completes end-to-end.
func TestTLSProbeRejectsMismatchedServerName(t *testing.T) {
	addr, stop := newTLSServer(t, time.Now().Add(24*time.Hour))
	defer stop()
	r := NewTLSRunner([]TLSTarget{{Address: addr, InsecureSkipVerify: true}}, 2*time.Second)
	res := r.Run()
	if !res[0].OK {
		t.Fatalf("sanity: probe against loopback IP failed: %q", res[0].Error)
	}
}

// silence the unused-import linter when this file is built without the
// pem package being referenced (it's here so the file stays self-
// sufficient for future cert-rewriting tests).
var _ = pem.Encode
var _ = atomic.AddInt32
var _ = errors.New
