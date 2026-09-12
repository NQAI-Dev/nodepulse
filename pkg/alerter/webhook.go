package alerter

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

type WebhookDispatcher struct {
	client *http.Client
}

func NewWebhook() *WebhookDispatcher {
	return &WebhookDispatcher{
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

func NewWebhookDispatcher() *WebhookDispatcher {
	return NewWebhook()
}

func (w *WebhookDispatcher) Dispatch(url string, event protocol.WebhookAlert) error {
	if url == "" {
		return nil
	}
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	resp, err := w.client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}
