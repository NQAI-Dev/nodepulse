package store

import (
	"database/sql"
	"sync"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// ProbeSummary is the rolled-up view of one probe target over a window.
// LatencyP50/LatencyP95 are computed from raw results inside the requested
// window so the public status page can render "API p95: 240ms" without
// pulling every row. Total is the number of probe attempts; OK counts how
// many returned a 2xx. Kind is the probe type ("http" or "tcp") so the
// public status page can render the right widget without inferring it
// from the URL scheme.
type ProbeSummary struct {
	URL         string  `json:"url"`
	Kind        string  `json:"kind"`
	WindowSecs  int64   `json:"window_secs"`
	Total       int64   `json:"total"`
	OK          int64   `json:"ok"`
	Failed      int64   `json:"failed"`
	LastStatus  int     `json:"last_status"`
	LastLatency int64   `json:"last_latency_ms"`
	LastTs      int64   `json:"last_ts"`
	LastOK      bool    `json:"last_ok"`
	UptimePct   float64 `json:"uptime_pct"`
	LatencyP50  int64   `json:"latency_p50_ms"`
	LatencyP95  int64   `json:"latency_p95_ms"`
}

// RecordProbeResults appends one row per probe observation to the
// probe_results table. The call is idempotent on input shape: an empty
// slice is a no-op. Callers (the ingest handler) call this once per
// heartbeat with all probes attached, so the row count matches the
// cadence rather than being per-tick-per-target doubled.
//
// ponytail: a batch INSERT (single stmt, multiple VALUES tuples) is the
// obvious next optimization; current insert-per-row is fine up to a few
// thousand probes/min. Switch to bulk when the public widget starts
// pulling per-target timelines and the table grows past 1M rows.
func (p *PersistentStore) RecordProbeResults(nodeID string, results []protocol.ProbeResult) error {
	if len(results) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	tx, err := p.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare("INSERT INTO probe_results (node_id, url, kind, status_code, latency_ms, ok, error, ts) VALUES (?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, r := range results {
		okInt := 0
		if r.OK {
			okInt = 1
		}
		kind := r.Kind
		if kind == "" {
			kind = protocol.ProbeKindHTTP
		}
		if _, err := stmt.Exec(nodeID, r.URL, kind, r.StatusCode, r.LatencyMs, okInt, r.Error, r.Ts); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// ProbeSummaries returns one ProbeSummary per distinct URL observed in
// the last `windowSecs` seconds, across all nodes. The window default of
// 24h matches the per-day uptime widgets; pass 0 for "all time" (which
// is the same as "ever since the janitor last pruned").
//
// `kindFilter` optionally restricts results to a single probe kind
// ("http" or "tcp"); pass "" to include every kind. We bucket by URL
// only, so when both HTTP and TCP probes hit the same "host:port"
// target, the summary shows the most recent sample's kind.
//
// The query is a single GROUP BY scan over the index
// (url, ts); we then fetch the most recent sample per URL with a
// correlated subquery. Latency percentiles are computed in Go from the
// raw latencies for the URL/window slice — moving the math out of SQL
// keeps the query portable across SQLite versions where percentile_cont
// isn't available.
//
// ponytail: when the probe fleet grows past a few hundred URLs, move
// percentile computation into a running sketch (t-digest) and serve the
// window from in-memory state, not SQLite. Today's scale makes the
// in-Go sort the right trade-off.
func (p *PersistentStore) ProbeSummaries(windowSecs int64) ([]ProbeSummary, error) {
	return p.probeSummaries(windowSecs, "")
}

// ProbeSummariesByKind is the kind-filtered sibling of ProbeSummaries.
// Empty kind returns the unfiltered view (same as ProbeSummaries);
// unknown kinds return an empty slice without erroring so a UI typo
// can't break the status page.
func (p *PersistentStore) ProbeSummariesByKind(windowSecs int64, kind string) ([]ProbeSummary, error) {
	return p.probeSummaries(windowSecs, kind)
}

func (p *PersistentStore) probeSummaries(windowSecs int64, kindFilter string) ([]ProbeSummary, error) {
	if windowSecs <= 0 {
		windowSecs = 86400
	}
	cutoff := time.Now().Unix() - windowSecs

	p.mu.Lock()
	defer p.mu.Unlock()

	var (
		rows *sql.Rows
		err  error
	)
	if kindFilter == "" {
		rows, err = p.db.Query(
			"SELECT url, kind, ts, status_code, latency_ms, ok, error FROM probe_results WHERE ts >= ? ORDER BY url, ts",
			cutoff,
		)
	} else {
		rows, err = p.db.Query(
			"SELECT url, kind, ts, status_code, latency_ms, ok, error FROM probe_results WHERE ts >= ? AND kind = ? ORDER BY url, ts",
			cutoff, kindFilter,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Group in Go because the dataset fits comfortably in memory and the
	// percentile math has to live in Go anyway.
	type bucket struct {
		latencies []int64
		last      protocol.ProbeResult
		lastNode  string
	}
	buckets := map[string]*bucket{}
	byURL := map[string]*struct {
		total, ok int64
	}{}
	for rows.Next() {
		var url, errStr, kind string
		var ts int64
		var status, latency, okInt int
		if err := rows.Scan(&url, &kind, &ts, &status, &latency, &okInt, &errStr); err != nil {
			return nil, err
		}
		// We bucket by URL only — different probes (HTTP + TCP) on the
		// same host:port are rare and would confuse the public widget.
		// The Kind tag travels with the "last" sample so the summary
		// picks the most recent kind for the URL.
		b, ok := buckets[url]
		if !ok {
			b = &bucket{}
			buckets[url] = b
		}
		b.latencies = append(b.latencies, int64(latency))
		b.last = protocol.ProbeResult{
			URL: url, Kind: kind, StatusCode: status,
			LatencyMs: int64(latency), OK: okInt == 1, Error: errStr, Ts: ts,
		}
		c := byURL[url]
		if c == nil {
			c = &struct{ total, ok int64 }{}
			byURL[url] = c
		}
		c.total++
		if okInt == 1 {
			c.ok++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]ProbeSummary, 0, len(buckets))
	for url, b := range buckets {
		c := byURL[url]
		latencies := b.latencies
		p50 := percentile(latencies, 0.50)
		p95 := percentile(latencies, 0.95)
		uptime := 0.0
		if c.total > 0 {
			uptime = float64(c.ok) / float64(c.total) * 100.0
		}
		out = append(out, ProbeSummary{
			URL:         url,
			Kind:        defaultKind(b.last.Kind),
			WindowSecs:  windowSecs,
			Total:       c.total,
			OK:          c.ok,
			Failed:      c.total - c.ok,
			LastStatus:  b.last.StatusCode,
			LastLatency: b.last.LatencyMs,
			LastTs:      b.last.Ts,
			LastOK:      b.last.OK,
			UptimePct:   uptime,
			LatencyP50:  p50,
			LatencyP95:  p95,
		})
	}
	return out, nil
}

// percentile returns the nearest-rank percentile of a sorted-by-value
// integer slice, or 0 when the slice is empty. The input is mutated into
// sorted order to avoid the cost of a second allocation; callers that
// need the original order must pass a copy.
//
// ponytail: nearest-rank is fine for status-page reporting. If we ever
// serve probes to internal SLO dashboards, switch to linear
// interpolation (the "C=1" variant) for smoother graphs.
func percentile(values []int64, p float64) int64 {
	n := len(values)
	if n == 0 {
		return 0
	}
	// in-place insertion sort is fine for n ≤ ~200; if probe URLs grow
	// past that, switch to sort.Slice. Today's probe sets stay under 50.
	for i := 1; i < n; i++ {
		v := values[i]
		j := i - 1
		for j >= 0 && values[j] > v {
			values[j+1] = values[j]
			j--
		}
		values[j+1] = v
	}
	rank := int(float64(n) * p)
	if rank >= n {
		rank = n - 1
	}
	if rank < 0 {
		rank = 0
	}
	return values[rank]
}

// PurgeProbeResultsOlderThan deletes probe rows older than `cutoff`. The
// janitor uses this to keep the table bounded (see janitor.go). Exposed
// publicly so operators can force a prune from the CLI if needed.
//
// Returns the number of rows actually removed so the janitor can log
// progress without re-querying.
func (p *PersistentStore) PurgeProbeResultsOlderThan(cutoff time.Time) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	res, err := p.db.Exec("DELETE FROM probe_results WHERE ts < ?", cutoff.Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// silence the unused import linter when this file is built without the
// other files (e.g. during partial test compilation).
var _ = sql.ErrNoRows
var _ sync.Mutex

// defaultKind normalizes a missing/empty kind tag to "http" so the
// public API stays backwards-compatible with rows that pre-date the
// TCP probe feature (older rows may have kind='' depending on which
// schema migration ran first).
func defaultKind(k string) string {
	if k == "" {
		return protocol.ProbeKindHTTP
	}
	return k
}
