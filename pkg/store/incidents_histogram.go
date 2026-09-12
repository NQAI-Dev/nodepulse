package store

import (
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// PublicIncidentHistogram returns per-day incident counts for the last
// `days` calendar days (UTC). Used by /api/v1/public/incidents/histogram
// to power stacked-bar sparklines on the status page.
//
// Each day carries two maps:
//   - TotalBySeverity: every incident that *started* that day
//   - OpenBySeverity: incidents that started that day and are still open
//
// Days with zero activity are emitted as empty buckets so the widget can
// render a continuous timeline without sparse gaps.
//
// ponytail: at fleet sizes >10k incidents/day, drop the per-day rollup to
// pre-aggregated daily rows or stream via a generated series CTE.
func (p *PersistentStore) PublicIncidentHistogram(days int) protocol.PublicIncidentHistogram {
	if days <= 0 || days > 90 {
		days = 30
	}
	if p == nil || p.db == nil {
		return protocol.PublicIncidentHistogram{Days: days, UpdatedAt: time.Now().Unix()}
	}

	now := time.Now().UTC()
	cutoff := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).
		AddDate(0, 0, -days+1).Unix()

	rows, err := p.db.Query(
		`SELECT severity,
		        CAST((started_at - ?) / 86400 AS INTEGER) AS day_idx,
		        SUM(CASE WHEN resolved = 0 THEN 1 ELSE 0 END) AS open_cnt,
		        COUNT(*) AS total_cnt
		   FROM incidents
		  WHERE started_at >= ?
		  GROUP BY severity, day_idx
		  ORDER BY day_idx DESC`,
		cutoff, cutoff,
	)
	if err != nil {
		return protocol.PublicIncidentHistogram{Days: days, UpdatedAt: now.Unix()}
	}
	defer rows.Close()

	type bucketKey struct {
		DayIdx int
		Sev    string
	}
	totalByDay := map[int]map[string]int{}
	openByDay := map[int]map[string]int{}

	for rows.Next() {
		var sev string
		var dayIdx, openCnt, totalCnt int
		if err := rows.Scan(&sev, &dayIdx, &openCnt, &totalCnt); err != nil {
			continue
		}
		if _, ok := totalByDay[dayIdx]; !ok {
			totalByDay[dayIdx] = map[string]int{}
			openByDay[dayIdx] = map[string]int{}
		}
		totalByDay[dayIdx][sev] = totalCnt
		openByDay[dayIdx][sev] = openCnt
	}

	buckets := make([]protocol.PublicIncidentHistogramDay, 0, days)
	for i := 0; i < days; i++ {
		ts := time.Unix(cutoff+int64(i)*86400, 0).UTC()
		dayKey := ts.Format("2006-01-02")
		b := protocol.PublicIncidentHistogramDay{
			Day:             dayKey,
			TotalBySeverity: map[string]int{},
			OpenBySeverity:  map[string]int{},
		}
		if m, ok := totalByDay[i]; ok {
			b.TotalBySeverity = m
		}
		if m, ok := openByDay[i]; ok {
			b.OpenBySeverity = m
		}
		buckets = append(buckets, b)
	}

	return protocol.PublicIncidentHistogram{
		Days:      days,
		UpdatedAt: now.Unix(),
		Buckets:   buckets,
	}
}
