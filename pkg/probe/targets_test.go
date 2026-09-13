package probe

import (
	"reflect"
	"testing"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func TestParseTCPTargetsEmpty(t *testing.T) {
	got, err := ParseTCPTargets("")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got != nil {
		t.Errorf("got = %v, want nil", got)
	}
	got, err = ParseTCPTargets("   ,  , ")
	if err != nil {
		t.Fatalf("err = %v, want nil for blanks-only", err)
	}
	if got != nil {
		t.Errorf("got = %v, want nil for blanks-only", got)
	}
}

func TestParseTCPTargetsHostPort(t *testing.T) {
	got, err := ParseTCPTargets("db.local:5432, redis:6379")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	want := []TCPTarget{
		{Address: "db.local:5432"},
		{Address: "redis:6379"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got = %+v, want %+v", got, want)
	}
}

func TestParseTCPTargetsBannerSpec(t *testing.T) {
	got, err := ParseTCPTargets("db:5432=postgres, cache:6379=redis")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	want := []TCPTarget{
		{Address: "db:5432", ExpectBanner: "postgres"},
		{Address: "cache:6379", ExpectBanner: "redis"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got = %+v, want %+v", got, want)
	}
}

func TestParseTCPTargetsRejectsEmptyHost(t *testing.T) {
	if _, err := ParseTCPTargets("=banner"); err == nil {
		t.Errorf("expected error for empty host:port, got nil")
	}
}

func TestParseTCPTargetsDedupAndBannerMerge(t *testing.T) {
	// NewTCPRunner does the dedup + banner-merge; this test pins the
	// external behaviour that the agent relies on.
	r := NewTCPRunner(mustParse(t, "db:5432, db:5432=postgres"), 0)
	got := r.Targets()
	if len(got) != 1 {
		t.Fatalf("len(targets) = %d, want 1", len(got))
	}
	if got[0].ExpectBanner != "postgres" {
		t.Errorf("ExpectBanner = %q, want %q", got[0].ExpectBanner, "postgres")
	}
}

func TestMergeProbeResultsBothEmpty(t *testing.T) {
	if got := MergeProbeResults(nil, nil); got != nil {
		t.Errorf("got = %v, want nil", got)
	}
}

func TestMergeProbeResultsHTTPOnly(t *testing.T) {
	in := []protocol.ProbeResult{{URL: "http://x", Kind: protocol.ProbeKindHTTP, OK: true}}
	got := MergeProbeResults(in, nil)
	if !reflect.DeepEqual(got, in) {
		t.Errorf("got = %+v, want %+v", got, in)
	}
}

func TestMergeProbeResultsTCPOnly(t *testing.T) {
	in := []protocol.ProbeResult{{URL: "db:5432", Kind: protocol.ProbeKindTCP, OK: true}}
	got := MergeProbeResults(nil, in)
	if !reflect.DeepEqual(got, in) {
		t.Errorf("got = %+v, want %+v", got, in)
	}
}

func TestMergeProbeResultsOrder(t *testing.T) {
	httpR := []protocol.ProbeResult{
		{URL: "http://a", Kind: protocol.ProbeKindHTTP, OK: true},
		{URL: "http://b", Kind: protocol.ProbeKindHTTP, OK: true},
	}
	tcpR := []protocol.ProbeResult{
		{URL: "db:5432", Kind: protocol.ProbeKindTCP, OK: true},
	}
	got := MergeProbeResults(httpR, tcpR)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].Kind != protocol.ProbeKindHTTP || got[2].Kind != protocol.ProbeKindTCP {
		t.Errorf("merge lost order: %+v", got)
	}
}

func mustParse(t *testing.T, raw string) []TCPTarget {
	t.Helper()
	got, err := ParseTCPTargets(raw)
	if err != nil {
		t.Fatalf("ParseTCPTargets(%q): %v", raw, err)
	}
	return got
}
