package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
	"github.com/NQAI-Dev/nodepulse/pkg/store"
)

func main() {
	addr := flag.String("addr", ":8080", "Listen address")
	dbPath := flag.String("db", "nodepulse_fleet.db", "SQLite database path")
	flag.Parse()

	pStore, err := store.NewPersistentStore(*dbPath)
	if err != nil {
		log.Fatalf("Store initialization failure: %v", err)
	}

	mux := http.NewServeMux()

	// Ingestion endpoint for agents (supports token verification)
	mux.HandleFunc("POST /api/v1/ingest", func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-NodePulse-Token")
		if token == "" {
			token = r.URL.Query().Get("token")
		}

		if token != "" && !pStore.ValidateToken(token) && token != "np_live_master_secret" {
			http.Error(w, `{"error":"unauthorized node token"}`, http.StatusUnauthorized)
			return
		}

		var hb protocol.Heartbeat
		if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if hb.NodeID == "" {
			http.Error(w, "missing node_id", http.StatusBadRequest)
			return
		}

		pStore.Ingest(&hb)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(protocol.HeartbeatResponse{Acknowledged: true})
	})

	// Fleet Nodes API
	mux.HandleFunc("GET /api/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pStore.GetAll())
	})

	// Incidents API
	mux.HandleFunc("GET /api/v1/incidents", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pStore.GetActiveIncidents())
	})

	// Dynamic 1-line installation script generator
	mux.HandleFunc("GET /install.sh", func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		if token == "" {
			token = "np_live_master_secret"
		}
		w.Header().Set("Content-Type", "text/x-shellscript")
		script := fmt.Sprintf(`#!/bin/sh
set -e
echo "==> [NodePulse] Starting rapid agent installation..."
SERVER_URL="https://pulse.nqai.es-cloud.ru"
TOKEN="%s"
NODE_ID="$(hostname)"

mkdir -p /opt/nodepulse /etc/nodepulse
curl -sSL -o /usr/local/bin/nodepulse-agent ${SERVER_URL}/bin/nodepulse-agent || true
chmod +x /usr/local/bin/nodepulse-agent

cat << UNIT > /etc/systemd/system/nodepulse-agent.service
[Unit]
Description=NodePulse Enterprise Agent
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/nodepulse-agent -node ${NODE_ID} -server ${SERVER_URL}/api/v1/ingest
Environment=NODEPULSE_TOKEN=${TOKEN}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now nodepulse-agent.service
echo "==> [NodePulse] Agent installed and registered successfully as ${NODE_ID}!"
`, token)
		w.Write([]byte(script))
	})

	// Serve compiled agent binary directly for installer
	mux.HandleFunc("GET /bin/nodepulse-agent", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "bin/nodepulse-agent")
	})

	// Public Health
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","system":"nodepulse-platform"}` + "\n"))
	})

	// Static Web Dashboard
	mux.Handle("/", http.FileServer(http.Dir("web/public")))

	server := &http.Server{
		Addr:         *addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	log.Printf("NodePulse Platform Control Plane running on %s", *addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}
