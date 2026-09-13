package store

import (
	"fmt"
	"strings"
)

// RenderSVGStatusBadge returns an SVG badge in shields.io style for system status or uptime.
// label is left side (e.g. "status", "uptime").
// status is right side ("operational", "degraded", "outage", "100%", etc.).
func RenderSVGStatusBadge(label, status string) string {
	if label == "" {
		label = "status"
	}
	color := "#4c1" // green
	norm := strings.ToLower(status)
	switch norm {
	case "operational", "online", "up", "healthy":
		color = "#4c1" // green
	case "degraded", "warning", "warn":
		color = "#dfb317" // yellow/orange
	case "outage", "offline", "down", "critical", "error":
		color = "#e05d44" // red
	default:
		if strings.HasSuffix(norm, "%") {
			color = "#4c1"
		} else {
			color = "#007ec6" // blue
		}
	}

	charWidth := 7
	padding := 10
	labelWidth := len(label)*charWidth + padding*2
	statusWidth := len(status)*charWidth + padding*2
	totalWidth := labelWidth + statusWidth

	labelX := labelWidth / 2
	statusX := labelWidth + statusWidth/2

	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="20" role="img" aria-label="%s: %s">
  <linearGradient id="b" x2="0" y2="100%%">
    <stop offset="0" stop-color="#bbb" stop-opacity=".1"/>
    <stop offset="1" stop-opacity=".1"/>
  </linearGradient>
  <clipPath id="a">
    <rect width="%d" height="20" rx="3" fill="#fff"/>
  </clipPath>
  <g clip-path="url(#a)">
    <rect width="%d" height="20" fill="#555"/>
    <rect x="%d" width="%d" height="20" fill="%s"/>
    <rect width="%d" height="20" fill="url(#b)"/>
  </g>
  <g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" text-rendering="geometricPrecision" font-size="110">
    <text x="%d" y="150" fill="#010101" fill-opacity=".3" transform="scale(.1)">%s</text>
    <text x="%d" y="140" transform="scale(.1)">%s</text>
    <text x="%d" y="150" fill="#010101" fill-opacity=".3" transform="scale(.1)">%s</text>
    <text x="%d" y="140" transform="scale(.1)">%s</text>
  </g>
</svg>`,
		totalWidth, label, status,
		totalWidth,
		labelWidth,
		labelWidth, statusWidth, color,
		totalWidth,
		labelX*10, label,
		labelX*10, label,
		statusX*10, status,
		statusX*10, status,
	)
}
