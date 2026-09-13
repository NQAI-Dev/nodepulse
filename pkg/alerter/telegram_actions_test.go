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
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows (ack/resolve + snooze shortcuts), got %d", len(rows))
	}
	if len(rows[0]) != 2 {
		t.Fatalf("expected 2 buttons in row 0, got %d", len(rows[0]))
	}
	if !strings.HasPrefix(rows[0][0].CallbackData, ActionAckPrefix+"7") {
		t.Fatalf("ack data missing prefix: %q", rows[0][0].CallbackData)
	}
	if !strings.HasPrefix(rows[0][1].CallbackData, ActionResolvePrefix+"7") {
		t.Fatalf("resolve data missing prefix: %q", rows[0][1].CallbackData)
	}
	if len(rows[1]) != 3 {
		t.Fatalf("expected 3 snooze buttons in row 1, got %d", len(rows[1]))
	}
	for _, b := range rows[1] {
		if !strings.HasPrefix(b.CallbackData, ActionSnoozePrefix+"7:") {
			t.Fatalf("snooze button missing prefix: %q", b.CallbackData)
		}
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

func TestSignAndVerifySnoozeCallback(t *testing.T) {
	const secret = "bot-token-xyz"
	id := "42"

	for _, dur := range AllowedSnoozeKeys() {
		signed := SignSnoozeCallback(secret, id, dur)
		gotID, gotDur, ok := VerifySnoozeCallback(secret, signed)
		if !ok || gotID != id || gotDur != dur {
			t.Fatalf("snooze verify %s: id=%q dur=%q ok=%v", dur, gotID, gotDur, ok)
		}
	}

	// Wrong secret rejects.
	if _, _, ok := VerifySnoozeCallback("wrong", SignSnoozeCallback(secret, id, "1h")); ok {
		t.Fatalf("verify accepted wrong secret")
	}

	// Disallowed duration (24h, even if it parses as a snooze duration) is
	// rejected at the parse layer; the dispatcher only ever produces the
	// three canonical keys, but the verifier must not trust caller input.
	if _, _, ok := VerifySnoozeCallback(secret, "np:snooze:1:24h.xxx"); ok {
		t.Fatalf("verify accepted non-canonical duration")
	}

	// Unsigned form (no secret configured).
	unsigned := SignSnoozeCallback("", id, "4h")
	gotID, gotDur, ok := VerifySnoozeCallback("", unsigned)
	if !ok || gotID != id || gotDur != "4h" {
		t.Fatalf("unsigned snooze verify failed: id=%q dur=%q ok=%v", gotID, gotDur, ok)
	}
}

func TestSnoozeSeconds(t *testing.T) {
	if SnoozeSeconds("1h") != 3600 {
		t.Fatalf("1h != 3600")
	}
	if SnoozeSeconds("4h") != 4*3600 {
		t.Fatalf("4h wrong")
	}
	if SnoozeSeconds("8h") != 8*3600 {
		t.Fatalf("8h wrong")
	}
	if SnoozeSeconds("12h") != 0 {
		t.Fatalf("unknown key must return 0, got %d", SnoozeSeconds("12h"))
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
