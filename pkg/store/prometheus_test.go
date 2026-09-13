package store

import (
	"strings"
	"testing"
)

// Self-check: the Prometheus exposition format requires every metric to be
// prefixed with a HELP and TYPE line, and labels must be wrapped in curly
// braces with backslash-escaped quotes. We seed a tiny scenario with only
// public store methods (CreateIncident) and assert the rendered output
// parses back into the expected metric/label pairs. No external Prometheus
// server needed — this is just the writer surface.
func TestPrometheusMetricsShape(t *testing.T) {
	s, err := NewPersistentStore(":memory:", "", 0)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// No Close on PersistentStore — in-memory SQLite is released when the
	// *sql.DB handle is dropped. Tests don't need explicit cleanup.

	_ = s.CreateIncident("n1", "critical", "Docker Down", "x")
	_ = s.CreateIncident("n2", "warning", "High CPU", "y")

	body, err := s.PrometheusMetrics()
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	wantSubstrings := []string{
		"# HELP nodepulse_fleet_nodes",
		"# TYPE nodepulse_fleet_nodes gauge",
		`nodepulse_incidents_open{severity="critical"} 1`,
		`nodepulse_incidents_open{severity="warning"} 1`,
		`# TYPE nodepulse_incidents_total counter`,
		`# TYPE nodepulse_probes_total_1h counter`,
		`# TYPE nodepulse_webhook_deliveries_1h counter`,
		`# TYPE nodepulse_autoheal_actions_1h counter`,
	}
	for _, w := range wantSubstrings {
		if !strings.Contains(body, w) {
			t.Fatalf("missing %q in output:\n%s", w, body)
		}
	}
}

// Self-check: label values containing quotes, backslashes and newlines
// must be escaped per the Prometheus spec, otherwise scrapers reject the
// whole scrape with a parse error.
func TestEscapeLabel(t *testing.T) {
	cases := map[string]string{
		"plain":                  "plain",
		`quote " inside`:         `quote \" inside`,
		`back\slash`:             `back\\slash`,
		"line\nbreak":            `line\nbreak`,
		`all " \ ` + "\n" + ` x`: `all \" \\ \n x`,
	}
	for in, want := range cases {
		if got := escapeLabel(in); got != want {
			t.Errorf("escapeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
