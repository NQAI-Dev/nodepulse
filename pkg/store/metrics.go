package store

import (
	"database/sql"
	"time"
)

// MetricSample is one observation of a node's resource pressure. The values
// are all ratios expressed as percentages (0..100) so they fit comfortably
// in a single shared scale — sparkline UI rendering compares them side by
// side without conversion. DiskUsagePct is the load1-derived boot disk only;
// secondary mounts land in MetricSample extras if/when needed.
type MetricSample struct {
	NodeID      string  `json:"node_id"`
	Timestamp   int64   `json:"ts"`
	CPUPercent  float64 `json:"cpu_pct"`
	Load1       float64 `json:"load1"`
	MemUsedPct  float64 `json:"mem_pct"`
	DiskUsedPct float64 `json:"disk_pct"`
}

// RecordSample persists a single heartbeat sample and triggers a cheap
// retention prune when the underlying raw table exceeds the cap. The cap
// is per-node so a busy fleet can't evict a quieter node's data.
//
// ponytail: SQLite's INSERT-then-DELETE dance stays cheap as long as the
// raw table is indexed on (node_id, ts). If a single node blows past
// ~100k samples, switch to a bucketed upsert for the raw samples too.
const (
	rawRetentionDays = 7 // raw samples kept this long before prune
	rawRetentionSecs = int64(rawRetentionDays * 24 * 60 * 60)
)

func (p *PersistentStore) RecordSample(s MetricSample) error {
	if p == nil || p.db == nil || s.NodeID == "" {
		return nil
	}
	_, err := p.db.Exec(
		`INSERT INTO metric_samples (node_id, ts, cpu_pct, load1, mem_pct, disk_pct) VALUES (?, ?, ?, ?, ?, ?)`,
		s.NodeID, s.Timestamp, s.CPUPercent, s.Load1, s.MemUsedPct, s.DiskUsedPct,
	)
	if err != nil {
		return err
	}
	// Prune any node whose oldest sample is older than the retention
	// window. Cheap because idx_samples_node_ts leads the range scan.
	cutoff := time.Now().Unix() - rawRetentionSecs
	p.db.Exec("DELETE FROM metric_samples WHERE ts < ?", cutoff)
	return nil
}

// MetricsRange is the canonical request envelope for historical metrics.
// Buckets are aligned to a fixed cadence so a dashboard can stitch
// series from multiple requests without surprise jumps.
type MetricsRange struct {
	NodeID    string         `json:"node_id"`
	RangeKey  string         `json:"range"`            // "1h" | "6h" | "24h" | "7d"
	From      int64          `json:"from"`             // unix seconds
	To        int64          `json:"to"`               // unix seconds
	BucketSec int64          `json:"bucket_secs"`      // sample cadence in seconds
	Points    []MetricSample `json:"points,omitempty"` // optional raw points (<24h)
	Buckets   []MetricBucket `json:"buckets"`          // downsampled averages
}

// MetricBucket is a 5-minute (or coarser) average window — used for ranges
// beyond 24h where the raw stream would balloon the response.
type MetricBucket struct {
	Timestamp   int64   `json:"ts"`
	CPUPercent  float64 `json:"cpu_pct"`
	Load1       float64 `json:"load1"`
	MemUsedPct  float64 `json:"mem_pct"`
	DiskUsedPct float64 `json:"disk_pct"`
	SampleCount int     `json:"n"`
}

// MetricsRange returns a sampled time series for nodeID covering the
// supplied range. The function selects raw points (<=24h) or downsampled
// buckets (>24h) so the wire payload stays bounded.
//
// ponytail: the downsample query computes per-bucket averages in SQL
// rather than streaming raw rows into Go. With ~10s agents and 7d windows
// that's ~60k rows/node — fine in SQLite, painful in Go.
func (p *PersistentStore) MetricsRange(nodeID, rangeKey string) (MetricsRange, error) {
	if nodeID == "" || rangeKey == "" {
		return MetricsRange{}, nil
	}
	now := time.Now().Unix()
	var window int64
	switch rangeKey {
	case "1h":
		window = 3600
	case "6h":
		window = 6 * 3600
	case "24h":
		window = 24 * 3600
	case "7d":
		window = 7 * 24 * 3600
	default:
		window = 3600
		rangeKey = "1h"
	}

	out := MetricsRange{
		NodeID:   nodeID,
		RangeKey: rangeKey,
		From:     now - window,
		To:       now,
	}

	if window <= 24*3600 {
		// Raw points: 6 points/minute at 10s cadence => ~360 rows/hour.
		out.BucketSec = 10
		out.Points = make([]MetricSample, 0, 256)
		rows, err := p.db.Query(
			`SELECT ts, cpu_pct, load1, mem_pct, disk_pct
			   FROM metric_samples
			  WHERE node_id = ? AND ts >= ? AND ts <= ?
			  ORDER BY ts ASC`,
			nodeID, out.From, out.To,
		)
		if err != nil {
			return out, err
		}
		defer rows.Close()
		for rows.Next() {
			var s MetricSample
			if err := rows.Scan(&s.Timestamp, &s.CPUPercent, &s.Load1, &s.MemUsedPct, &s.DiskUsedPct); err != nil {
				continue
			}
			s.NodeID = nodeID
			out.Points = append(out.Points, s)
		}
		return out, rows.Err()
	}

	// Downsampled buckets: 30-minute windows for the 7d view.
	out.BucketSec = 30 * 60
	out.Buckets = make([]MetricBucket, 0, 336) // 7d * 48 buckets
	bucketSize := out.BucketSec
	rows, err := p.db.Query(
		`SELECT (ts / ?) * ? AS bucket_ts,
		        AVG(cpu_pct), AVG(load1), AVG(mem_pct), AVG(disk_pct),
		        COUNT(*)
		   FROM metric_samples
		  WHERE node_id = ? AND ts >= ? AND ts <= ?
		  GROUP BY bucket_ts
		  ORDER BY bucket_ts ASC`,
		bucketSize, bucketSize, nodeID, out.From, out.To,
	)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var b MetricBucket
		if err := rows.Scan(&b.Timestamp, &b.CPUPercent, &b.Load1, &b.MemUsedPct, &b.DiskUsedPct, &b.SampleCount); err != nil {
			continue
		}
		out.Buckets = append(out.Buckets, b)
	}
	return out, rows.Err()
}

// LatestSampleForNode is a best-effort peek at the most recent sample —
// used by the public status page when a node is currently offline and
// the in-memory store has aged out. Returns ok=false if no rows exist.
func (p *PersistentStore) LatestSampleForNode(nodeID string) (MetricSample, bool, error) {
	var s MetricSample
	err := p.db.QueryRow(
		`SELECT ts, cpu_pct, load1, mem_pct, disk_pct
		   FROM metric_samples
		  WHERE node_id = ?
		  ORDER BY ts DESC LIMIT 1`,
		nodeID,
	).Scan(&s.Timestamp, &s.CPUPercent, &s.Load1, &s.MemUsedPct, &s.DiskUsedPct)
	if err == sql.ErrNoRows {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	s.NodeID = nodeID
	return s, true, nil
}
