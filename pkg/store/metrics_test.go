package store

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *PersistentStore {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	p, err := NewPersistentStore(dbPath, "", 0)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { p.db.Close() })
	return p
}

func TestRecordSample_RoundTrip(t *testing.T) {
	p := newTestStore(t)
	now := time.Now().Unix()

	if err := p.RecordSample(MetricSample{
		NodeID: "node-a", Timestamp: now - 30,
		CPUPercent: 12.5, Load1: 0.5, MemUsedPct: 40.0, DiskUsedPct: 60.0,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := p.RecordSample(MetricSample{
		NodeID: "node-a", Timestamp: now,
		CPUPercent: 80.0, Load1: 3.5, MemUsedPct: 95.0, DiskUsedPct: 70.0,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	out, err := p.MetricsRange("node-a", "1h")
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	if len(out.Points) != 2 {
		t.Fatalf("want 2 points, got %d", len(out.Points))
	}
	if out.Points[0].CPUPercent != 12.5 {
		t.Fatalf("first sample cpu=%v", out.Points[0].CPUPercent)
	}
	if out.Points[1].MemUsedPct != 95.0 {
		t.Fatalf("second sample mem=%v", out.Points[1].MemUsedPct)
	}
	if out.BucketSec != 10 {
		t.Fatalf("want bucket=10s for 1h, got %d", out.BucketSec)
	}
	if out.RangeKey != "1h" {
		t.Fatalf("range key echo: %q", out.RangeKey)
	}
}

func TestMetricsRange_FallsBackToBuckets(t *testing.T) {
	p := newTestStore(t)
	now := time.Now().Unix()
	// Sprinkle 6 samples ~30 minutes apart to populate more than one bucket.
	for i := 0; i < 6; i++ {
		ts := now - int64(i)*30*60
		if err := p.RecordSample(MetricSample{
			NodeID: "node-b", Timestamp: ts,
			CPUPercent: float64(10 * i), Load1: float64(i),
			MemUsedPct: 50, DiskUsedPct: 60,
		}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}

	out, err := p.MetricsRange("node-b", "7d")
	if err != nil {
		t.Fatalf("range 7d: %v", err)
	}
	if len(out.Buckets) == 0 {
		t.Fatal("expected downsampled buckets for 7d window")
	}
	if len(out.Points) != 0 {
		t.Fatalf("7d should not return raw points, got %d", len(out.Points))
	}
	if out.BucketSec != 30*60 {
		t.Fatalf("bucket size for 7d should be 30m, got %d", out.BucketSec)
	}
	// All samples fall within the latest 3 buckets (3h span).
	for _, b := range out.Buckets {
		if b.SampleCount <= 0 {
			t.Fatalf("bucket with zero samples: %+v", b)
		}
	}
}

func TestMetricsRange_UnknownRangeFallsBackToHour(t *testing.T) {
	p := newTestStore(t)
	now := time.Now().Unix()
	if err := p.RecordSample(MetricSample{
		NodeID: "node-c", Timestamp: now,
		CPUPercent: 5, MemUsedPct: 10,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	out, _ := p.MetricsRange("node-c", "bogus")
	if out.RangeKey != "1h" {
		t.Fatalf("unknown range should normalize to 1h, got %q", out.RangeKey)
	}
}

func TestRecordSample_PrunesBeyondRetention(t *testing.T) {
	p := newTestStore(t)
	now := time.Now().Unix()

	// 8 days old -> should be pruned on the next RecordSample call.
	if err := p.RecordSample(MetricSample{
		NodeID: "node-d", Timestamp: now - 8*24*3600,
	}); err != nil {
		t.Fatalf("old record: %v", err)
	}
	// Recent sample triggers the prune.
	if err := p.RecordSample(MetricSample{
		NodeID: "node-d", Timestamp: now,
	}); err != nil {
		t.Fatalf("recent record: %v", err)
	}

	out, _ := p.MetricsRange("node-d", "7d")
	// Only the recent sample survives the 7d bucket query because the 8d-old
	// sample was pruned at insert time.
	totalBuckets := 0
	for _, b := range out.Buckets {
		totalBuckets += b.SampleCount
	}
	if totalBuckets != 1 {
		t.Fatalf("expected 1 sample across buckets, got %d (buckets=%d)", totalBuckets, len(out.Buckets))
	}
}

func TestLatestSampleForNode(t *testing.T) {
	p := newTestStore(t)
	now := time.Now().Unix()
	for i := 0; i < 5; i++ {
		_ = p.RecordSample(MetricSample{
			NodeID: "node-e", Timestamp: now - int64(5-i)*10,
			CPUPercent: float64(i), MemUsedPct: 30,
		})
	}
	s, ok, err := p.LatestSampleForNode("node-e")
	if err != nil || !ok {
		t.Fatalf("latest: ok=%v err=%v", ok, err)
	}
	if s.CPUPercent != 4 {
		t.Fatalf("want latest cpu=4, got %v", s.CPUPercent)
	}

	if _, ok, _ := p.LatestSampleForNode("ghost"); ok {
		t.Fatal("ghost node should report ok=false")
	}
}

func TestRecordSample_IgnoresEmptyNode(t *testing.T) {
	p := newTestStore(t)
	if err := p.RecordSample(MetricSample{NodeID: "", Timestamp: time.Now().Unix()}); err != nil {
		t.Fatalf("empty node id should be a no-op, got %v", err)
	}
}
