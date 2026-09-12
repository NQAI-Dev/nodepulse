package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/autoheal"
	"github.com/NQAI-Dev/nodepulse/pkg/collector"
	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func main() {
	nodeID := flag.String("node", "", "Unique Node ID")
	serverURL := flag.String("server", "http://127.0.0.1:8080/api/v1/ingest", "NodePulse Central URL")
	token := flag.String("token", "", "NodePulse Ingest API Token")
	interval := flag.Duration("interval", 10*time.Second, "Heartbeat interval")
	unitsFlag := flag.String("units", "", "Comma-separated systemd units to monitor")
	tagsFlag := flag.String("tags", "", "Comma-separated node tags, e.g. env=prod,region=eu,role=db")
	dryRun := flag.Bool("dry-run", false, "Collect and print without network push")
	flag.Parse()

	if *token == "" {
		*token = os.Getenv("NODEPULSE_TOKEN")
		if *token == "" {
			*token = "np_live_master_secret"
		}
	}

	if *nodeID == "" {
		*nodeID = os.Getenv("NODEPULSE_NODE_ID")
		if *nodeID == "" {
			hostname, _ := os.Hostname()
			*nodeID = hostname
		}
	}

	units := []string{}
	rawUnits := *unitsFlag
	if rawUnits == "" {
		rawUnits = os.Getenv("NODEPULSE_SYSTEMD_UNITS")
	}
	if rawUnits != "" {
		for _, u := range strings.Split(rawUnits, ",") {
			if trimmed := strings.TrimSpace(u); trimmed != "" {
				units = append(units, trimmed)
			}
		}
	}

	c := collector.New(*nodeID, units).WithTags(collector.TagsFromEnv(*tagsFlag))
	client := &http.Client{Timeout: 5 * time.Second}
	breaker := autoheal.NewBreaker()

	var pendingMu sync.Mutex
	pending := []protocol.AutoHealLog{}

	flushLogs := func() {
		pendingMu.Lock()
		if len(pending) == 0 {
			pendingMu.Unlock()
			return
		}
		batch := pending
		pending = nil
		pendingMu.Unlock()

		base := *serverURL
		if i := strings.Index(base, "/api/v1/"); i >= 0 {
			base = base[:i]
		}
		uploadURL := base + "/api/v1/autoheal/log"

		body, err := json.Marshal(map[string]interface{}{
			"node_id": *nodeID,
			"events":  batch,
		})
		if err != nil {
			return
		}
		req, err := http.NewRequest(http.MethodPost, uploadURL, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if *token != "" {
			req.Header.Set("X-NodePulse-Token", *token)
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}

	log.Printf("NodePulse Edge Agent active. ID: %s, Destination: %s, Cadence: %v, Monitored units: %d", *nodeID, *serverURL, *interval, len(units))

	for {
		hb, err := c.Collect()
		if err != nil {
			log.Printf("Collection failure: %v", err)
			time.Sleep(*interval)
			continue
		}

		data, err := json.Marshal(hb)
		if err != nil {
			log.Printf("Marshal error: %v", err)
			time.Sleep(*interval)
			continue
		}

		if *dryRun {
			var pretty bytes.Buffer
			json.Indent(&pretty, data, "", "  ")
			fmt.Println(pretty.String())
			return
		}

		req, err := http.NewRequest("POST", *serverURL, bytes.NewBuffer(data))
		if err != nil {
			log.Printf("Request creation error: %v", err)
			time.Sleep(*interval)
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if *token != "" {
			req.Header.Set("X-NodePulse-Token", *token)
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Heartbeat delivery failed: %v", err)
		} else {
			var hbResp protocol.HeartbeatResponse
			if err := json.NewDecoder(resp.Body).Decode(&hbResp); err == nil {
				for _, cmd := range hbResp.Commands {
					key := autoheal.Key(cmd)
					decision := breaker.Allow(key)
					ts := time.Now().Unix()

					if !decision.Allowed {
						log.Printf("Auto-heal %s skipped (%s, retry=%s)", key, decision.Reason, decision.RetryAfter)
						pendingMu.Lock()
						pending = append(pending, protocol.AutoHealLog{
							Command:  key,
							Status:   "skipped",
							Reason:   decision.Reason,
							Ts:       ts,
							RetrySec: int64(decision.RetryAfter.Seconds()),
						})
						pendingMu.Unlock()
						continue
					}

					execErr := collector.ExecuteCommand(cmd)
					breaker.Record(key, execErr)
					if execErr != nil {
						log.Printf("Auto-heal %s error: %v", key, execErr)
					} else {
						log.Printf("Auto-heal %s ok", key)
					}
					pendingMu.Lock()
					pending = append(pending, protocol.AutoHealLog{
						Command: key,
						Status:  boolToStatus(execErr == nil),
						Error:   errString(execErr),
						Ts:      ts,
					})
					pendingMu.Unlock()
				}
			}
			resp.Body.Close()
		}

		flushLogs()
		time.Sleep(*interval)
	}
}

func boolToStatus(ok bool) string {
	if ok {
		return "ok"
	}
	return "failed"
}

func errString(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}
