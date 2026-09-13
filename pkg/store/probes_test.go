package store

import (
	"os"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func newProbeTestStore(t *testing.T) *PersistentStore {
	t.Helper()
	tmp, err := os.CreateTemp("", "nodepulse-probes-*.db")
	if err != nil {
		t.Fatalf("tempfile: %v", err)
	}
	tmp.Close()
	store, err := NewPersistentStore(tmp.Name(), "", 0)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}
	t.Cleanup(func() {
		os.Remove(tmp.Name())
	})
	return store
}

func TestRecordAndSummarizeProbes(t *testing.T) {
	store := newProbeTestStore(t)
	now := time.Now().Unix()

	results := []protocol.ProbeResult{
		{URL: "https://a.example/health", StatusCode: 200, LatencyMs: 80, OK: true, Ts: now - 100},
		{URL: "https://a.example/health", StatusCode: 200, LatencyMs: 120, OK: true, Ts: now - 80},
		{URL: "https://a.example/health", StatusCode: 503, LatencyMs: 200, OK: false, Error: "Service Unavailable", Ts: now - 60},
		{URL: "https://b.example/health", StatusCode: 200, LatencyMs: 50, OK: true, Ts: now - 50},
		{URL: "https://b.example/health", StatusCode: 0, LatencyMs: 5000, OK: false, Error: "context deadline exceeded", Ts: now - 20},
	}
	if err := store.RecordProbeResults("node-1", results); err != nil {
		t.Fatalf("RecordProbeResults: %v", err)
	}

	summaries, err := store.ProbeSummaries(3600)
	if err != nil {
		t.Fatalf("ProbeSummaries: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("len(summaries) = %d, want 2 (one per URL)", len(summaries))
	}

	byURL := map[string]ProbeSummary{}
	for _, s := range summaries {
		byURL[s.URL] = s
	}

	a := byURL["https://a.example/health"]
	if a.Total != 3 || a.OK != 2 || a.Failed != 1 {
		t.Errorf("a total=%d ok=%d failed=%d, want 3/2/1", a.Total, a.OK, a.Failed)
	}
	if a.LastStatus != 503 || a.LastOK {
		t.Errorf("a last_status=%d last_ok=%v, want 503/false", a.LastStatus, a.LastOK)
	}
	if a.UptimePct < 66.6 || a.UptimePct > 66.8 {
		t.Errorf("a uptime = %f, want ~66.66", a.UptimePct)
	}
	if a.LatencyP50 != 120 || a.LatencyP95 != 200 {
		t.Errorf("a latency p50=%d p95=%d, want 120/200", a.LatencyP50, a.LatencyP95)
	}

	b := byURL["https://b.example/health"]
	if b.Total != 2 || b.OK != 1 || b.Failed != 1 {
		t.Errorf("b total=%d ok=%d failed=%d, want 2/1/1", b.Total, b.OK, b.Failed)
	}
	if b.LastStatus != 0 || b.LastOK {
		t.Errorf("b last_status=%d last_ok=%v, want 0/false (timeout)", b.LastStatus, b.LastOK)
	}
}

func TestRecordProbeResultsEmptyNoop(t *testing.T) {
	store := newProbeTestStore(t)
	if err := store.RecordProbeResults("node-1", nil); err != nil {
		t.Errorf("RecordProbeResults(nil) = %v, want nil", err)
	}
	if err := store.RecordProbeResults("node-1", []protocol.ProbeResult{}); err != nil {
		t.Errorf("RecordProbeResults([]) = %v, want nil", err)
	}
}

func TestPurgeProbeResultsOlderThan(t *testing.T) {
	store := newProbeTestStore(t)
	now := time.Now().Unix()

	results := []protocol.ProbeResult{
		{URL: "https://x.example", StatusCode: 200, OK: true, Ts: now - 7200}, // 2h old
		{URL: "https://x.example", StatusCode: 200, OK: true, Ts: now - 60},    // fresh
	}
	if err := store.RecordProbeResults("node-1", results); err != nil {
		t.Fatalf("RecordProbeResults: %v", err)
	}
	deleted, err := store.PurgeProbeResultsOlderThan(time.Unix(now-3600, 0))
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	summaries, _ := store.ProbeSummaries(86400)
	if len(summaries) != 1 {
		t.Fatalf("after purge len(summaries) = %d, want 1", len(summaries))
	}
	if summaries[0].Total != 1 {
		t.Errorf("after purge total = %d, want 1", summaries[0].Total)
	}
}

func TestProbeSummariesWindowExcludesOldSamples(t *testing.T) {
	store := newProbeTestStore(t)
	now := time.Now().Unix()
	results := []protocol.ProbeResult{
		// Outside the 1h window.
		{URL: "https://w.example", StatusCode: 500, OK: false, Ts: now - 7200},
		// Inside.
		{URL: "https://w.example", StatusCode: 200, OK: true, Ts: now - 30},
	}
	if err := store.RecordProbeResults("node-1", results); err != nil {
		t.Fatalf("RecordProbeResults: %v", err)
	}
	s, err := store.ProbeSummaries(3600)
	if err != nil {
		t.Fatalf("ProbeSummaries: %v", err)
	}
	if len(s) != 1 || s[0].Total != 1 || !s[0].LastOK {
		t.Errorf("window filter failed: got %+v, want 1 sample OK", s)
	}
}

func TestPercentile(t *testing.T) {
	cases := []struct {
		values []int64
		p      float64
		want   int64
	}{
		{[]int64{}, 0.5, 0},
		{[]int64{10}, 0.5, 10},
		// nearest-rank: rank = floor(p*n); for odd n=3 p=0.5 -> values[1]=20
		{[]int64{10, 20, 30}, 0.5, 20},
		{[]int64{10, 20, 30, 40, 50}, 0.95, 50},
		// unsorted input -> sorted in-place; rank=floor(0.5*5)=2 -> values[2]=30
		{[]int64{50, 40, 30, 20, 10}, 0.5, 30},
	}
	for _, tc := range cases {
		v := append([]int64(nil), tc.values...) // don't mutate caller
		got := percentile(v, tc.p)
		if got != tc.want {
			t.Errorf("percentile(%v, %v) = %d, want %d", tc.values, tc.p, got, tc.want)
		}
	}
}

func TestProbeKindIsPersistedAndFilterable(t *testing.T) {
	store := newProbeTestStore(t)
	now := time.Now().Unix()
	httpR := []protocol.ProbeResult{
		{URL: "https://api.example/health", Kind: protocol.ProbeKindHTTP, StatusCode: 200, LatencyMs: 90, OK: true, Ts: now - 30},
	}
	tcpR := []protocol.ProbeResult{
		{URL: "db:5432", Kind: protocol.ProbeKindTCP, LatencyMs: 12, OK: true, Ts: now - 20},
		{URL: "cache:6379", Kind: protocol.ProbeKindTCP, LatencyMs: 5, OK: false, Error: "connection_refused", Ts: now - 10},
	}
	if err := store.RecordProbeResults("node-1", httpR); err != nil {
		t.Fatalf("http record: %v", err)
	}
	if err := store.RecordProbeResults("node-1", tcpR); err != nil {
		t.Fatalf("tcp record: %v", err)
	}

	all, err := store.ProbeSummaries(3600)
	if err != nil {
		t.Fatalf("ProbeSummaries: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all summaries len = %d, want 3", len(all))
	}
	byURL := map[string]ProbeSummary{}
	for _, s := range all {
		byURL[s.URL] = s
	}
	if byURL["https://api.example/health"].Kind != protocol.ProbeKindHTTP {
		t.Errorf("http kind = %q, want %q", byURL["https://api.example/health"].Kind, protocol.ProbeKindHTTP)
	}
	if byURL["db:5432"].Kind != protocol.ProbeKindTCP {
		t.Errorf("tcp kind = %q, want %q", byURL["db:5432"].Kind, protocol.ProbeKindTCP)
	}

	tcpOnly, err := store.ProbeSummariesByKind(3600, protocol.ProbeKindTCP)
	if err != nil {
		t.Fatalf("ProbeSummariesByKind: %v", err)
	}
	if len(tcpOnly) != 2 {
		t.Errorf("tcp-only len = %d, want 2", len(tcpOnly))
	}
	for _, s := range tcpOnly {
		if s.Kind != protocol.ProbeKindTCP {
			t.Errorf("tcp-only summary kind = %q for %s, want tcp", s.Kind, s.URL)
		}
	}

	// Unknown kind returns empty, not error.
	none, err := store.ProbeSummariesByKind(3600, "icmp")
	if err != nil {
		t.Errorf("ProbeSummariesByKind(icmp) err = %v, want nil", err)
	}
	if len(none) != 0 {
		t.Errorf("icmp summaries len = %d, want 0", len(none))
	}
}

func TestRecordProbeResultsDefaultsKindToHTTP(t *testing.T) {
	store := newProbeTestStore(t)
	now := time.Now().Unix()
	// Kind intentionally omitted — pre-TCP-probe agents send this shape
	// and the store must keep treating them as HTTP on disk.
	r := []protocol.ProbeResult{
		{URL: "https://legacy.example/health", StatusCode: 200, OK: true, Ts: now - 10},
	}
	if err := store.RecordProbeResults("node-1", r); err != nil {
		t.Fatalf("record: %v", err)
	}
	all, _ := store.ProbeSummaries(3600)
	if len(all) != 1 {
		t.Fatalf("len = %d, want 1", len(all))
	}
	if all[0].Kind != protocol.ProbeKindHTTP {
		t.Errorf("legacy kind = %q, want %q (legacy rows must default to http)",
			all[0].Kind, protocol.ProbeKindHTTP)
	}
}
