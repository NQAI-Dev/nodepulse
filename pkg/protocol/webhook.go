package protocol

type WebhookAlert struct {
	Event     string    `json:"event"`
	Incident  Incident  `json:"incident"`
	Timestamp int64     `json:"timestamp"`
}
