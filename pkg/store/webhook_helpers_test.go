package store

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
)

// newWebhookCaptureServer returns a tiny httptest.Server that counts POST hits
// and replies with `status`. Used by retry tests to verify the dispatcher
// actually hit the wire.
func newWebhookCaptureServer(hits *int, status int) *httptest.Server {
	var counter int64
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			atomic.AddInt64(&counter, 1)
			*hits = int(atomic.LoadInt64(&counter))
		}
		w.WriteHeader(status)
	}))
}
