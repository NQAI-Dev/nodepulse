package alerter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

type Dispatcher struct {
	botToken string
	chatID   int64
	client   *http.Client
}

func New(botToken string, chatID int64) *Dispatcher {
	return &Dispatcher{
		botToken: botToken,
		chatID:   chatID,
		client:   &http.Client{Timeout: 5 * time.Second},
	}
}

func (d *Dispatcher) NotifyIncident(nodeID, severity, title, detail string) {
	if d.botToken == "" || d.chatID == 0 {
		return
	}

	icon := "⚠️"
	if severity == "critical" {
		icon = "🚨"
	}

	text := fmt.Sprintf("%s <b>[NodePulse Incident Alert]</b>\n\n"+
		"<b>Node:</b> <code>%s</code>\n"+
		"<b>Severity:</b> %s\n"+
		"<b>Issue:</b> %s\n"+
		"<b>Detail:</b> <i>%s</i>\n"+
		"<b>Time:</b> %s",
		icon, nodeID, severity, title, detail, time.Now().Format("2006-01-02 15:04:05 UTC"))

	payload, _ := json.Marshal(map[string]interface{}{
		"chat_id":    d.chatID,
		"text":       text,
		"parse_mode": "HTML",
	})

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", d.botToken)
	resp, err := d.client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Printf("[alerter] Telegram notification error: %v", err)
		return
	}
	defer resp.Body.Close()
}
