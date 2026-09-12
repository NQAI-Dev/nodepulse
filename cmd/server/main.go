package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
	"github.com/NQAI-Dev/nodepulse/pkg/store"
)

func main() {
	addr := flag.String("addr", ":8080", "Listen address")
	flag.Parse()

	db := store.New()

	mux := http.NewServeMux()

	// Ingestion endpoint for agents
	mux.HandleFunc("POST /api/v1/ingest", func(w http.ResponseWriter, r *http.Request) {
		var hb protocol.Heartbeat
		if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if hb.NodeID == "" {
			http.Error(w, "missing node_id", http.StatusBadRequest)
			return
		}

		db.Ingest(&hb)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(protocol.HeartbeatResponse{Acknowledged: true})
	})

	// Platform status & fleet API
	mux.HandleFunc("GET /api/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(db.GetAll())
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
