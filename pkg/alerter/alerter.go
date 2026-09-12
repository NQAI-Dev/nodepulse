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
	d.NotifyIncidentTo(d.chatID, nodeID, severity, title, detail)
}

func (d *Dispatcher) NotifyIncidentTo(chatID int64, nodeID, severity, title, detail string) {
	if d.botToken == "" || chatID == 0 {
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
		"chat_id":    chatID,
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

func (d *Dispatcher) GetChatID() int64 {
	return d.chatID
}

// NotifyResolvedTo sends a recovery message for an auto-resolved incident.
func (d *Dispatcher) NotifyResolvedTo(chatID int64, nodeID, severity, title string) {
	if d.botToken == "" || chatID == 0 {
		return
	}
	text := fmt.Sprintf("✅ <b>[NodePulse Incident Resolved]</b>\n\n"+
		"<b>Node:</b> <code>%s</code>\n"+
		"<b>Severity:</b> %s\n"+
		"<b>Recovered:</b> %s\n"+
		"<b>Time:</b> %s",
		nodeID, severity, title, time.Now().Format("2006-01-02 15:04:05 UTC"))
	payload, _ := json.Marshal(map[string]interface{}{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "HTML",
	})
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", d.botToken)
	resp, err := d.client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Printf("[alerter] Telegram resolution notification error: %v", err)
		return
	}
	resp.Body.Close()
}
