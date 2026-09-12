package alerter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Dispatcher struct {
	client   *http.Client
	botToken string
	chatID   int64
}

func New(botToken string, chatID int64) *Dispatcher {
	return &Dispatcher{
		client:   &http.Client{Timeout: 5 * time.Second},
		botToken: botToken,
		chatID:   chatID,
	}
}

func (d *Dispatcher) NotifyIncident(nodeID, severity, title, detail string) error {
	if d.botToken == "" || d.chatID == 0 {
		return nil
	}

	icon := "⚠️"
	if severity == "critical" {
		icon = "🚨"
	}

	msg := fmt.Sprintf("%s *[NodePulse Alert]*\n\n*Нода:* `%s`\n*Уровень:* `%s`\n*Событие:* *%s*\n*Детали:* _%s_\n*Время:* `%s`",
		icon, nodeID, severity, title, detail, time.Now().UTC().Format("15:04:05 02.01.2006"))

	payload, _ := json.Marshal(map[string]interface{}{
		"chat_id":    d.chatID,
		"text":       msg,
		"parse_mode": "Markdown",
	})

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", d.botToken)
	resp, err := d.client.Post(url, "application/json", bytes.NewBuffer(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}
