package alerter

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

type WebhookDispatcher struct {
	endpoints []string
	client    *http.Client
}

func NewWebhookDispatcher(endpoints []string) *WebhookDispatcher {
	return &WebhookDispatcher{
		endpoints: endpoints,
		client:    &http.Client{Timeout: 5 * time.Second},
	}
}

func (w *WebhookDispatcher) DispatchIncident(inc protocol.Incident) {
	if len(w.endpoints) == 0 {
		return
	}

	payload := protocol.WebhookAlert{
		Event:     "incident.created",
		Incident:  inc,
		Timestamp: time.Now().Unix(),
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	for _, endpoint := range w.endpoints {
		go func(url string) {
			req, err := http.NewRequest("POST", url, bytes.NewReader(data))
			if err != nil {
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", "NodePulse-Webhook/1.0")

			resp, err := w.client.Do(req)
			if err != nil {
				log.Printf("[webhook] failed delivery to %s: %v", url, err)
				return
			}
			resp.Body.Close()
		}(endpoint)
	}
}
