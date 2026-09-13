package store

import (
	"database/sql"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func (p *PersistentStore) GetSettings(userID int64) (*protocol.UserSettings, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.getSettingsLocked(userID)
}

func (p *PersistentStore) getSettingsLocked(userID int64) (*protocol.UserSettings, error) {
	var s protocol.UserSettings
	var crit, warn int
	err := p.db.QueryRow(`
		SELECT telegram_chat_id, webhook_url, webhook_secret,
		       COALESCE(slack_webhook_url, ''), COALESCE(discord_webhook_url, ''),
		       notify_critical, notify_warning
		FROM user_settings WHERE user_id = ?`, userID).
		Scan(&s.TelegramChatID, &s.WebhookURL, &s.WebhookSecret,
			&s.SlackWebhookURL, &s.DiscordWebhookURL, &crit, &warn)
	if err == sql.ErrNoRows {
		return &protocol.UserSettings{
			NotifyCritical: true,
			NotifyWarning:  true,
		}, nil
	} else if err != nil {
		return nil, err
	}

	s.NotifyCritical = crit == 1
	s.NotifyWarning = warn == 1
	return &s, nil
}

// UpdateSettings persists the user's alert routing. Slack and Discord
// channels are accepted as raw incoming-webhook URLs and stored verbatim;
// the dispatch layer enforces format validity (https://, sensible host)
// at send time so a misconfigured URL surfaces as a per-event delivery
// error rather than crashing the request path.
func (p *PersistentStore) UpdateSettings(userID int64, tgChatID, webhookURL, webhookSecret, slackURL, discordURL string, crit, warn bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	critInt := 0
	if crit {
		critInt = 1
	}
	warnInt := 0
	if warn {
		warnInt = 1
	}

	_, err := p.db.Exec(`
		INSERT INTO user_settings (user_id, telegram_chat_id, webhook_url, webhook_secret,
		                           slack_webhook_url, discord_webhook_url,
		                           notify_critical, notify_warning)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			telegram_chat_id = excluded.telegram_chat_id,
			webhook_url = excluded.webhook_url,
			webhook_secret = excluded.webhook_secret,
			slack_webhook_url = excluded.slack_webhook_url,
			discord_webhook_url = excluded.discord_webhook_url,
			notify_critical = excluded.notify_critical,
			notify_warning = excluded.notify_warning`,
		userID, tgChatID, webhookURL, webhookSecret, slackURL, discordURL, critInt, warnInt)
	return err
}

func (p *PersistentStore) GetNodeOwner(nodeID string) (int64, error) {
	var uid int64
	err := p.db.QueryRow("SELECT user_id FROM node_owners WHERE node_id = ?", nodeID).Scan(&uid)
	if err != nil {
		return 1, nil // default to admin
	}
	return uid, nil
}
