package store

import (
	"strings"
	"testing"
)

func TestRenderSVGStatusBadge(t *testing.T) {
	cases := []struct {
		label    string
		status   string
		expected string
	}{
		{"status", "operational", "#4c1"},
		{"status", "degraded", "#dfb317"},
		{"status", "outage", "#e05d44"},
		{"uptime", "99.9%", "#4c1"},
		{"custom", "info", "#007ec6"},
	}

	for _, c := range cases {
		svg := RenderSVGStatusBadge(c.label, c.status)
		if !strings.Contains(svg, "<svg") || !strings.Contains(svg, "</svg>") {
			t.Errorf("expected valid svg tags for %s/%s", c.label, c.status)
		}
		if !strings.Contains(svg, c.label) {
			t.Errorf("expected svg to contain label %s", c.label)
		}
		if !strings.Contains(svg, c.status) {
			t.Errorf("expected svg to contain status %s", c.status)
		}
		if !strings.Contains(svg, c.expected) {
			t.Errorf("expected color %s for status %s in svg", c.expected, c.status)
		}
	}
}
