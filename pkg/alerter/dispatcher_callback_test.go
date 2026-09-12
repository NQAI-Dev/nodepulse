package alerter

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

// roundTripFunc implements http.RoundTripper for injecting fake responses.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// capturedRequest collects one Telegram API call: which endpoint was hit
// and what JSON body went on the wire.
//
// ponytail: pointer because the round-trip closure captures the variable by
// reference at construction time; a by-value copy would mutate a local
// that the test cannot read back.
type capturedRequest struct {
	URL  string
	Body [][]byte
}

func newTestDispatcher(token, secret string, cb *capturedRequest) *Dispatcher {
	d := New(token, 1)
	d.callbackSecret = secret
	d.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		cb.URL = r.URL.String()
		cb.Body = append(cb.Body, body)
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(bytes.NewReader([]byte(`{"ok":true}`))),
		}, nil
	})}
	return d
}

func TestDispatcher_NotifyWithButtonsHitsSendMessage(t *testing.T) {
	var cb capturedRequest
	d := newTestDispatcher("bot", "s3cret", &cb)

	d.NotifyIncidentWithButtonsTo(42, "77", "node-x", "critical", "Docker Down", "5 containers stopped")

	if !strings.HasSuffix(cb.URL, "/sendMessage") {
		t.Fatalf("expected sendMessage, got URL=%s", cb.URL)
	}
	if len(cb.Body) != 1 {
		t.Fatalf("expected exactly 1 POST, got %d", len(cb.Body))
	}
	body := string(cb.Body[0])
	if !strings.Contains(body, `"chat_id":42`) {
		t.Fatalf("missing chat_id in payload: %s", body)
	}
	if !strings.Contains(body, `"reply_markup"`) {
		t.Fatalf("missing reply_markup in payload: %s", body)
	}
	if !strings.Contains(body, `"callback_data":"np:ack:77.`) {
		t.Fatalf("missing signed ack callback_data: %s", body)
	}
	if !strings.Contains(body, `"callback_data":"np:resolve:77.`) {
		t.Fatalf("missing signed resolve callback_data: %s", body)
	}
}

func TestDispatcher_AnswerCallbackHitsEndpoint(t *testing.T) {
	var cb capturedRequest
	d := newTestDispatcher("bot", "", &cb)
	d.AnswerCallback("cbq-1", "Acknowledged")

	if !strings.HasSuffix(cb.URL, "/answerCallbackQuery") {
		t.Fatalf("expected answerCallbackQuery, got %s", cb.URL)
	}
	if !strings.Contains(string(cb.Body[0]), `"callback_query_id":"cbq-1"`) {
		t.Fatalf("missing callback_query_id: %s", string(cb.Body[0]))
	}
	if !strings.Contains(string(cb.Body[0]), `"text":"Acknowledged"`) {
		t.Fatalf("missing text: %s", string(cb.Body[0]))
	}
}

func TestDispatcher_NotifyWithoutIncidentIDFallsBack(t *testing.T) {
	var cb capturedRequest
	d := newTestDispatcher("bot", "s3cret", &cb)

	d.NotifyIncidentWithButtonsTo(7, "", "node-y", "warning", "Lag", "high") // empty ID path

	if strings.Contains(string(cb.Body[0]), `"reply_markup"`) {
		t.Fatalf("empty incidentID must NOT include reply_markup: %s", string(cb.Body[0]))
	}
}

func TestDispatcher_DispatchRespectsEmptyToken(t *testing.T) {
	// No token ⇒ no HTTP calls, no panic.
	d := New("", 1)
	d.NotifyIncidentWithButtonsTo(1, "1", "x", "warning", "t", "d") // must not panic
}
