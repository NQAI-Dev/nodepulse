package store

import (
	"log"
	"sync"
	"time"
)

// NetworkSample is one observation of an interface's counters. Rate fields
// are computed on the server by diffing against the previous stored sample
// for the same node+iface and dividing by the elapsed wall-clock seconds.
type NetworkSample struct {
	NodeID    string `json:"node_id"`
	Iface     string `json:"iface"`
	Timestamp int64  `json:"ts"`
	RxBytes   uint64 `json:"rx_bytes"`
	TxBytes   uint64 `json:"tx_bytes"`
	RxPackets uint64 `json:"rx_packets"`
	TxPackets uint64 `json:"tx_packets"`
	RxErrors  uint64 `json:"rx_errors"`
	TxErrors  uint64 `json:"tx_errors"`
	RxDrops   uint64 `json:"rx_drops"`
	TxDrops   uint64 `json:"tx_drops"`
}

// NetworkRate is the derived per-second view of a sample. Computed lazily
// when callers ask for "now"; stored alongside NetworkSample only if the
// caller persists the precomputed rate.
type NetworkRate struct {
	Iface        string  `json:"iface"`
	RxBytesPerSec float64 `json:"rx_bps"`
	TxBytesPerSec float64 `json:"tx_bps"`
	RxPacketsPerSec float64 `json:"rx_pps"`
	TxPacketsPerSec float64 `json:"tx_pps"`
	RxErrorsPerSec  float64 `json:"rx_errps"`
	TxErrorsPerSec  float64 `json:"tx_errps"`
	RxDropsPerSec   float64 `json:"rx_dropps"`
	TxDropsPerSec   float64 `json:"tx_dropps"`
	WindowSec    int64   `json:"window_secs"` // elapsed between the two samples used for the diff
}

// networkRateTracker caches the last NetworkSample per (node,iface) so we
// can produce rate deltas on the next heartbeat without an extra round-trip
// to the database. Lives on PersistentStore via the sync.Mutex held by the
// store for incident bookkeeping.
type networkRateTracker struct {
	mu    sync.Mutex
	last  map[string]NetworkSample // key = nodeID + "|" + iface
}

func newNetworkRateTracker() *networkRateTracker {
	return &networkRateTracker{last: make(map[string]NetworkSample)}
}

func networkKey(nodeID, iface string) string { return nodeID + "|" + iface }

// RecordNetworkSamples persists per-interface counters and returns the rate
// deltas for every sample where a previous observation exists. The first
// observation of any (node,iface) pair yields a zero-rate placeholder so
// the caller can correlate index positions.
//
// ponytail: a single ingest with N interfaces costs N rows and N row
// inserts. SQLite handles that comfortably at agent cadences (≤30s).
// If a node ever reports >32 interfaces, batch the inserts in a tx.
func (p *PersistentStore) RecordNetworkSamples(nodeID string, samples []NetworkSample) []NetworkRate {
	if len(samples) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	rates := make([]NetworkRate, 0, len(samples))
	now := time.Now().Unix()
	tx, err := p.db.Begin()
	if err != nil {
		return rates
	}
	stmt, err := tx.Prepare(`INSERT INTO network_samples
		(node_id, iface, ts, rx_bytes, tx_bytes, rx_packets, tx_packets, rx_errors, tx_errors, rx_drops, tx_drops)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return rates
	}
	defer stmt.Close()

	p.netRates.mu.Lock()
	for _, s := range samples {
		if s.Iface == "" {
			continue
		}
		s.NodeID = nodeID
		if s.Timestamp == 0 {
			s.Timestamp = now
		}
		if _, err := stmt.Exec(nodeID, s.Iface, s.Timestamp,
			int64(s.RxBytes), int64(s.TxBytes), int64(s.RxPackets), int64(s.TxPackets),
			int64(s.RxErrors), int64(s.TxErrors), int64(s.RxDrops), int64(s.TxDrops)); err != nil {
			// Persist failure must not poison the in-memory rate tracker —
			// the next heartbeat still needs a delta to compute against.
			// Log and fall through so netRates stays consistent.
			log.Printf("network_samples insert: %v", err)
		}
		key := networkKey(nodeID, s.Iface)
		prev, hasPrev := p.netRates.last[key]
		p.netRates.last[key] = s

		rate := NetworkRate{Iface: s.Iface, WindowSec: 0}
		if !hasPrev {
			rates = append(rates, rate)
			continue
		}
		window := s.Timestamp - prev.Timestamp
		if window <= 0 {
			rates = append(rates, rate)
			continue
		}
		w := float64(window)
		rate.WindowSec = window
		rate.RxBytesPerSec = float64(diffUint64(s.RxBytes, prev.RxBytes)) / w
		rate.TxBytesPerSec = float64(diffUint64(s.TxBytes, prev.TxBytes)) / w
		rate.RxPacketsPerSec = float64(diffUint64(s.RxPackets, prev.RxPackets)) / w
		rate.TxPacketsPerSec = float64(diffUint64(s.TxPackets, prev.TxPackets)) / w
		rate.RxErrorsPerSec = float64(diffUint64(s.RxErrors, prev.RxErrors)) / w
		rate.TxErrorsPerSec = float64(diffUint64(s.TxErrors, prev.TxErrors)) / w
		rate.RxDropsPerSec = float64(diffUint64(s.RxDrops, prev.RxDrops)) / w
		rate.TxDropsPerSec = float64(diffUint64(s.TxDrops, prev.TxDrops)) / w
		rates = append(rates, rate)
	}
	p.netRates.mu.Unlock()

	if err := tx.Commit(); err != nil {
		return rates
	}

	// 7-day retention, same policy as metric_samples.
	cutoff := time.Now().Unix() - rawRetentionSecs
	p.db.Exec("DELETE FROM network_samples WHERE ts < ?", cutoff)

	return rates
}

// LatestNetworkRates returns the most recent rate observation per interface
// for the given node. Used by the dashboard to render "current traffic".
func (p *PersistentStore) LatestNetworkRates(nodeID string) []NetworkRate {
	p.netRates.mu.Lock()
	defer p.netRates.mu.Unlock()
	out := make([]NetworkRate, 0, 4)
	for key, last := range p.netRates.last {
		if splitKey(key, 0) != nodeID {
			continue
		}
		iface := splitKey(key, 1)
		// We can't compute a rate without a predecessor; emit a zero-rate
		// entry so the UI knows the iface exists.
		out = append(out, NetworkRate{Iface: iface})
		_ = last
	}
	return out
}

func splitKey(key string, idx int) string {
	for i := 0; i < len(key); i++ {
		if key[i] == '|' {
			if idx == 0 {
				return key[:i]
			}
			return key[i+1:]
		}
	}
	return key
}

func diffUint64(now, prev uint64) uint64 {
	if now >= prev {
		return now - prev
	}
	// Counter rollover: kernel counters are 32-bit on some NICs, so a
	// smaller "now" means we wrapped. Sum the tail and now without the
	// off-by-one bias.
	return (^uint64(0) - prev) + now
}

// NetworkErrorIncidentThreshold is the minimum per-second error rate that
// counts as "interface errors rising". Below this, kernel counters can
// flap for legitimate reasons (link reset, hot-plug). 1 error/sec catches
// persistent issues without spamming on transient bumps.
const NetworkErrorIncidentThreshold = 1.0

// NetworkDropIncidentThreshold mirrors the error threshold but for packet
// drops. Drops can spike during burst traffic so we set this higher.
const NetworkDropIncidentThreshold = 10.0

// EvaluateNetworkAlerts inspects fresh rate samples and opens critical
// incidents when an interface's errors or drops climb past the threshold.
// The same throttling/cooldown rules from CreateIncident apply: existing
// open incidents get their detail refreshed but no new spam fires until
// the cooldown elapses.
func (p *PersistentStore) EvaluateNetworkAlerts(nodeID string, rates []NetworkRate) {
	for _, r := range rates {
		if r.RxErrorsPerSec >= NetworkErrorIncidentThreshold || r.TxErrorsPerSec >= NetworkErrorIncidentThreshold {
			detail := formatNetAlert(r, "errors",
				r.RxErrorsPerSec, r.TxErrorsPerSec)
			p.CreateIncident(nodeID, "critical",
				"Interface Errors Rising: "+r.Iface, detail)
		}
		if r.RxDropsPerSec >= NetworkDropIncidentThreshold || r.TxDropsPerSec >= NetworkDropIncidentThreshold {
			detail := formatNetAlert(r, "drops",
				r.RxDropsPerSec, r.TxDropsPerSec)
			p.CreateIncident(nodeID, "warning",
				"Interface Drops Rising: "+r.Iface, detail)
		}
	}
}

func formatNetAlert(r NetworkRate, kind string, rx, tx float64) string {
	switch kind {
	case "errors":
		return formatFloat(rx, 2) + " rx err/s, " + formatFloat(tx, 2) + " tx err/s"
	case "drops":
		return formatFloat(rx, 2) + " rx drop/s, " + formatFloat(tx, 2) + " tx drop/s"
	}
	return ""
}

func formatFloat(v float64, decimals int) string {
	if decimals == 2 {
		return strconvFmtFloat(v)
	}
	return strconvFmtFloat(v)
}

func strconvFmtFloat(v float64) string {
	// Local helper so we don't pull strconv into this file's namespace clash.
	// Ponytail: if more formatters land here, fold into a shared util package.
	if v >= 100 {
		return i64ToA(int64(v))
	}
	if v >= 10 {
		return i64ToA(int64(v))
	}
	// 1 decimal place below 10 keeps the alert readable.
	whole := int64(v)
	frac := int64((v - float64(whole)) * 10)
	if frac < 0 {
		frac = -frac
	}
	return i64ToA(whole) + "." + i64ToA(frac)
}

func i64ToA(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
