package probe

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func TestNewRunnerDedupesAndTrims(t *testing.T) {
	r := NewRunner([]string{"  https://a.example/health ", "", "https://a.example/health", "https://b.example"}, 0, false)
	if got, want := len(r.Targets()), 2; got != want {
		t.Fatalf("targets len = %d, want %d", got, want)
	}
	if r.Targets()[0] != "https://a.example/health" {
		t.Errorf("targets[0] = %q, want trimmed url", r.Targets()[0])
	}
}

func TestRunReportsOKFor2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	r := NewRunner([]string{srv.URL}, 2*time.Second, false)
	got := r.Run()
	if len(got) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(got))
	}
	res := got[0]
	if !res.OK {
		t.Errorf("OK = false, want true (status=%d err=%q)", res.StatusCode, res.Error)
	}
	if res.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", res.StatusCode)
	}
	if res.LatencyMs < 0 {
		t.Errorf("LatencyMs = %d, want >=0", res.LatencyMs)
	}
	if res.Ts == 0 {
		t.Errorf("Ts = 0, want non-zero unix seconds")
	}
}

func TestRunReportsFailingFor5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := NewRunner([]string{srv.URL}, 2*time.Second, false)
	got := r.Run()
	if got[0].OK {
		t.Errorf("OK = true, want false for 500")
	}
	if got[0].StatusCode != 500 {
		t.Errorf("StatusCode = %d, want 500", got[0].StatusCode)
	}
	if !strings.Contains(got[0].Error, "Internal Server Error") {
		t.Errorf("Error = %q, want contains status text", got[0].Error)
	}
}

func TestRunReportsTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := NewRunner([]string{srv.URL}, 50*time.Millisecond, false)
	got := r.Run()
	if got[0].OK {
		t.Errorf("OK = true, want false on timeout")
	}
	if got[0].Error == "" {
		t.Errorf("Error = empty, want timeout message")
	}
}

func TestRunReportsInvalidURL(t *testing.T) {
	r := NewRunner([]string{"://bad url", "http://[::1"}, 1*time.Second, false)
	got := r.Run()
	if len(got) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(got))
	}
	for i, res := range got {
		if res.OK {
			t.Errorf("results[%d].OK = true, want false for invalid URL", i)
		}
		if !strings.HasPrefix(res.Error, "invalid_url:") {
			t.Errorf("results[%d].Error = %q, want invalid_url prefix", i, res.Error)
		}
	}
}

func TestRunRespectsFollowRedirect(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// off: probe sees 302, not OK
	off := NewRunner([]string{srv.URL + "/redirect"}, 2*time.Second, false)
	if res := off.Run()[0]; res.OK || res.StatusCode != http.StatusFound {
		t.Errorf("followRedirect=false: OK=%v status=%d, want OK=false status=302", res.OK, res.StatusCode)
	}
	// on: probe sees 200
	on := NewRunner([]string{srv.URL + "/redirect"}, 2*time.Second, true)
	if res := on.Run()[0]; !res.OK || res.StatusCode != http.StatusOK {
		t.Errorf("followRedirect=true: OK=%v status=%d, want OK=true status=200", res.OK, res.StatusCode)
	}
}

func TestRunIsParallelAndStable(t *testing.T) {
	// Verify results come back in the same order as targets even when
	// some handlers sleep longer than others.
	var delay atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// path /slow -> 200ms; /fast -> instant
		if strings.Contains(r.URL.String(), "slow") {
			time.Sleep(200 * time.Millisecond)
		}
		delay.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := NewRunner([]string{srv.URL + "/slow", srv.URL + "/fast"}, 2*time.Second, false)
	got := r.Run()
	if len(got) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(got))
	}
	if !strings.HasSuffix(got[0].URL, "/slow") {
		t.Errorf("results[0].URL = %q, want /slow (order must match input)", got[0].URL)
	}
	if !strings.HasSuffix(got[1].URL, "/fast") {
		t.Errorf("results[1].URL = %q, want /fast (order must match input)", got[1].URL)
	}
	if delay.Load() != 2 {
		t.Errorf("handlers hit %d times, want 2", delay.Load())
	}
}

func TestRunEmptyTargetsReturnsNil(t *testing.T) {
	r := NewRunner(nil, time.Second, false)
	if got := r.Run(); got != nil {
		t.Errorf("Run() = %v, want nil", got)
	}
	r = NewRunner([]string{"", "   "}, time.Second, false)
	if got := r.Run(); got != nil {
		t.Errorf("Run() with blanks = %v, want nil", got)
	}
}

// Sanity guard: a result with StatusCode 0 and OK=false carries an Error
// string, otherwise the public status page renders an empty tooltip.
func TestProbeResultAlwaysExplainsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()
	r := NewRunner([]string{srv.URL}, time.Second, false)
	res := r.Run()[0]
	if res.OK {
		t.Fatalf("418 must not be OK")
	}
	if res.Error == "" {
		t.Errorf("Error must be populated for non-2xx; got empty")
	}
	// And it must remain a protocol.ProbeResult so JSON encoding on the
	// wire stays stable for the control plane.
	var _ protocol.ProbeResult = res
}
