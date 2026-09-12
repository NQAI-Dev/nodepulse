package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestRecordNetworkSamples_FirstObservationZeroRate(t *testing.T) {
	p := newTestStore(t)
	rates := p.RecordNetworkSamples("node-a", []NetworkSample{
		{Iface: "eth0", RxBytes: 1000, TxBytes: 500, Timestamp: time.Now().Unix()},
	})
	if len(rates) != 1 {
		t.Fatalf("want 1 rate, got %d", len(rates))
	}
	if rates[0].Iface != "eth0" {
		t.Fatalf("iface=%q", rates[0].Iface)
	}
	if rates[0].RxBytesPerSec != 0 || rates[0].WindowSec != 0 {
		t.Fatalf("first observation must be zero-rate, got %+v", rates[0])
	}
}

func TestRecordNetworkSamples_DerivesRates(t *testing.T) {
	p := newTestStore(t)
	now := time.Now().Unix()
	// 10-second window with +10240 bytes rx and +5 packets tx.
	p.RecordNetworkSamples("node-b", []NetworkSample{
		{Iface: "eth0", Timestamp: now - 10,
			RxBytes: 1000, TxBytes: 2000,
			RxPackets: 100, TxPackets: 200,
			RxErrors: 0, TxErrors: 0, RxDrops: 0, TxDrops: 0},
	})
	rates := p.RecordNetworkSamples("node-b", []NetworkSample{
		{Iface: "eth0", Timestamp: now,
			RxBytes: 11240, TxBytes: 2005,
			RxPackets: 105, TxPackets: 202,
			RxErrors: 1, TxErrors: 0, RxDrops: 12, TxDrops: 0},
	})
	if len(rates) != 1 {
		t.Fatalf("want 1 rate, got %d", len(rates))
	}
	r := rates[0]
	if r.WindowSec != 10 {
		t.Errorf("window=%d want 10", r.WindowSec)
	}
	// 10240 / 10 = 1024
	if r.RxBytesPerSec != 1024 {
		t.Errorf("rx_bps=%v want 1024", r.RxBytesPerSec)
	}
	// 5 / 10 = 0.5
	if r.TxBytesPerSec != 0.5 {
		t.Errorf("tx_bps=%v want 0.5", r.TxBytesPerSec)
	}
	if r.RxErrorsPerSec != 0.1 {
		t.Errorf("rx_errps=%v want 0.1", r.RxErrorsPerSec)
	}
	if r.RxDropsPerSec != 1.2 {
		t.Errorf("rx_dropps=%v want 1.2", r.RxDropsPerSec)
	}
}

func TestRecordNetworkSamples_HandlesRollover(t *testing.T) {
	p := newTestStore(t)
	now := time.Now().Unix()
	p.RecordNetworkSamples("node-c", []NetworkSample{
		{Iface: "eth0", Timestamp: now - 5, RxBytes: ^uint64(0) - 100},
	})
	rates := p.RecordNetworkSamples("node-c", []NetworkSample{
		{Iface: "eth0", Timestamp: now, RxBytes: 50},
	})
	if rates[0].RxBytesPerSec != 30 { // (100 + 50) / 5
		t.Errorf("rollover rate=%v want 30", rates[0].RxBytesPerSec)
	}
	if rates[0].WindowSec != 5 {
		t.Errorf("rollover window=%d want 5", rates[0].WindowSec)
	}
}

func TestRecordNetworkSamples_PerInterfaceIsolated(t *testing.T) {
	p := newTestStore(t)
	now := time.Now().Unix()
	rates := p.RecordNetworkSamples("node-d", []NetworkSample{
		{Iface: "eth0", Timestamp: now - 1, RxBytes: 1000},
		{Iface: "wlan0", Timestamp: now - 1, RxBytes: 9999},
	})
	if len(rates) != 2 {
		t.Fatalf("want 2 rates, got %d", len(rates))
	}
	rates = p.RecordNetworkSamples("node-d", []NetworkSample{
		{Iface: "eth0", Timestamp: now, RxBytes: 2000},
		{Iface: "wlan0", Timestamp: now, RxBytes: 9999}, // no change
	})
	if rates[0].RxBytesPerSec != 1000 {
		t.Errorf("eth0 rate=%v want 1000", rates[0].RxBytesPerSec)
	}
	if rates[1].RxBytesPerSec != 0 {
		t.Errorf("wlan0 rate=%v want 0", rates[1].RxBytesPerSec)
	}
}

func TestEvaluateNetworkAlerts_OpensIncidents(t *testing.T) {
	p := newTestStore(t)
	rec := &alerterRecordingNotifier{}
	p.SetNotifier(rec)
	// Mirror production ingest: bind the node so the notifier has an
	// owner to look up settings for.
	p.BindNode("node-e", 1)

	rates := []NetworkRate{
		{Iface: "eth0", RxErrorsPerSec: 5.0, TxErrorsPerSec: 0},
		{Iface: "wlan0", RxDropsPerSec: 20.0, TxDropsPerSec: 0},
		{Iface: "ok0", RxErrorsPerSec: 0.1, RxDropsPerSec: 0},
	}
	p.EvaluateNetworkAlerts("node-e", rates)
	if len(rec.Incidents) != 2 {
		t.Fatalf("want 2 incidents (errors + drops), got %d", len(rec.Incidents))
	}
	titles := map[string]bool{}
	for _, inc := range rec.Incidents {
		titles[inc.Title] = true
	}
	if !titles["Interface Errors Rising: eth0"] {
		t.Errorf("missing errors incident")
	}
	if !titles["Interface Drops Rising: wlan0"] {
		t.Errorf("missing drops incident")
	}
}

func TestEvaluateNetworkAlerts_RespectsThresholds(t *testing.T) {
	p := newTestStore(t)
	rec := &alerterRecordingNotifier{}
	p.SetNotifier(rec)

	rates := []NetworkRate{{Iface: "eth0", RxErrorsPerSec: 0.5}}
	p.EvaluateNetworkAlerts("node-f", rates)
	if len(rec.Incidents) != 0 {
		t.Fatalf("sub-threshold errors should not alert, got %d", len(rec.Incidents))
	}
}

func TestDiffUint64_Normal(t *testing.T) {
	if d := diffUint64(1000, 100); d != 900 {
		t.Errorf("diff=%d want 900", d)
	}
}

func TestDiffUint64_Rollover(t *testing.T) {
	maxU := ^uint64(0)
	d := diffUint64(10, maxU-5)
	if d != 15 {
		t.Errorf("rollover diff=%d want 15", d)
	}
}

// alerterRecordingNotifier captures calls for assertion.
type alerterRecordingNotifier struct {
	Incidents []notifierCall
	Resolves  []notifierCall
}

type notifierCall struct {
	ChatID   int64
	NodeID   string
	Severity string
	Title    string
	Detail   string
}

func (a *alerterRecordingNotifier) NotifyIncidentTo(chatID int64, nodeID, severity, title, detail string) {
	a.Incidents = append(a.Incidents, notifierCall{chatID, nodeID, severity, title, detail})
}

func (a *alerterRecordingNotifier) NotifyResolvedTo(chatID int64, nodeID, severity, title string) {
	a.Resolves = append(a.Resolves, notifierCall{ChatID: chatID, NodeID: nodeID, Severity: severity, Title: title})
}

// helper: silence unused-import vet if filepath not used elsewhere in this file.
var _ = filepath.Join
