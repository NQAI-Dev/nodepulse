package protocol

type WebhookAlert struct {
	Event     string    `json:"event"`
	Timestamp int64     `json:"timestamp"`
	Incident  *Incident `json:"incident,omitempty"`
}

// WebhookDelivery captures one HTTP attempt (or the rolled-up outcome of a
// burst of retries) against a user's configured webhook URL. The operator UI
// surfaces this verbatim so they can tell at a glance whether their endpoint
// is up, slow, or rejecting signed payloads — without having to grep server
// logs. Record shape is stable across server versions; new fields go at the
// end and must remain omitempty-safe.
type WebhookDelivery struct {
	ID            int64  `json:"id"`
	UserID        int64  `json:"user_id"`
	IncidentID    string `json:"incident_id,omitempty"`
	Event         string `json:"event"`
	URL           string `json:"url"`
	Attempts      int    `json:"attempts"`
	Status        int    `json:"status"`        // 0 = network error
	OK            bool   `json:"ok"`
	Error         string `json:"error,omitempty"`
	LatencyMs     int64  `json:"latency_ms"`    // wall time spent on the successful (or final) attempt
	TotalLatencyMs int64 `json:"total_latency_ms"` // dispatch() start→finish, includes sleeps
	Timestamp     int64  `json:"timestamp"`
}
