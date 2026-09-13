package store

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

// RSSFeed represents an RSS 2.0 feed channel for system status & incident notifications.
type RSSFeed struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Channel RSSChannel `xml:"channel"`
}

type RSSChannel struct {
	Title         string    `xml:"title"`
	Link          string    `xml:"link"`
	Description   string    `xml:"description"`
	LastBuildDate string    `xml:"lastBuildDate"`
	Generator     string    `xml:"generator"`
	Items         []RSSItem `xml:"item"`
}

type RSSItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	PubDate     string `xml:"pubDate"`
	GUID        string `xml:"guid"`
}

// RenderRSSFeed generates RSS 2.0 compliant XML for incident history.
func RenderRSSFeed(baseURL, title string, incidents []protocol.PublicIncidentHistory) ([]byte, error) {
	if title == "" {
		title = "NodePulse System Status"
	}
	if baseURL == "" {
		baseURL = "https://pulse.nqai.es-cloud.ru"
	}

	buildTime := time.Now().UTC()
	if len(incidents) > 0 && incidents[0].StartedAt > 0 {
		t := time.Unix(incidents[0].StartedAt, 0).UTC()
		if t.After(buildTime) {
			buildTime = t
		}
	}

	feed := RSSFeed{
		Version: "2.0",
		Channel: RSSChannel{
			Title:         title,
			Link:          fmt.Sprintf("%s/status", baseURL),
			Description:   "Real-time operational status updates, service notices, and incident logs.",
			LastBuildDate: buildTime.Format(time.RFC1123Z),
			Generator:     "NodePulse Monitoring",
			Items:         make([]RSSItem, 0, len(incidents)),
		},
	}

	for _, inc := range incidents {
		pubTime := time.Unix(inc.StartedAt, 0).UTC().Format(time.RFC1123Z)
		statusLabel := "OPEN"
		if inc.Resolved {
			statusLabel = "RESOLVED"
			if inc.ResolutionReason != "" {
				statusLabel = fmt.Sprintf("RESOLVED (%s)", inc.ResolutionReason)
			}
		}

		desc := fmt.Sprintf("[%s] [%s] %s on node %s (Started: %s)", statusLabel, inc.Severity, inc.Title, inc.NodeID, pubTime)
		if inc.Resolved && inc.ResolvedAt > 0 {
			resTime := time.Unix(inc.ResolvedAt, 0).UTC().Format(time.RFC1123Z)
			desc += fmt.Sprintf(" - Resolved at: %s", resTime)
		}

		item := RSSItem{
			Title:       fmt.Sprintf("[%s] %s", inc.Severity, inc.Title),
			Link:        fmt.Sprintf("%s/status#incident-%s", baseURL, inc.ID),
			Description: desc,
			PubDate:     pubTime,
			GUID:        fmt.Sprintf("urn:nodepulse:incident:%s", inc.ID),
		}
		feed.Channel.Items = append(feed.Channel.Items, item)
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
