package store

import (
	"database/sql"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func (p *PersistentStore) GetSettings(userID int64) (*protocol.UserSettings, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var s protocol.UserSettings
	var crit, warn int
	err := p.db.QueryRow(`
		SELECT telegram_chat_id, webhook_url, notify_critical, notify_warning
		FROM user_settings WHERE user_id = ?`, userID).Scan(&s.TelegramChatID, &s.WebhookURL, &crit, &warn)
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

func (p *PersistentStore) UpdateSettings(userID int64, tgChatID, webhookURL string, crit, warn bool) error {
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
		INSERT INTO user_settings (user_id, telegram_chat_id, webhook_url, notify_critical, notify_warning)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			telegram_chat_id = excluded.telegram_chat_id,
			webhook_url = excluded.webhook_url,
			notify_critical = excluded.notify_critical,
			notify_warning = excluded.notify_warning`,
		userID, tgChatID, webhookURL, critInt, warnInt)
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
