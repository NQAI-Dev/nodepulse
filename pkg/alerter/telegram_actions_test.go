package alerter

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestSignAndVerifyCallbackData(t *testing.T) {
	const secret = "bot-token-xyz"
	id := "42"

	ack := SignCallbackData(secret, ActionAckPrefix, id)
	res := SignCallbackData(secret, ActionResolvePrefix, id)

	if ack == res {
		t.Fatalf("ack and resolve must not collide: %s", ack)
	}

	action, incidentID, ok := VerifyCallbackData(secret, ack)
	if !ok || action != "ack" || incidentID != id {
		t.Fatalf("verify ack: action=%q id=%q ok=%v", action, incidentID, ok)
	}

	action, incidentID, ok = VerifyCallbackData(secret, res)
	if !ok || action != "resolve" || incidentID != id {
		t.Fatalf("verify resolve: action=%q id=%q ok=%v", action, incidentID, ok)
	}

	// Wrong secret must reject.
	if _, _, ok := VerifyCallbackData("wrong", ack); ok {
		t.Fatalf("verify accepted wrong secret")
	}
}

func TestBuildIncidentButtons(t *testing.T) {
	rows := BuildIncidentButtons("7", "")
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if len(rows[0]) != 2 {
		t.Fatalf("expected 2 buttons, got %d", len(rows[0]))
	}
	if !strings.HasPrefix(rows[0][0].CallbackData, ActionAckPrefix+"7") {
		t.Fatalf("ack data missing prefix: %q", rows[0][0].CallbackData)
	}
	if !strings.HasPrefix(rows[0][1].CallbackData, ActionResolvePrefix+"7") {
		t.Fatalf("resolve data missing prefix: %q", rows[0][1].CallbackData)
	}

	signed := BuildIncidentButtons("9", "secret")
	if !strings.Contains(signed[0][0].CallbackData, ".") {
		t.Fatalf("expected HMAC suffix on signed data: %q", signed[0][0].CallbackData)
	}
	// Round-trip the signed form.
	action, id, ok := VerifyCallbackData("secret", signed[0][0].CallbackData)
	if !ok || action != "ack" || id != "9" {
		t.Fatalf("signed verify failed: %s %s %v", action, id, ok)
	}
}

func TestSignCallbackData_Format(t *testing.T) {
	const secret = "s"
	const prefix = ActionAckPrefix
	const id = "123"
	got := SignCallbackData(secret, prefix, id)

	body := prefix + id
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	want := body + "." + hex.EncodeToString(mac.Sum(nil))
	if got != want {
		t.Fatalf("format mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestVerifyCallbackData_Malformed(t *testing.T) {
	cases := []string{
		"",
		"np",
		"np:",
		"np:ack",
		"np:ack:",
		"np:other:1",
		"np:ack:notanumber.signature",
	}
	for _, c := range cases {
		if _, _, ok := VerifyCallbackData("s", c); ok {
			t.Fatalf("expected reject for %q", c)
		}
	}
}

func TestEncodeReplyMarkup(t *testing.T) {
	rows := BuildIncidentButtons("1", "")
	b, err := EncodeReplyMarkup(rows)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"inline_keyboard"`) {
		t.Fatalf("missing inline_keyboard key: %s", s)
	}
	if !strings.Contains(s, `"callback_data":"np:ack:1"`) {
		t.Fatalf("missing ack callback_data: %s", s)
	}
}
