package collector

import (
	"testing"
)

const procNetDevFixture = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1234567   12345    0    0    0     0          0         0  1234567   12345    0    0    0     0       0          0
  eth0: 9876543210 8765432    1    2    0     0          0         0  1234567890 7654321    3    4    0     0       0          0
 wlan0:    10000      100    0    0    0     0          0         0     20000      200    0    0    0     0       0          0
 tun0:     5000       50    0    0    0     0          0         0     4000       40    0    0    0     0       0          0
`

func TestCollectNetworkInterfaces_ParseAndFilter(t *testing.T) {
	// Validate the parser via the testable inner function; the production
	// CollectNetworkInterfaces() reads /proc/net/dev directly and would
	// require root + a real NIC to exercise end-to-end.
	got := parseProcNetDev([]byte(procNetDevFixture))
	wantIfaces := map[string]bool{"eth0": false, "wlan0": false, "tun0": false}
	for _, n := range got {
		if _, ok := wantIfaces[n.Iface]; !ok {
			t.Fatalf("unexpected iface %q", n.Iface)
		}
		wantIfaces[n.Iface] = true
	}
	for iface, seen := range wantIfaces {
		if !seen {
			t.Fatalf("missing iface %q", iface)
		}
	}
}

func TestParseProcNetDev_ValuesExact(t *testing.T) {
	got := parseProcNetDev([]byte(procNetDevFixture))
	var eth0 *struct{ rx, tx, rxp, txp uint64 }
	for i := range got {
		if got[i].Iface == "eth0" {
			eth0 = &struct{ rx, tx, rxp, txp uint64}{got[i].RxBytes, got[i].TxBytes, got[i].RxPackets, got[i].TxPackets}
		}
	}
	if eth0 == nil {
		t.Fatal("eth0 missing")
	}
	if eth0.rx != 9876543210 {
		t.Errorf("eth0 rx=%d want 9876543210", eth0.rx)
	}
	if eth0.tx != 1234567890 {
		t.Errorf("eth0 tx=%d want 1234567890", eth0.tx)
	}
	if eth0.rxp != 8765432 {
		t.Errorf("eth0 rxp=%d want 8765432", eth0.rxp)
	}
	if eth0.txp != 7654321 {
		t.Errorf("eth0 txp=%d want 7654321", eth0.txp)
	}
}

func TestParseProcNetDev_LoExcluded(t *testing.T) {
	got := parseProcNetDev([]byte(procNetDevFixture))
	for _, n := range got {
		if n.Iface == "lo" {
			t.Fatalf("lo must be filtered out, got %+v", n)
		}
	}
}

func TestParseProcNetDev_HandlesShortLines(t *testing.T) {
	short := "Inter-|   Receive"
	got := parseProcNetDev([]byte(short))
	if len(got) != 0 {
		t.Fatalf("expected empty for header-only input, got %d", len(got))
	}
}

func TestParseProcNetDev_HandlesMissingTxColumns(t *testing.T) {
	// Only 4 columns -> tx fields stay zero.
	short := "  eth1: 100 200 1 2\n"
	got := parseProcNetDev([]byte(short))
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	if got[0].RxBytes != 100 || got[0].TxBytes != 0 {
		t.Errorf("partial columns parsed incorrectly: %+v", got[0])
	}
}
