package alerter

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// WebhookDispatcher sends signed incident events to user-configured URLs with
// exponential-backoff retries on transient failures.
type WebhookDispatcher struct {
	client     *http.Client
	maxRetries int
}

// NewWebhook returns a dispatcher with 4 attempts total (initial + 3 retries).
func NewWebhook() *WebhookDispatcher {
	return &WebhookDispatcher{
		client:     &http.Client{Timeout: 5 * time.Second},
		maxRetries: 3,
	}
}

func NewWebhookDispatcher() *WebhookDispatcher {
	return NewWebhook()
}

// Dispatch attempts to deliver the webhook. The `secret`, if non-empty, is used
// to compute an HMAC-SHA256 signature sent in the X-NodePulse-Signature header.
//
// ponytail: full success/audit trail lives in the incident log; this function
// only logs final failures to stderr. Add a delivery audit table if operators
// need a UI for it.
func (w *WebhookDispatcher) Dispatch(url string, event protocol.WebhookAlert) error {
	return w.DispatchSigned(url, "", event)
}

func (w *WebhookDispatcher) DispatchSigned(url, secret string, event protocol.WebhookAlert) error {
	if url == "" {
		return nil
	}
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}

	signature := ""
	if secret != "" {
		signature = signBody(secret, body)
	}

	var lastErr error
	for attempt := 0; attempt <= w.maxRetries; attempt++ {
		err := w.deliverOnce(url, body, signature)
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt == w.maxRetries {
			break
		}
		// Exponential backoff with cap: 250ms, 500ms, 1s, 2s, ...
		delay := time.Duration(math.Pow(2, float64(attempt))) * 250 * time.Millisecond
		if delay > 5*time.Second {
			delay = 5 * time.Second
		}
		time.Sleep(delay)
	}
	return lastErr
}

func (w *WebhookDispatcher) deliverOnce(url string, body []byte, signature string) error {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "NodePulse-Webhook/1.0")
	if signature != "" {
		req.Header.Set("X-NodePulse-Signature", signature)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused.
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return &webhookStatusError{
		status: resp.StatusCode,
		retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
	}
}

func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

type webhookStatusError struct {
	status    int
	retryAfter time.Duration
}

func (e *webhookStatusError) Error() string {
	return "webhook returned status " + strconv.Itoa(e.status)
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}
