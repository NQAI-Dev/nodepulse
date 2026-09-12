package alerter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func TestWebhookDispatcher(t *testing.T) {
	received := make(chan protocol.WebhookAlert, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var alert protocol.WebhookAlert
		if err := json.NewDecoder(r.Body).Decode(&alert); err != nil {
			t.Errorf("failed to decode alert: %v", err)
			return
		}
		received <- alert
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	d := NewWebhookDispatcher([]string{server.URL})
	inc := protocol.Incident{
		ID:        "101",
		NodeID:    "test-node-1",
		Severity:  "critical",
		Title:     "Test Panic",
		Detail:    "Crash detected",
		StartedAt: 12345678,
	}

	d.DispatchIncident(inc)

	select {
	case alert := <-received:
		if alert.Event != "incident.created" {
			t.Errorf("expected event incident.created, got %s", alert.Event)
		}
		if alert.Incident.NodeID != "test-node-1" {
			t.Errorf("expected node test-node-1, got %s", alert.Incident.NodeID)
		}
	}
}
