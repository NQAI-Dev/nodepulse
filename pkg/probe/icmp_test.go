package probe

import (
	"strings"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func TestParseICMPTargets(t *testing.T) {
	cases := []struct {
		in      string
		want    []ICMPTarget
		wantErr bool
	}{
		{in: "", want: nil},
		{in: "  ,  ,", want: nil},
		{
			in:   "1.1.1.1",
			want: []ICMPTarget{{Address: "1.1.1.1", Count: 0}},
		},
		{
			in:   "1.1.1.1=5",
			want: []ICMPTarget{{Address: "1.1.1.1", Count: 5}},
		},
		{
			in:   "google.com=4",
			want: []ICMPTarget{{Address: "google.com", Count: 4}},
		},
		{
			in: "a.example,b.example=2,c.example=7",
			want: []ICMPTarget{
				{Address: "a.example", Count: 0},
				{Address: "b.example", Count: 2},
				{Address: "c.example", Count: 7},
			},
		},
		{
			in:   "  1.1.1.1  ",
			want: []ICMPTarget{{Address: "1.1.1.1", Count: 0}},
		},
		{in: "=5", wantErr: true},
		{in: "1.1.1.1=abc", wantErr: true},
		{in: "1.1.1.1=0", wantErr: true},
		{in: "1.1.1.1=-3", wantErr: true},
	}
	for _, c := range cases {
		got, err := ParseICMPTargets(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseICMPTargets(%q): want error, got nil", c.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParseICMPTargets(%q) err=%v", c.in, err)
		}
		if len(got) != len(c.want) {
			t.Fatalf("ParseICMPTargets(%q) len=%d want %d (got=%+v want=%+v)",
				c.in, len(got), len(c.want), got, c.want)
		}
		for i := range got {
			if got[i].Address != c.want[i].Address || got[i].Count != c.want[i].Count {
				t.Errorf("ParseICMPTargets(%q)[%d] = %+v, want %+v", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestNewICMPRunnerDedupDefaultsAndClamp(t *testing.T) {
	r := NewICMPRunner([]ICMPTarget{
		{Address: "  1.1.1.1  "},
		{Address: ""},
		{Address: "1.1.1.1", Count: 7},
		{Address: "8.8.8.8"},
		{Address: "9.9.9.9", Count: 50},
	}, 0)
	got := r.Targets()
	if len(got) != 3 {
		t.Fatalf("targets len = %d, want 3", len(got))
	}
	if got[0].Address != "1.1.1.1" {
		t.Errorf("trim: got %q", got[0].Address)
	}
	if got[0].Count != 7 {
		t.Errorf("dedupe keeps last: Count=%d want 7", got[0].Count)
	}
	if got[2].Count != 10 {
		t.Errorf("clamp to 10: Count=%d", got[2].Count)
	}
}

func TestValidateICMPAddr(t *testing.T) {
	cases := []struct {
		addr    string
		wantErr bool
	}{
		{addr: "1.1.1.1", wantErr: false},
		{addr: "example.com", wantErr: false},
		{addr: "  example.com  ", wantErr: true},
		{addr: "host with space", wantErr: true},
		{addr: "host:80", wantErr: true},
		{addr: strings.Repeat("a", 254), wantErr: true},
		{addr: "example..com", wantErr: true},
	}
	for _, c := range cases {
		err := validateICMPAddr(c.addr)
		if c.wantErr && err == nil {
			t.Errorf("validateICMPAddr(%q): want error, got nil", c.addr)
		}
		if !c.wantErr && err != nil {
			t.Errorf("validateICMPAddr(%q): want nil, got %v", c.addr, err)
		}
	}
}

func TestICMPRunnerInvalidAddr(t *testing.T) {
	r := NewICMPRunner([]ICMPTarget{
		{Address: "host with space"},
		{Address: "host:1234"},
	}, time.Second)
	results := r.Run()
	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2", len(results))
	}
	for i, p := range results {
		if p.OK {
			t.Errorf("results[%d] should fail, got OK", i)
		}
		if !strings.Contains(p.Error, "invalid_addr") {
			t.Errorf("results[%d].Error = %q, want invalid_addr prefix", i, p.Error)
		}
		if p.Kind != protocol.ProbeKindICMP {
			t.Errorf("results[%d].Kind = %q, want icmp", i, p.Kind)
		}
	}
}

func TestICMPEchoPacketIsWellFormed(t *testing.T) {
	pkt, err := buildICMPEcho(0x1234, 0x0007, 56)
	if err != nil {
		t.Fatalf("buildICMPEcho: %v", err)
	}
	if len(pkt) != 64 {
		t.Fatalf("len = %d, want 64", len(pkt))
	}
	if pkt[0] != 8 {
		t.Errorf("type = %d, want 8 (echo request)", pkt[0])
	}
	if pkt[1] != 0 {
		t.Errorf("code = %d, want 0", pkt[1])
	}
	id := uint16(pkt[4])<<8 | uint16(pkt[5])
	if id != 0x1234 {
		t.Errorf("id = %#x, want 0x1234", id)
	}
	seq := uint16(pkt[6])<<8 | uint16(pkt[7])
	if seq != 0x0007 {
		t.Errorf("seq = %#x, want 0x0007", seq)
	}
	// Checksum must equal zero when re-summed (the field is set, then
	// re-summing the whole packet with the field set yields zero).
	if got := icmpChecksum(pkt); got != 0 {
		t.Errorf("re-checksum = %#x, want 0 (packet corrupt)", got)
	}
}

func TestClassifyICMPErrors(t *testing.T) {
	if got := classifyICMPRecvErr(nil); got != "" {
		t.Errorf("nil err: got %q, want empty", got)
	}
	if got := classifyICMPDialErr(nil); got != "" {
		t.Errorf("nil err: got %q, want empty", got)
	}
	// "operation not permitted" maps to permission_denied with a hint.
	got := classifyICMPDialErr(errString_{"operation not permitted"})
	if !strings.Contains(got, "permission_denied") {
		t.Errorf("want permission_denied prefix, got %q", got)
	}
	// Context deadline is the canonical recv-time error.
	got = classifyICMPRecvErr(timeoutErr)
	if got != "timeout" {
		t.Errorf("deadline: got %q, want timeout", got)
	}
}

// errString_ is a tiny adapter so we don't need to import a private
// sentinel type into the test file.
type errString_ struct{ s string }

func (e errString_) Error() string { return e.s }

var timeoutErr = timeoutErr_{}

type timeoutErr_ struct{}

func (timeoutErr_) Error() string  { return "i/o timeout" }
func (timeoutErr_) Timeout() bool { return true }

func TestExtractICMPEchoReply(t *testing.T) {
	// 1. Raw ICMP packet without IP header (typical for Linux ip4:icmp ListenPacket)
	// Echo reply: type=0, code=0, cksum, id=0x1234, seq=0x0001
	rawICMP := []byte{0x00, 0x00, 0x00, 0x00, 0x12, 0x34, 0x00, 0x01, 0xAA, 0xBB}
	pkt := extractICMPPayload(rawICMP)
	if len(pkt) != len(rawICMP) || pkt[0] != 0 || pkt[4] != 0x12 || pkt[5] != 0x34 {
		t.Fatalf("extractICMPPayload(rawICMP) failed: %v", pkt)
	}

	// 2. Full IPv4 frame with 20-byte IP header prepended (IPv4 proto=1 ICMP)
	ipHeader := []byte{
		0x45, 0x00, 0x00, 0x20, // ver=4, ihl=5 (20 bytes)
		0x00, 0x00, 0x00, 0x00,
		0x40, 0x01, 0x00, 0x00, // ttl=64, proto=1 (ICMP)
		127, 0, 0, 1,
		127, 0, 0, 1,
	}
	framed := append(ipHeader, rawICMP...)
	pktFramed := extractICMPPayload(framed)
	if len(pktFramed) != len(rawICMP) || pktFramed[0] != 0 || pktFramed[4] != 0x12 || pktFramed[5] != 0x34 {
		t.Fatalf("extractICMPPayload(framed) failed: %v", pktFramed)
	}
}

