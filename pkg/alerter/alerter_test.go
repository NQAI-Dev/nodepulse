package alerter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func TestWebhookDispatcher(t *testing.T) {
	received := make(chan protocol.WebhookAlert, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a protocol.WebhookAlert
		if err := json.NewDecoder(r.Body).Decode(&a); err == nil {
			received <- a
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	d := NewWebhook()
	err := d.Dispatch(ts.URL, protocol.WebhookAlert{
		Event:     "incident.created",
		Timestamp: time.Now().Unix(),
		Incident: &protocol.Incident{
			ID:       "1",
			NodeID:   "test-node",
			Severity: "critical",
			Title:    "Test alert",
			Detail:   "Detailed message",
		},
	})

	if err != nil {
		t.Fatalf("dispatch error: %v", err)
	}

	select {
	case a := <-received:
		if a.Event != "incident.created" || a.Incident.NodeID != "test-node" {
			t.Fatalf("unexpected payload: %+v", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook not received in time")
	}
}
