package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/billing"
	"github.com/NQAI-Dev/nodepulse/pkg/metrics"
	"github.com/NQAI-Dev/nodepulse/pkg/protocol"
	"github.com/NQAI-Dev/nodepulse/pkg/store"
)

func main() {
	addr := flag.String("addr", ":8080", "Listen address")
	dbPath := flag.String("db", "nodepulse_fleet.db", "SQLite database path")
	botToken := flag.String("tg-token", "", "Telegram Bot Token for alerts")
	chatIDStr := flag.String("tg-chat", "", "Telegram Chat ID for alerts")
	cryptoToken := flag.String("crypto-token", "", "CryptoBot API Token")
	flag.Parse()

	token := *botToken
	if token == "" {
		token = os.Getenv("NODEPULSE_TG_TOKEN")
	}

	var chatID int64
	cStr := *chatIDStr
	if cStr == "" {
		cStr = os.Getenv("NODEPULSE_TG_CHAT")
	}
	if cStr != "" {
		chatID, _ = strconv.ParseInt(cStr, 10, 64)
	}

	cToken := *cryptoToken
	if cToken == "" {
		cToken = os.Getenv("CRYPTOBOT_API_TOKEN")
	}
	cryptoClient := billing.NewCryptoBot(cToken)

	serverStart := time.Now()
	pStore, err := store.NewPersistentStore(*dbPath, token, chatID)
	if err != nil {
		log.Fatalf("Store initialization failure: %v", err)
	}
	pStore.InitBillingSchema()

	mux := http.NewServeMux()

	getUser := func(r *http.Request) (int64, string, error) {
		auth := r.Header.Get("Authorization")
		tok := strings.TrimPrefix(auth, "Bearer ")
		if tok == "" {
			tok = r.URL.Query().Get("token")
		}
		return pStore.GetUserByToken(tok)
	}

	// 1. Auth endpoints
	mux.HandleFunc("POST /api/v1/auth/register", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.AuthRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Username) == "" || len(req.Password) < 6 {
			http.Error(w, `{"error":"username required, password min 6 chars"}`, http.StatusBadRequest)
			return
		}
		uid, tok, err := pStore.Register(req.Username, req.Password)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(protocol.AuthResponse{
			Token: tok,
			User: protocol.User{
				ID:       fmt.Sprintf("%d", uid),
				Username: req.Username,
			},
		})
	})

	mux.HandleFunc("POST /api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.AuthRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
			return
		}
		uid, tok, err := pStore.Authenticate(req.Username, req.Password)
		if err != nil {
			http.Error(w, `{"error":"invalid credentials"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(protocol.AuthResponse{
			Token: tok,
			User: protocol.User{
				ID:       fmt.Sprintf("%d", uid),
				Username: req.Username,
			},
		})
	})

	// 2. Billing endpoints
	mux.HandleFunc("POST /api/v1/billing/create-invoice", func(w http.ResponseWriter, r *http.Request) {
		uid, uname, err := getUser(r)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		inv, err := cryptoClient.CreateInvoice("5.00", "USDT", "NodePulse Pro Plan (1 Month)", fmt.Sprintf("%d", uid))
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
			return
		}

		invID := fmt.Sprintf("%d", inv.Result.InvoiceID)
		pStore.SaveInvoice(invID, uid, "pro", inv.Result.Amount, inv.Result.PayURL)

		log.Printf("Created invoice %s for user %s (id %d)", invID, uname, uid)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(protocol.InvoiceResponse{
			InvoiceID: invID,
			PayURL:    inv.Result.PayURL,
			Amount:    inv.Result.Amount,
			Currency:  inv.Result.Asset,
		})
	})

	mux.HandleFunc("POST /api/v1/billing/webhook", func(w http.ResponseWriter, r *http.Request) {
		var hook protocol.CryptoBotWebhook
		if err := json.NewDecoder(r.Body).Decode(&hook); err != nil {
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}

		if hook.Payload.Status == "paid" {
			invID := fmt.Sprintf("%d", hook.Payload.InvoiceID)
			uid, err := pStore.MarkInvoicePaid(invID)
			if err != nil {
				log.Printf("Error marking invoice %s paid: %v", invID, err)
			} else {
				log.Printf("Invoice %s successfully paid! Upgraded user %d to PRO", invID, uid)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})

	mux.HandleFunc("GET /api/v1/billing/plan", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		plan, _ := pStore.GetUserPlan(uid)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"user_id": uid,
			"plan":    plan,
		})
	})

	// 3. Ingestion endpoint for agents (validates token, evaluates autoheal & limits)
	mux.HandleFunc("POST /api/v1/ingest", func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-NodePulse-Token")
		if token == "" {
			token = r.URL.Query().Get("token")
		}

		uid, _, err := pStore.GetUserByToken(token)
		if err != nil && token != "np_live_master_secret" {
			http.Error(w, `{"error":"unauthorized node token"}`, http.StatusUnauthorized)
			return
		}

		var hb protocol.Heartbeat
		if err := json.NewDecoder(r.Body).Decode(&hb); err != nil || hb.NodeID == "" {
			http.Error(w, `{"error":"invalid payload"}`, http.StatusBadRequest)
			return
		}

		if uid > 0 {
			if !pStore.CanAddNode(uid) {
				http.Error(w, `{"error":"node limit reached, upgrade to PRO"}`, http.StatusPaymentRequired)
				return
			}
			pStore.BindNode(hb.NodeID, uid)
		}
		pStore.Ingest(&hb)

		// Evaluate auto-heal remediation commands
		commands := pStore.EvaluateAutoHeal(&hb)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(protocol.HeartbeatResponse{
			Acknowledged: true,
			Commands:     commands,
		})
	})

	// 4. Fleet Nodes API
	mux.HandleFunc("GET /api/v1/public/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pStore.GetPublicStatus())
	})

	mux.HandleFunc("GET /api/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			uid = 1
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pStore.GetUserNodes(uid))
	})

	// 5. Settings API
	mux.HandleFunc("GET /api/v1/settings", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		settings, err := pStore.GetSettings(uid)
		if err != nil {
			http.Error(w, `{"error":"failed to fetch settings"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(settings)
	})

	mux.HandleFunc("POST /api/v1/settings", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		var req protocol.UserSettings
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid settings payload"}`, http.StatusBadRequest)
			return
		}
		if err := pStore.UpdateSettings(uid, req.TelegramChatID, req.WebhookURL, req.WebhookSecret, req.NotifyCritical, req.NotifyWarning); err != nil {
			http.Error(w, `{"error":"failed to update settings"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
	})

	// 6. Incidents API
	mux.HandleFunc("GET /api/v1/incidents", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			uid = 1
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pStore.GetActiveIncidents(uid))
	})

	mux.HandleFunc("POST /api/v1/incidents/resolve", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, `{"error":"missing id"}`, http.StatusBadRequest)
			return
		}
		if err := pStore.ResolveIncident(id); err != nil {
			http.Error(w, `{"error":"resolve failure"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true}` + "\n"))
	})

	// 6. Dynamic 1-line installation script generator
	mux.HandleFunc("GET /install.sh", func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		if token == "" {
			token = "np_live_master_secret"
		}
		w.Header().Set("Content-Type", "text/x-shellscript")
		script := fmt.Sprintf(`#!/bin/sh
set -e
echo "==> [NodePulse] Installing NodePulse Enterprise Agent..."
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
Environment=NODEPULSE_TOKEN=%s
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now nodepulse-agent.service
echo "==> [NodePulse] Agent installed and registered successfully as ${NODE_ID}!"
`, token, token)
		w.Write([]byte(script))
	})

	mux.HandleFunc("GET /bin/nodepulse-agent", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "bin/nodepulse-agent")
	})

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "web/public/status.html")
	})

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","system":"nodepulse-platform"}` + "\n"))
	})

	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if err := metrics.WriteFleet(w, pStore, serverStart); err != nil {
			log.Printf("metrics render error: %v", err)
		}
	})

	mux.Handle("/", http.FileServer(http.Dir("web/public")))

	server := &http.Server{
		Addr:         *addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	log.Printf("NodePulse Platform with Billing running on %s", *addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}
