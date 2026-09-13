package protocol

type User struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	CreatedAt int64  `json:"created_at"`
}

type AuthRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type AuthResponse struct {
	Token string `json:"token"`
	User  User   `json:"user"`
}

// UserSettings is the per-user alerting config: which channels should
// receive which kinds of incidents, plus credentials for the chat-style
// targets. NotifyCritical / NotifyWarning toggle the *severity* filter —
// every channel still obeys the gate, so disabling NotifyWarning silences
// warning-class alerts across Telegram, Slack, Discord and the generic
// webhook in one go.
//
// SlackWebhookURL / DiscordWebhookURL are optional convenience channels
// pointed at the team's chat workspace. Their payload format is chat-native
// (Slack block kit / Discord embeds) so the alert renders correctly
// without the operator standing up an adapter service. Both are signed
// using the channel's own convention (Slack x-slack-signing-secret header
// is unsupported here; Discord has no built-in signing) so operators
// should treat the URLs as secrets and rotate via the settings endpoint.
type UserSettings struct {
	TelegramChatID    string `json:"telegram_chat_id"`
	WebhookURL        string `json:"webhook_url"`
	WebhookSecret     string `json:"webhook_secret,omitempty"`
	SlackWebhookURL   string `json:"slack_webhook_url,omitempty"`
	DiscordWebhookURL string `json:"discord_webhook_url,omitempty"`
	NotifyCritical    bool   `json:"notify_critical"`
	NotifyWarning     bool   `json:"notify_warning"`
}

// MaintenanceWindow describes a planned silence period during which alerts
// for one or more nodes are suppressed. EndUnix == 0 means open-ended
// (operator must close it manually); NodeIDs empty means it applies to the
// owning user's entire fleet. Scope "user" targets the owner's fleet,
// "node" targets only the listed NodeIDs.
//
// ponytail: if scheduling gets sophisticated (RRULE, blackout calendars)
// swap this for a proper cron-style representation; for v1 a single
// StartUnix → EndUnix pair covers the 90% case (deploy / db migration).
type MaintenanceWindow struct {
	ID         int64    `json:"id"`
	UserID     int64    `json:"user_id"`
	NodeIDs    []string `json:"node_ids"`
	Reason     string   `json:"reason"`
	StartUnix  int64    `json:"start_unix"`
	EndUnix    int64    `json:"end_unix"` // 0 = open-ended
	Scope      string   `json:"scope"`   // "user" | "node"
	CreatedAt  int64    `json:"created_at"`
	CreatedBy  string   `json:"created_by,omitempty"`
}

// MaintenanceWindowRequest is the JSON body for POST /api/v1/maintenance.
// EndUnix==0 leaves the window open until DELETE.
type MaintenanceWindowRequest struct {
	NodeIDs   []string `json:"node_ids"`
	Reason    string   `json:"reason"`
	StartUnix int64    `json:"start_unix"`
	EndUnix   int64    `json:"end_unix"`
	Scope     string   `json:"scope"`
}
