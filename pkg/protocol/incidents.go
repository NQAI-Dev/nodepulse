package protocol

// PublicIncidentHistogram is the /api/v1/public/incidents/histogram payload.
// Buckets cover the last `days` calendar days (UTC), newest first. Each row
// carries open + resolved counts grouped by severity so a status widget can
// render a single stacked-bar sparkline without re-querying.
//
// ponytail: if days > 90 the payload grows beyond the widget's needs. Hard
// cap at 90 unless a future caller explicitly asks for longer ranges.
type PublicIncidentHistogram struct {
	Days      int                         `json:"days"`
	UpdatedAt int64                       `json:"updated_at"`
	Buckets   []PublicIncidentHistogramDay `json:"buckets"`
}

type PublicIncidentHistogramDay struct {
	Day            string             `json:"day"`             // YYYY-MM-DD (UTC)
	OpenBySeverity map[string]int      `json:"open_by_severity"` // severity → count of still-open
	TotalBySeverity map[string]int     `json:"total_by_severity"` // severity → count of all started that day
	Resolved       int                `json:"resolved"`         // total resolved that day
}
