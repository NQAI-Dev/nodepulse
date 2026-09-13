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
	botToken   string
	chatID     int64
	callbackSecret string // HMAC secret for inline-keyboard callback_data; may be empty (no signing)
	client     *http.Client
}

func New(botToken string, chatID int64) *Dispatcher {
	return &Dispatcher{
		botToken:   botToken,
		chatID:     chatID,
		client:     &http.Client{Timeout: 5 * time.Second},
	}
}

// SetCallbackSecret enables HMAC-signed inline-keyboard callback_data. Pass
// the bot token (or any other shared secret between dispatcher and the
// /telegram/callback endpoint) so verifyCallbackData on the server can
// distinguish operator clicks from forged requests.
func (d *Dispatcher) SetCallbackSecret(s string) {
	d.callbackSecret = s
}

func (d *Dispatcher) CallbackSecret() string {
	return d.callbackSecret
}

func (d *Dispatcher) NotifyIncident(nodeID, severity, title, detail string) {
	d.NotifyIncidentTo(d.chatID, nodeID, severity, title, detail)
}

func (d *Dispatcher) NotifyIncidentTo(chatID int64, nodeID, severity, title, detail string) {
	if d.botToken == "" || chatID == 0 {
		return
	}
	d.sendIncident(chatID, "", nodeID, severity, title, detail)
}

// NotifyIncidentWithButtonsTo sends the same alert but attaches the
// Acknowledge / Resolve inline keyboard. incidentID is the database row
// id (base-10 string) and is required so the callback endpoint can route
// the operator's click back to the right incident.
func (d *Dispatcher) NotifyIncidentWithButtonsTo(chatID int64, incidentID, nodeID, severity, title, detail string) {
	if d.botToken == "" || chatID == 0 || incidentID == "" {
		d.NotifyIncidentTo(chatID, nodeID, severity, title, detail)
		return
	}
	d.sendIncident(chatID, incidentID, nodeID, severity, title, detail)
}

func (d *Dispatcher) sendIncident(chatID int64, incidentID, nodeID, severity, title, detail string) {
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

	body := map[string]interface{}{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "HTML",
	}
	if incidentID != "" {
		rows := BuildIncidentButtons(incidentID, d.callbackSecret)
		markup, err := EncodeReplyMarkup(rows)
		if err == nil {
			var raw interface{}
			if jsonErr := json.Unmarshal(markup, &raw); jsonErr == nil {
				body["reply_markup"] = raw
			}
		}
	}

	payload, _ := json.Marshal(body)
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", d.botToken)
	resp, err := d.client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Printf("[alerter] Telegram notification error: %v", err)
		return
	}
	defer resp.Body.Close()
}

// AnswerCallback answers a callback_query so Telegram stops its "loading"
// spinner. text is shown as a toast; if empty, a tiny default is used.
func (d *Dispatcher) AnswerCallback(callbackQueryID, text string) {
	if d.botToken == "" || callbackQueryID == "" {
		return
	}
	if text == "" {
		text = "OK"
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"callback_query_id": callbackQueryID,
		"text":              text,
		"show_alert":        false,
	})
	url := fmt.Sprintf("https://api.telegram.org/bot%s/answerCallbackQuery", d.botToken)
	resp, err := d.client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Printf("[alerter] Telegram answerCallbackQuery error: %v", err)
		return
	}
	resp.Body.Close()
}

func (d *Dispatcher) GetChatID() int64 {
	return d.chatID
}

// SendTestAlert sends a clearly-marked test message to chatID so operators
// can verify their Telegram wiring without waiting for a real incident.
// Returns nil when api.telegram.org responds 2xx, or the underlying error
// otherwise. Caller decides how to surface the result to the UI.
func (d *Dispatcher) SendTestAlert(chatID int64) error {
	if d.botToken == "" {
		return fmt.Errorf("telegram bot token not configured")
	}
	if chatID == 0 {
		return fmt.Errorf("telegram chat id is empty")
	}
	text := fmt.Sprintf("🧪 <b>[NodePulse Test Alert]</b>\n\n"+
		"Это тестовое уведомление — настройки Telegram работают корректно.\n"+
		"<b>Time:</b> %s",
		time.Now().Format("2006-01-02 15:04:05 UTC"))
	payload, _ := json.Marshal(map[string]interface{}{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "HTML",
	})
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", d.botToken)
	resp, err := d.client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("telegram sendMessage: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telegram sendMessage: status %d", resp.StatusCode)
	}
	return nil
}

// BotToken exposes the configured bot token (read-only).
func (d *Dispatcher) BotToken() string { return d.botToken }

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
