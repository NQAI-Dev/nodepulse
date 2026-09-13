package store

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// AtomFeed represents an Atom 1.0 feed (RFC 4287) for system status & incident notifications.
type AtomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Xmlns   string      `xml:"xmlns,attr"`
	ID      string      `xml:"id"`
	Title   string      `xml:"title"`
	Updated string      `xml:"updated"`
	Link    []AtomLink  `xml:"link"`
	Author  AtomAuthor  `xml:"author"`
	Entries []AtomEntry `xml:"entry"`
}

type AtomLink struct {
	Rel  string `xml:"rel,attr,omitempty"`
	Href string `xml:"href,attr"`
	Type string `xml:"type,attr,omitempty"`
}

type AtomAuthor struct {
	Name string `xml:"name"`
}

type AtomEntry struct {
	ID        string      `xml:"id"`
	Title     string      `xml:"title"`
	Updated   string      `xml:"updated"`
	Published string      `xml:"published"`
	Link      AtomLink    `xml:"link"`
	Summary   AtomSummary `xml:"summary"`
	Content   AtomContent `xml:"content"`
}

type AtomSummary struct {
	Type  string `xml:"type,attr,omitempty"`
	Value string `xml:",chardata"`
}

type AtomContent struct {
	Type  string `xml:"type,attr,omitempty"`
	Value string `xml:",cdata"`
}

// RenderAtomFeed generates RFC 4287 compliant XML for incident history.
func RenderAtomFeed(baseURL, title string, incidents []protocol.PublicIncidentHistory) ([]byte, error) {
	if title == "" {
		title = "NodePulse System Status"
	}
	if baseURL == "" {
		baseURL = "https://pulse.nqai.es-cloud.ru"
	}

	updated := time.Now().UTC()
	if len(incidents) > 0 && incidents[0].StartedAt > 0 {
		t := time.Unix(incidents[0].StartedAt, 0).UTC()
		if t.After(updated) {
			updated = t
		}
	}

	feed := AtomFeed{
		Xmlns:   "http://www.w3.org/2005/Atom",
		ID:      fmt.Sprintf("%s/api/v1/public/feed.atom", baseURL),
		Title:   title,
		Updated: updated.Format(time.RFC3339),
		Link: []AtomLink{
			{Rel: "self", Href: fmt.Sprintf("%s/api/v1/public/feed.atom", baseURL), Type: "application/atom+xml"},
			{Rel: "alternate", Href: fmt.Sprintf("%s/status", baseURL), Type: "text/html"},
		},
		Author: AtomAuthor{Name: "NodePulse Monitoring"},
		Entries: make([]AtomEntry, 0, len(incidents)),
	}

	for _, inc := range incidents {
		pubTime := time.Unix(inc.StartedAt, 0).UTC().Format(time.RFC3339)
		updTime := pubTime
		if inc.Resolved && inc.ResolvedAt > 0 {
			updTime = time.Unix(inc.ResolvedAt, 0).UTC().Format(time.RFC3339)
		}

		statusLabel := "OPEN"
		if inc.Resolved {
			statusLabel = "RESOLVED"
			if inc.ResolutionReason != "" {
				statusLabel = fmt.Sprintf("RESOLVED (%s)", inc.ResolutionReason)
			}
		}

		summary := fmt.Sprintf("[%s] [%s] %s on node %s", statusLabel, inc.Severity, inc.Title, inc.NodeID)
		htmlContent := fmt.Sprintf(
			"<p><strong>Status:</strong> %s</p><p><strong>Severity:</strong> %s</p><p><strong>Node:</strong> %s</p><p><strong>Started:</strong> %s</p>",
			statusLabel, inc.Severity, inc.NodeID, pubTime,
		)
		if inc.Resolved {
			htmlContent += fmt.Sprintf("<p><strong>Resolved:</strong> %s</p>", updTime)
		}

		entry := AtomEntry{
			ID:        fmt.Sprintf("urn:nodepulse:incident:%s", inc.ID),
			Title:     fmt.Sprintf("[%s] %s", inc.Severity, inc.Title),
			Updated:   updTime,
			Published: pubTime,
			Link: AtomLink{
				Rel:  "alternate",
				Href: fmt.Sprintf("%s/status#incident-%s", baseURL, inc.ID),
				Type: "text/html",
			},
			Summary: AtomSummary{
				Type:  "text",
				Value: summary,
			},
			Content: AtomContent{
				Type:  "html",
				Value: htmlContent,
			},
		}
		feed.Entries = append(feed.Entries, entry)
	}

	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(feed); err != nil {
		return nil, err
	}
	buf.WriteString("\n")
	return buf.Bytes(), nil
}
