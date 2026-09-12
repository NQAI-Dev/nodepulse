package alerter

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
)

// Telegram inline-keyboard callback data conventions.
//
//   np:ack:<incident_id>[.<hex-hmac>]     → operator acknowledges the alert
//   np:resolve:<incident_id>[.<hex-hmac>] → operator marks the incident resolved
//
// Prefix `np:` keeps the namespace ours and stops us from colliding with any
// other bot the chat might be hosting. The optional `.hex-hmac` suffix is an
// HMAC of `<prefix><id>` under the bot token; the server-side callback
// endpoint verifies it before acting on the request.
//
// ponytail: callback data is capped at 64 bytes by Telegram. With the prefix
// and a base-10 int64 incident ID we stay well under that, but if incident
// IDs ever become UUIDs/ULIDs switch to a short token table.
const (
	ActionAckPrefix     = "np:ack:"
	ActionResolvePrefix = "np:resolve:"
)

// InlineKeyboardButton mirrors the Telegram Bot API shape we need.
type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// SignCallbackData returns callback_data of the form
// "<prefix><id>.<hex-hmac>" so the server-side callback endpoint can verify
// the request originated from this dispatcher. An empty secret returns the
// unsigned "<prefix><id>" form.
func SignCallbackData(secret, prefix, incidentID string) string {
	body := prefix + incidentID
	if secret == "" {
		return body
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return body + "." + hex.EncodeToString(mac.Sum(nil))
}

// VerifyCallbackData checks that the secret-bound HMAC matches. Returns the
// parsed action ("ack" or "resolve") and the incident ID, or false.
//
// Accepts both the signed form ("np:ack:7.<hex>") and the unsigned form
// ("np:ack:7"); the unsigned form is intended for local development and
// integration tests where the bot token is unavailable.
func VerifyCallbackData(secret, data string) (action string, incidentID string, ok bool) {
	body, sig, hasSig := splitSig(data)
	if body == "" {
		return "", "", false
	}

	action = parseActionBody(body)
	if action == "" {
		return "", "", false
	}

	idx := strings.LastIndex(body, ":")
	if idx < 0 || idx == len(body)-1 {
		return "", "", false
	}
	incidentID = body[idx+1:]

	if !hasSig {
		// Unsigned path: only accept when the caller also passed no secret.
		return action, incidentID, secret == ""
	}
	if secret == "" {
		return "", "", false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	expected := hex.EncodeToString(mac.Sum(nil))
	if hmac.Equal([]byte(sig), []byte(expected)) {
		return action, incidentID, true
	}
	return "", "", false
}

// splitSig separates "np:ack:7" from "abcdef…" if present. The dot separator
// is not allowed inside the action body.
func splitSig(data string) (body, sig string, hasSig bool) {
	idx := strings.LastIndex(data, ".")
	if idx < 0 {
		return data, "", false
	}
	return data[:idx], data[idx+1:], true
}

func parseActionBody(body string) string {
	switch {
	case strings.HasPrefix(body, ActionAckPrefix):
		return "ack"
	case strings.HasPrefix(body, ActionResolvePrefix):
		return "resolve"
	default:
		return ""
	}
}

// BuildIncidentButtons returns the two-button inline keyboard for an incident.
// Pass an empty secret to omit HMAC signing (useful for tests).
func BuildIncidentButtons(incidentID, secret string) [][]InlineKeyboardButton {
	return [][]InlineKeyboardButton{
		{
			{Text: "✅ Acknowledge", CallbackData: SignCallbackData(secret, ActionAckPrefix, incidentID)},
			{Text: "🛠 Resolve", CallbackData: SignCallbackData(secret, ActionResolvePrefix, incidentID)},
		},
	}
}

// EncodeReplyMarkup marshals an inline-keyboard reply_markup for the
// sendMessage payload.
func EncodeReplyMarkup(rows [][]InlineKeyboardButton) ([]byte, error) {
	return json.Marshal(map[string]interface{}{
		"inline_keyboard": rows,
	})
}

// parseIncidentID is a tiny helper kept here so callers don't import strconv
// just for the inline-keyboard code path.
func parseIncidentID(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
