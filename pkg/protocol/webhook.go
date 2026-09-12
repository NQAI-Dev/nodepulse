package protocol

type WebhookAlert struct {
	Event     string    `json:"event"`
	Timestamp int64     `json:"timestamp"`
	Incident  *Incident `json:"incident,omitempty"`
}
