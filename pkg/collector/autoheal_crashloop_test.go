package collector

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// startFakeDockerSocket simulates the subset of the Docker unix-socket API
// we exercise: POST /containers/{id}/restart and GET /containers/{id}/json.
// The caller controls state via state so each test can describe the
// "happy", "exited" and "restarting" transitions the live check should
// distinguish. Returns the bare host:port (no scheme) ready to drop into
// the package-level dockerSocketPath variable.
func startFakeDockerSocket(t *testing.T, state string, calls *int32) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/restart") {
			atomic.AddInt32(calls, 1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		body := fmt.Sprintf(`{"State":{"Status":%q,"Running":%t,"Restarting":%t,"ExitCode":1,"Error":"","FinishedAt":"2026-09-13T00:00:00Z"}}`,
			state,
			state == "running",
			state == "restarting",
		)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	// Strip scheme; dialDockerSocket expects a bare host:port when the
	// value does not start with "/".
	addr := strings.TrimPrefix(srv.URL, "http://")
	return addr
}

func useFakeSocket(t *testing.T, addr string) {
	t.Helper()
	prev := dockerSocketPath
	dockerSocketPath = addr
	t.Cleanup(func() { dockerSocketPath = prev })
}

func TestPostRestartLiveCheck_HealthyContainerIsOK(t *testing.T) {
	var calls int32
	useFakeSocket(t, startFakeDockerSocket(t, "running", &calls))

	if err := restartDockerContainer("happy"); err != nil {
		t.Fatalf("healthy container should report nil, got %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("expected exactly 1 restart call, got %d", calls)
	}
}

func TestPostRestartLiveCheck_CrashLoopReturnsSentinel(t *testing.T) {
	var calls int32
	useFakeSocket(t, startFakeDockerSocket(t, "exited", &calls))

	err := restartDockerContainer("crashloop")
	if err == nil {
		t.Fatalf("exited container must produce an error")
	}
	if !errors.Is(err, ErrPostRestartDown) {
		t.Fatalf("expected ErrPostRestartDown, got %v", err)
	}
	if !strings.Contains(err.Error(), "state=exited") {
		t.Fatalf("error should expose container state for the operator, got %q", err.Error())
	}
}

func TestPostRestartLiveCheck_RestartingIsNotCrash(t *testing.T) {
	var calls int32
	useFakeSocket(t, startFakeDockerSocket(t, "restarting", &calls))

	if err := restartDockerContainer("churn"); err != nil {
		t.Fatalf("Docker-managed restart must not be classified as a crash, got %v", err)
	}
}

// TestPostRestartLiveCheck_InspectFailureDoesNotMaskRestart guards against a
// regression where a flaky inspect call could flip a successful restart into
// an error and start blaming the agent for a network blip.
func TestPostRestartLiveCheck_InspectFailureDoesNotMaskRestart(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/restart") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	useFakeSocket(t, strings.TrimPrefix(srv.URL, "http://"))

	if err := restartDockerContainer("flaky"); err != nil {
		t.Fatalf("inspect failure must not propagate, got %v", err)
	}
}

// TestExecuteCommandDispatchesSentinel ensures the exported function
// surfaces the sentinel through the wrapping fmt.Errorf path so callers can
// rely on errors.Is without depending on internal formatting.
func TestExecuteCommandDispatchesSentinel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/restart") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(`{"State":{"Running":false,"Restarting":false,"ExitCode":137}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	useFakeSocket(t, strings.TrimPrefix(srv.URL, "http://"))

	err := ExecuteCommand("restart_docker:killloop")
	if !errors.Is(err, ErrPostRestartDown) {
		t.Fatalf("ExecuteCommand must propagate ErrPostRestartDown, got %v", err)
	}
}
