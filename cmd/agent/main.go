package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/collector"
	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
)

func main() {
	nodeID := flag.String("node", "", "Unique Node ID")
	serverURL := flag.String("server", "http://127.0.0.1:8080/api/v1/ingest", "NodePulse Central URL")
	interval := flag.Duration("interval", 15*time.Second, "Heartbeat interval")
	dryRun := flag.Bool("dry-run", false, "Collect and print without network push")
	flag.Parse()

	if *nodeID == "" {
		*nodeID = os.Getenv("NODEPULSE_NODE_ID")
		if *nodeID == "" {
			hostname, _ := os.Hostname()
			*nodeID = hostname
		}
	}

	c := collector.New(*nodeID)
	client := &http.Client{Timeout: 5 * time.Second}

	log.Printf("NodePulse Edge Agent active. ID: %s, Destination: %s, Cadence: %v", *nodeID, *serverURL, *interval)

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

		resp, err := client.Post(*serverURL, "application/json", bytes.NewBuffer(data))
		if err != nil {
			log.Printf("Heartbeat delivery failed: %v", err)
		} else {
			var hbResp protocol.HeartbeatResponse
			json.NewDecoder(resp.Body).Decode(&hbResp)
			resp.Body.Close()
			if !hbResp.Acknowledged {
				log.Printf("Server rejected heartbeat")
			}
		}

		time.Sleep(*interval)
	}
}
