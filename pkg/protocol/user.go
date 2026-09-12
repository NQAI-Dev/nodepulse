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

type UserSettings struct {
	TelegramChatID string `json:"telegram_chat_id"`
	WebhookURL     string `json:"webhook_url"`
	NotifyCritical bool   `json:"notify_critical"`
	NotifyWarning  bool   `json:"notify_warning"`
}
