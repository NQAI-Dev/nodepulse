package probe

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func TestNewTCPRunnerDedupesAndTrims(t *testing.T) {
	r := NewTCPRunner([]TCPTarget{
		{Address: "  localhost:80 ", ExpectBanner: "x"},
		{Address: ""},
		{Address: "localhost:80"},
		{Address: "localhost:81"},
	}, 0)
	if got, want := len(r.Targets()), 2; got != want {
		t.Fatalf("targets len = %d, want %d", got, want)
	}
	if r.Targets()[0].Address != "localhost:80" {
		t.Errorf("targets[0].Address = %q, want trimmed", r.Targets()[0].Address)
	}
}

func TestNewTCPRunnerPrefersBannerOnDedup(t *testing.T) {
	r := NewTCPRunner([]TCPTarget{
		{Address: "db:5432"},
		{Address: "db:5432", ExpectBanner: "postgres"},
	}, time.Second)
	got := r.Targets()
	if len(got) != 1 {
		t.Fatalf("len(targets) = %d, want 1", len(got))
	}
	if got[0].ExpectBanner != "postgres" {
		t.Errorf("ExpectBanner = %q, want %q (second entry should win)",
			got[0].ExpectBanner, "postgres")
	}
}

func TestTCPRunReportsOpenPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go acceptLoop(ln)

	r := NewTCPRunner([]TCPTarget{{Address: ln.Addr().String()}}, 2*time.Second)
	got := r.Run()
	if len(got) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(got))
	}
	res := got[0]
	if res.Kind != protocol.ProbeKindTCP {
		t.Errorf("Kind = %q, want %q", res.Kind, protocol.ProbeKindTCP)
	}
	if !res.OK {
		t.Errorf("OK = false, want true (err=%q)", res.Error)
	}
	if res.StatusCode != 0 {
		t.Errorf("StatusCode = %d, want 0 for TCP probe", res.StatusCode)
	}
	if res.LatencyMs < 0 {
		t.Errorf("LatencyMs = %d, want >=0", res.LatencyMs)
	}
}

func TestTCPRunReportsRefused(t *testing.T) {
	// Bind a port and immediately close it so we get a deterministic
	// "connection refused" target.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	r := NewTCPRunner([]TCPTarget{{Address: addr}}, 1*time.Second)
	res := r.Run()[0]
	if res.OK {
		t.Errorf("OK = true, want false for refused")
	}
	if !strings.Contains(res.Error, "refused") {
		t.Errorf("Error = %q, want contains 'refused'", res.Error)
	}
}

func TestTCPRunReportsTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	// Accept but never speak.
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				time.Sleep(2 * time.Second)
			}(c)
		}
	}()

	r := NewTCPRunner([]TCPTarget{
		{Address: ln.Addr().String(), ExpectBanner: "hello"},
	}, 200*time.Millisecond)
	res := r.Run()[0]
	if res.OK {
		t.Errorf("OK = true, want false on banner timeout")
	}
	if !strings.Contains(res.Error, "timeout") && !strings.Contains(res.Error, "deadline") {
		t.Errorf("Error = %q, want timeout-ish message", res.Error)
	}
}

func TestTCPRunBannerMatch(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("postgres 15.4 ready\r\n"))
			c.Close()
		}
	}()

	r := NewTCPRunner([]TCPTarget{
		{Address: ln.Addr().String(), ExpectBanner: "postgres"},
	}, 1*time.Second)
	res := r.Run()[0]
	if !res.OK {
		t.Errorf("OK = false, want true (err=%q)", res.Error)
	}
}

func TestTCPRunBannerMismatch(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("redis 7.2\r\n"))
			c.Close()
		}
	}()

	r := NewTCPRunner([]TCPTarget{
		{Address: ln.Addr().String(), ExpectBanner: "postgres"},
	}, 1*time.Second)
	res := r.Run()[0]
	if res.OK {
		t.Errorf("OK = true, want false on banner mismatch")
	}
	if !strings.Contains(res.Error, "banner_mismatch") {
		t.Errorf("Error = %q, want banner_mismatch prefix", res.Error)
	}
}

func TestTCPRunRejectsInvalidAddress(t *testing.T) {
	r := NewTCPRunner([]TCPTarget{
		{Address: "127.0.0.1:99999"},
		{Address: "127.0.0.1:abc"},
		{Address: ":80"},
		{Address: "localhost"},
	}, time.Second)
	got := r.Run()
	if len(got) != 4 {
		t.Fatalf("len(results) = %d, want 4", len(got))
	}
	for i, res := range got {
		if res.OK {
			t.Errorf("results[%d].OK = true, want false", i)
		}
		if !strings.HasPrefix(res.Error, "invalid_addr:") {
			t.Errorf("results[%d].Error = %q, want invalid_addr prefix", i, res.Error)
		}
	}
}

func TestTCPRunEmptyTargetsReturnsNil(t *testing.T) {
	r := NewTCPRunner(nil, time.Second)
	if got := r.Run(); got != nil {
		t.Errorf("Run() = %v, want nil", got)
	}
}

func TestTCPRunOrderStable(t *testing.T) {
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln1.Close()
	go acceptLoop(ln1)
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln2.Close()
	go acceptLoop(ln2)

	r := NewTCPRunner([]TCPTarget{
		{Address: ln2.Addr().String()}, // reverse input order on purpose
		{Address: ln1.Addr().String()},
	}, 2*time.Second)
	got := r.Run()
	if got[0].URL != ln2.Addr().String() {
		t.Errorf("results[0].URL = %q, want %q (order must match input)",
			got[0].URL, ln2.Addr().String())
	}
	if got[1].URL != ln1.Addr().String() {
		t.Errorf("results[1].URL = %q, want %q", got[1].URL, ln1.Addr().String())
	}
}

// acceptLoop is a tiny test helper: accept + close forever, so connect
// probes see an open port without us caring about the payload.
func acceptLoop(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		c.Close()
	}
}
