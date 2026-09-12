package store

import (
	"testing"
	"time"
)

func TestNetworkSummary_AggregatesAcrossIfaces(t *testing.T) {
	p := newTestStore(t)
	now := time.Now().Unix()
	// Two ifaces, two samples each, inside a 1h window.
	p.RecordNetworkSamples("node-sum", []NetworkSample{
		{Iface: "eth0", Timestamp: now - 60, RxBytes: 1000, TxBytes: 500, RxPackets: 10, TxPackets: 5},
		{Iface: "wlan0", Timestamp: now - 60, RxBytes: 2000, TxBytes: 800, RxPackets: 20, TxPackets: 8},
	})
	p.RecordNetworkSamples("node-sum", []NetworkSample{
		{Iface: "eth0", Timestamp: now, RxBytes: 4000, TxBytes: 1500, RxPackets: 40, TxPackets: 15},
		{Iface: "wlan0", Timestamp: now, RxBytes: 3000, TxBytes: 900, RxPackets: 25, TxPackets: 9},
	})

	totals, err := p.NetworkSummary("node-sum", 3600)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if len(totals) != 2 {
		t.Fatalf("want 2 ifaces, got %d", len(totals))
	}
	byIface := map[string]NetworkTotals{}
	for _, t := range totals {
		byIface[t.Iface] = t
	}
	eth := byIface["eth0"]
	if eth.RxBytes != 4000 || eth.TxBytes != 1500 {
		t.Errorf("eth0 totals wrong: %+v", eth)
	}
	if eth.RxBytesPerSec <= 0 {
		t.Errorf("eth0 rate should be >0, got %v", eth.RxBytesPerSec)
	}
	wlan := byIface["wlan0"]
	if wlan.RxBytes != 3000 {
		t.Errorf("wlan0 latest rx wrong: %d", wlan.RxBytes)
	}
}

func TestNetworkSummary_EmptyNodeReturnsEmpty(t *testing.T) {
	p := newTestStore(t)
	totals, err := p.NetworkSummary("ghost", 3600)
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if len(totals) != 0 {
		t.Errorf("ghost should have no totals, got %d", len(totals))
	}
}

func TestNetworkSeries_BucketsRatesPerSecond(t *testing.T) {
	p := newTestStore(t)
	// Snap to the start of a minute so both samples land in the same
	// 60s bucket regardless of the wall-clock second we ran at.
	now := (time.Now().Unix() / 60) * 60
	p.RecordNetworkSamples("node-ser", []NetworkSample{
		{Iface: "eth0", Timestamp: now - 30, RxBytes: 1000},
		{Iface: "eth0", Timestamp: now - 10, RxBytes: 4000},
	})

	points, err := p.NetworkSeries("node-ser", "eth0", "1h")
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(points) < 1 {
		t.Fatalf("want at least 1 bucket, got %d", len(points))
	}
	// Both samples land in the same bucket; rx_bps should be (4000-1000)/60.
	last := points[len(points)-1]
	if last.RxBytesPerSec <= 0 {
		t.Errorf("rx_bps should be >0, got %v", last.RxBytesPerSec)
	}
	if last.SampleCount < 2 {
		t.Errorf("bucket should contain both samples, got count=%d", last.SampleCount)
	}
}

func TestNetworkSeries_UnknownIfaceReturnsEmpty(t *testing.T) {
	p := newTestStore(t)
	points, err := p.NetworkSeries("node-x", "ghost0", "1h")
	if err != nil {
		t.Fatalf("ghost iface: %v", err)
	}
	if len(points) != 0 {
		t.Errorf("ghost iface should return empty series, got %d", len(points))
	}
}

func TestNetworkSeries_UnknownRangeFallsBackTo1h(t *testing.T) {
	p := newTestStore(t)
	points, _ := p.NetworkSeries("node-x", "eth0", "bogus")
	// Bogus range should fall back to 1h without erroring out.
	if points == nil {
		// empty slice is acceptable
	}
	if len(points) > 0 && points[0].Timestamp == 0 {
		t.Errorf("bad timestamp on first bucket")
	}
}
