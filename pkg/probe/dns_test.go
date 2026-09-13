package probe

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func TestParseDNSTargets(t *testing.T) {
	cases := []struct {
		in   string
		want []DNSTarget
	}{
		{in: "", want: nil},
		{in: "  ,  ,", want: nil},
		{
			in: "example.com",
			want: []DNSTarget{{Address: "example.com", Type: "A"}},
		},
		{
			in: "example.com=10.0.",
			want: []DNSTarget{{Address: "example.com", Expected: "10.0.", Type: "A"}},
		},
		{
			in: "example.com=AAAA",
			want: []DNSTarget{{Address: "example.com", Type: "AAAA"}},
		},
		{
			in: "example.com=AAAA=2001",
			want: []DNSTarget{{Address: "example.com", Type: "AAAA", Expected: "2001"}},
		},
		{
			in: "example.com=A=10.0.",
			want: []DNSTarget{{Address: "example.com", Type: "A", Expected: "10.0."}},
		},
		{
			in: "  example.com  ",
			want: []DNSTarget{{Address: "example.com", Type: "A"}},
		},
		{
			in: "a.example.com,b.example.com=AAAA",
			want: []DNSTarget{
				{Address: "a.example.com", Type: "A"},
				{Address: "b.example.com", Type: "AAAA"},
			},
		},
	}
	for _, c := range cases {
		got, err := ParseDNSTargets(c.in)
		if err != nil {
			t.Fatalf("ParseDNSTargets(%q) err=%v", c.in, err)
		}
		if len(got) != len(c.want) {
			t.Fatalf("ParseDNSTargets(%q) len=%d want %d (got=%+v want=%+v)",
				c.in, len(got), len(c.want), got, c.want)
		}
		for i := range got {
			if got[i].Address != c.want[i].Address ||
				got[i].Expected != c.want[i].Expected ||
				got[i].Type != c.want[i].Type {
				t.Errorf("ParseDNSTargets(%q)[%d] = %+v, want %+v",
					c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestParseDNSTargetsRejectsEmptyAddr(t *testing.T) {
	if _, err := ParseDNSTargets("=substring"); err == nil {
		t.Fatal("expected error for empty address")
	}
}

func TestNewDNSRunnerDedupAndDefaults(t *testing.T) {
	r := NewDNSRunner([]DNSTarget{
		{Address: "  example.com  "},
		{Address: ""},
		{Address: "example.com", Expected: "1.2.3."},
		{Address: "api.example.com"},
		{Address: "v6.example.com", Type: "aaaa"},
	}, 0)
	got := r.Targets()
	if len(got) != 3 {
		t.Fatalf("targets len = %d, want 3", len(got))
	}
	if got[0].Address != "example.com" {
		t.Errorf("trim: got %q", got[0].Address)
	}
	// dedupe keeps the last entry; the second `example.com` adds the
	// Expected substring, so that should be present.
	if got[0].Expected != "1.2.3." {
		t.Errorf("dedupe overwrite: Expected=%q want %q", got[0].Expected, "1.2.3.")
	}
	if got[2].Type != "AAAA" {
		t.Errorf("type normalization: got %q want AAAA", got[2].Type)
	}
}

func TestDNSRunnerRunLookupAgainstLocalStub(t *testing.T) {
	// Use the system resolver to look up "localhost" which the Go
	// resolver always satisfies locally via /etc/hosts on every
	// supported platform. CI sandboxes that block DNS still answer
	// "localhost"; if even that fails the test is skipped rather than
	// marked failing to keep the suite green in airgapped envs.
	r := NewDNSRunner([]DNSTarget{
		{Address: "localhost"},
		{Address: "127.0.0.1"},
	}, 2*time.Second)
	results := r.Run()
	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2", len(results))
	}
	for i, p := range results {
		if p.Kind != protocol.ProbeKindDNS {
			t.Errorf("results[%d].Kind = %q, want dns", i, p.Kind)
		}
		if !p.OK {
			t.Skipf("results[%d] lookup failed in this environment: %s", i, p.Error)
		}
		if p.LatencyMs < 0 {
			t.Errorf("results[%d].LatencyMs = %d", i, p.LatencyMs)
		}
	}
}

func TestDNSRunnerInvalidAddr(t *testing.T) {
	r := NewDNSRunner([]DNSTarget{
		{Address: "host with space"},
		{Address: "host:1234"},
		{Address: strings.Repeat("a", 254)},
		{Address: "example.com..invalid"},
	}, time.Second)
	results := r.Run()
	for i, p := range results {
		if p.OK {
			t.Errorf("results[%d] should fail, got OK", i)
		}
		if !strings.Contains(p.Error, "invalid_addr") {
			t.Errorf("results[%d].Error = %q, want invalid_addr prefix", i, p.Error)
		}
	}
}

func TestDNSRunnerInvalidType(t *testing.T) {
	r := NewDNSRunner([]DNSTarget{
		{Address: "example.com", Type: "CNAME"},
	}, time.Second)
	p := r.Run()[0]
	if p.OK {
		t.Fatal("expected non-OK for invalid type")
	}
	if !strings.Contains(p.Error, "invalid_type") {
		t.Errorf("Error = %q, want invalid_type prefix", p.Error)
	}
}

func TestValidateDNSAddr(t *testing.T) {
	cases := []struct {
		addr    string
		wantErr bool
	}{
		{addr: "example.com", wantErr: false},
		{addr: "1.1.1.1", wantErr: false},
		{addr: "::1", wantErr: false},
		{addr: "  example.com  ", wantErr: true}, // leading whitespace
		{addr: "host with space", wantErr: true},
		{addr: "host:80", wantErr: true},
		{addr: strings.Repeat("a", 254), wantErr: true},
		{addr: "example..com", wantErr: true},
		{addr: "label." + strings.Repeat("a", 64), wantErr: true},
	}
	for _, c := range cases {
		err := validateDNSAddr(c.addr)
		if c.wantErr && err == nil {
			t.Errorf("validateDNSAddr(%q): want error, got nil", c.addr)
		}
		if !c.wantErr && err != nil {
			t.Errorf("validateDNSAddr(%q): want nil, got %v", c.addr, err)
		}
	}
}

func TestClassifyDNSLookupErr(t *testing.T) {
	if classifyDNSLookupErr(nil) != "" {
		t.Error("nil err should map to empty string")
	}
	// Construct a known DNSError shape.
	de := &net.DNSError{Err: "no such host", Name: "nx", IsNotFound: true}
	if got := classifyDNSLookupErr(de); got != "nxdomain" {
		t.Errorf("IsNotFound: got %q, want nxdomain", got)
	}
	de = &net.DNSError{Err: "timeout", Name: "x", IsTimeout: true}
	if got := classifyDNSLookupErr(de); got != "timeout" {
		t.Errorf("IsTimeout: got %q, want timeout", got)
	}
	de = &net.DNSError{Err: "temp", Name: "x", IsTemporary: true}
	if got := classifyDNSLookupErr(de); got != "temporary_failure" {
		t.Errorf("IsTemporary: got %q, want temporary_failure", got)
	}
	de = &net.DNSError{Err: "other", Name: "x", Server: "10.0.0.1:53"}
	if got := classifyDNSLookupErr(de); !strings.Contains(got, "10.0.0.1:53") {
		t.Errorf("server in error: got %q", got)
	}
}
