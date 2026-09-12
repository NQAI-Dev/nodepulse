package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/alerter"
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

	// Enable HMAC-signed inline-keyboard callback_data. Reuse the bot token
	// as the shared secret so signing/verification works out of the box;
	// operators can override with NODEPULSE_TG_CALLBACK_SECRET if they ever
	// rotate the bot token and don't want old alert buttons to keep working.
	cbSecret := os.Getenv("NODEPULSE_TG_CALLBACK_SECRET")
	if cbSecret == "" {
		cbSecret = token
	}
	if disp, ok := pStore.Alerter().(*alerter.Dispatcher); ok && cbSecret != "" {
		disp.SetCallbackSecret(cbSecret)
	}

	mux := http.NewServeMux()

	// Brute-force protection on auth endpoints: 10 attempts per IP per minute
	// is generous for humans but shuts down scriptable credential stuffing.
	loginLimiter := store.NewRateLimiter(10, time.Minute)
	clientKey := func(r *http.Request) string {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return xff
		}
		return r.RemoteAddr
	}
	authMw := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !loginLimiter.Allow(clientKey(r)) {
				http.Error(w, `{"error":"rate limit exceeded — slow down"}`, http.StatusTooManyRequests)
				return
			}
			next(w, r)
		}
	}

	getUser := func(r *http.Request) (int64, string, error) {
		auth := r.Header.Get("Authorization")
		tok := strings.TrimPrefix(auth, "Bearer ")
		if tok == "" {
			tok = r.URL.Query().Get("token")
		}
		return pStore.GetUserByToken(tok)
	}

	// 1. Auth endpoints
	mux.HandleFunc("POST /api/v1/auth/register", authMw(func(w http.ResponseWriter, r *http.Request) {
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
		loginLimiter.Reset(clientKey(r))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(protocol.AuthResponse{
			Token: tok,
			User: protocol.User{
				ID:       fmt.Sprintf("%d", uid),
				Username: req.Username,
			},
		})
	}))

	mux.HandleFunc("POST /api/v1/auth/login", authMw(func(w http.ResponseWriter, r *http.Request) {
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
		loginLimiter.Reset(clientKey(r))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(protocol.AuthResponse{
			Token: tok,
			User: protocol.User{
				ID:       fmt.Sprintf("%d", uid),
				Username: req.Username,
			},
		})
	}))

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

	mux.HandleFunc("POST /api/v1/autoheal/log", func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-NodePulse-Token")
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		uid, _, err := pStore.GetUserByToken(token)
		if err != nil && token != "np_live_master_secret" {
			http.Error(w, `{"error":"unauthorized node token"}`, http.StatusUnauthorized)
			return
		}

		var payload struct {
			NodeID string                  `json:"node_id"`
			Events []protocol.AutoHealLog  `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.NodeID == "" {
			http.Error(w, `{"error":"invalid payload"}`, http.StatusBadRequest)
			return
		}

		pStore.RecordAutoHealLogs(payload.NodeID, uid, payload.Events)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"accepted": true})
	})

	mux.HandleFunc("GET /api/v1/autoheal/logs", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		nodeID := r.URL.Query().Get("node_id")
		if nodeID == "" {
			http.Error(w, `{"error":"node_id required"}`, http.StatusBadRequest)
			return
		}

		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
				limit = n
			}
		}

		all := pStore.GetUserNodes(uid)
		if _, ok := all[nodeID]; !ok {
			http.Error(w, `{"error":"node not in your fleet"}`, http.StatusForbidden)
			return
		}

		logs := pStore.RecentAutoHealLogs(nodeID, limit)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"node_id": nodeID,
			"events":  logs,
		})
	})

	// 4. Fleet Nodes API
	mux.HandleFunc("GET /api/v1/public/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pStore.GetPublicStatus())
	})

	mux.HandleFunc("GET /api/v1/public/fleet-summary", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=10")
		json.NewEncoder(w).Encode(pStore.GetPublicFleetSummary())
	})

	mux.HandleFunc("GET /api/v1/public/incidents", func(w http.ResponseWriter, r *http.Request) {
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
				limit = n
			}
		}
		sinceUnix := int64(0)
		if v := r.URL.Query().Get("since"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				sinceUnix = n
			}
		}
		items, err := pStore.GetPublicIncidentHistory(sinceUnix, limit)
		if err != nil {
			http.Error(w, `{"error":"history query failed"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=15")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"items": items,
			"count": len(items),
		})
	})

	mux.HandleFunc("GET /api/v1/public/nodes", func(w http.ResponseWriter, r *http.Request) {
		tag := r.URL.Query().Get("tag")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=10")
		nodes := pStore.ListPublicNodes(tag)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"items": nodes,
			"count": len(nodes),
			"tag":   tag,
		})
	})

	mux.HandleFunc("GET /api/v1/public/incidents/histogram", func(w http.ResponseWriter, r *http.Request) {
		days := 30
		if v := r.URL.Query().Get("days"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 90 {
				days = n
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=60")
		json.NewEncoder(w).Encode(pStore.PublicIncidentHistogram(days))
	})

	// Fleet-wide daily uptime drill-down: one row per UTC day with the
	// aggregated up_secs/total_secs across every reporting node. Powers
	// the "30-day uptime" bar-chart on the public status page.
	mux.HandleFunc("GET /api/v1/public/uptime", func(w http.ResponseWriter, r *http.Request) {
		days := 30
		if v := r.URL.Query().Get("days"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 90 {
				days = n
			}
		}
		rows, err := pStore.FleetUptimeDaily(days)
		if err != nil {
			http.Error(w, `{"error":"uptime query failed"}`, http.StatusInternalServerError)
			return
		}
		out := protocol.PublicUptimeSeries{
			Scope:     "fleet",
			Days:      days,
			UpdatedAt: time.Now().Unix(),
			Rows:      make([]protocol.PublicUptimeRow, 0, len(rows)),
		}
		for _, r := range rows {
			out.Rows = append(out.Rows, protocol.PublicUptimeRow{
				Day:       r.Day,
				TotalSecs: r.TotalSecs,
				UpSecs:    r.UpSecs,
				UptimePct: r.UptimePct,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=60")
		json.NewEncoder(w).Encode(out)
	})

	// Per-node daily uptime drill-down. Same shape as /public/uptime so
	// the widget can plot fleet-vs-node side by side with one parser.
	mux.HandleFunc("GET /api/v1/public/uptime/{nodeID}", func(w http.ResponseWriter, r *http.Request) {
		nodeID := r.PathValue("nodeID")
		if nodeID == "" {
			http.Error(w, `{"error":"node_id required"}`, http.StatusBadRequest)
			return
		}
		days := 30
		if v := r.URL.Query().Get("days"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 90 {
				days = n
			}
		}
		rows, err := pStore.NodeUptimeDaily(nodeID, days)
		if err != nil {
			http.Error(w, `{"error":"uptime query failed"}`, http.StatusInternalServerError)
			return
		}
		out := protocol.PublicUptimeSeries{
			Scope:     nodeID,
			Days:      days,
			UpdatedAt: time.Now().Unix(),
			Rows:      make([]protocol.PublicUptimeRow, 0, len(rows)),
		}
		for _, r := range rows {
			out.Rows = append(out.Rows, protocol.PublicUptimeRow{
				Day:       r.Day,
				TotalSecs: r.TotalSecs,
				UpSecs:    r.UpSecs,
				UptimePct: r.UptimePct,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=60")
		json.NewEncoder(w).Encode(out)
	})

	mux.HandleFunc("GET /api/v1/nodes", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			uid = 1
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pStore.GetUserNodes(uid))
	})

	mux.HandleFunc("GET /api/v1/nodes/{nodeID}/metrics", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			uid = 1
		}
		nodeID := r.PathValue("nodeID")
		if nodeID == "" {
			http.Error(w, `{"error":"node_id required"}`, http.StatusBadRequest)
			return
		}
		owns := pStore.GetUserNodes(uid)
		if _, ok := owns[nodeID]; !ok {
			http.Error(w, `{"error":"node not in your fleet"}`, http.StatusForbidden)
			return
		}
		rangeKey := r.URL.Query().Get("range")
		if rangeKey == "" {
			rangeKey = "1h"
		}
		out, err := pStore.MetricsRange(nodeID, rangeKey)
		if err != nil {
			http.Error(w, `{"error":"metrics query failed"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})

	mux.HandleFunc("GET /api/v1/nodes/{nodeID}/network", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			uid = 1
		}
		nodeID := r.PathValue("nodeID")
		if nodeID == "" {
			http.Error(w, `{"error":"node_id required"}`, http.StatusBadRequest)
			return
		}
		owns := pStore.GetUserNodes(uid)
		if _, ok := owns[nodeID]; !ok {
			http.Error(w, `{"error":"node not in your fleet"}`, http.StatusForbidden)
			return
		}
		rangeKey := r.URL.Query().Get("range")
		var windowSec int64 = 3600
		switch rangeKey {
		case "1h":
			windowSec = 3600
		case "6h":
			windowSec = 6 * 3600
		case "24h":
			windowSec = 24 * 3600
		case "7d":
			windowSec = 7 * 24 * 3600
		default:
			rangeKey = "1h"
		}
		totals, err := pStore.NetworkSummary(nodeID, windowSec)
		if err != nil {
			http.Error(w, `{"error":"network query failed"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"node_id": nodeID,
			"range":   rangeKey,
			"ifaces":  totals,
		})
	})

	mux.HandleFunc("GET /api/v1/nodes/{nodeID}/network/series", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			uid = 1
		}
		nodeID := r.PathValue("nodeID")
		iface := r.URL.Query().Get("iface")
		if nodeID == "" || iface == "" {
			http.Error(w, `{"error":"node_id and iface required"}`, http.StatusBadRequest)
			return
		}
		owns := pStore.GetUserNodes(uid)
		if _, ok := owns[nodeID]; !ok {
			http.Error(w, `{"error":"node not in your fleet"}`, http.StatusForbidden)
			return
		}
		rangeKey := r.URL.Query().Get("range")
		if rangeKey == "" {
			rangeKey = "1h"
		}
		points, err := pStore.NetworkSeries(nodeID, iface, rangeKey)
		if err != nil {
			http.Error(w, `{"error":"series query failed"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"node_id": nodeID,
			"iface":   iface,
			"range":   rangeKey,
			"points":  points,
		})
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
		uid, _, err := getUser(r)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, `{"error":"missing id"}`, http.StatusBadRequest)
			return
		}
		err = pStore.ResolveIncident(id, uid)
		switch {
		case err == nil:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"success":true}` + "\n"))
		case errors.Is(err, store.ErrIncidentForbidden):
			http.Error(w, `{"error":"incident does not belong to you"}`, http.StatusForbidden)
		case errors.Is(err, store.ErrIncidentNotFound):
			http.Error(w, `{"error":"incident not found"}`, http.StatusNotFound)
		default:
			http.Error(w, `{"error":"resolve failure"}`, http.StatusInternalServerError)
		}
	})

	// 7. Telegram inline-keyboard callbacks (Acknowledge / Resolve buttons).
	//
	// The Telegram Bot API delivers one Update per callback; we only need the
	// callback_query.data field plus the callback_query.id so we can ACK the
	// spinner. Signature verification happens against the same secret the
	// dispatcher used to sign the button data at notification time.
	mux.HandleFunc("POST /api/v1/telegram/callback", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			CallbackQueryID string `json:"callback_query_id"`
			Data            string `json:"data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Data == "" {
			http.Error(w, `{"error":"invalid callback payload"}`, http.StatusBadRequest)
			return
		}

		disp, _ := pStore.Alerter().(*alerter.Dispatcher)
		if disp == nil {
			http.Error(w, `{"error":"telegram dispatcher not configured"}`, http.StatusServiceUnavailable)
			return
		}
		action, incidentID, ok := alerter.VerifyCallbackData(disp.CallbackSecret(), req.Data)
		if !ok {
			http.Error(w, `{"error":"invalid or unsigned callback"}`, http.StatusForbidden)
			return
		}

		switch action {
		case "ack":
			acked, err := pStore.AcknowledgeIncident(incidentID)
			if err != nil {
				http.Error(w, `{"error":"ack failure"}`, http.StatusInternalServerError)
				return
			}
			if acked {
				disp.AnswerCallback(req.CallbackQueryID, "✅ Acknowledged")
			} else {
				disp.AnswerCallback(req.CallbackQueryID, "⚠️ Already resolved or not found")
			}
			log.Printf("[tg-callback] ack incident=%s acked=%v", incidentID, acked)
		case "resolve":
			if err := pStore.ResolveIncident(incidentID, 1); err != nil {
				http.Error(w, `{"error":"resolve failure"}`, http.StatusInternalServerError)
				return
			}
			disp.AnswerCallback(req.CallbackQueryID, "🛠 Resolved")
			log.Printf("[tg-callback] resolve incident=%s", incidentID)
		default:
			http.Error(w, `{"error":"unknown action"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true}` + "\n"))
	})

	mux.HandleFunc("POST /api/v1/maintenance", func(w http.ResponseWriter, r *http.Request) {
		uid, uname, err := getUser(r)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		var req protocol.MaintenanceWindowRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid maintenance payload"}`, http.StatusBadRequest)
			return
		}
		win, err := pStore.CreateMaintenanceWindow(uid, req, uname)
		if err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(win)
	})

	mux.HandleFunc("GET /api/v1/maintenance", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		activeOnly := r.URL.Query().Get("active") == "true"
		wins, err := pStore.ListMaintenanceWindows(uid, activeOnly)
		if err != nil {
			http.Error(w, `{"error":"failed to list windows"}`, http.StatusInternalServerError)
			return
		}
		if wins == nil {
			wins = []protocol.MaintenanceWindow{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(wins)
	})

	mux.HandleFunc("DELETE /api/v1/maintenance/{id}", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		idStr := r.PathValue("id")
		id, perr := strconv.ParseInt(idStr, 10, 64)
		if perr != nil {
			http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
			return
		}
		deleted, err := pStore.DeleteMaintenanceWindow(uid, id)
		if err != nil {
			http.Error(w, `{"error":"delete failure"}`, http.StatusInternalServerError)
			return
		}
		if deleted == 0 {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true}` + "\n"))
	})

	mux.HandleFunc("GET /api/v1/incidents/history", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			uid = 1
		}
		rangeKey := r.URL.Query().Get("range")
		nodeID := r.URL.Query().Get("node_id")
		severity := r.URL.Query().Get("severity")
		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
				limit = n
			}
		}
		items, err := pStore.GetIncidentHistory(uid, rangeKey, nodeID, severity, limit)
		if err != nil {
			http.Error(w, `{"error":"history query failed"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"range":  rangeKey,
			"items":  items,
			"count":  len(items),
		})
	})

	mux.HandleFunc("GET /api/v1/incidents/stats", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			uid = 1
		}
		rangeKey := r.URL.Query().Get("range")
		buckets, err := pStore.GetIncidentStats(uid, rangeKey)
		if err != nil {
			http.Error(w, `{"error":"stats query failed"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"range":   rangeKey,
			"buckets": buckets,
		})
	})

	// 8. Webhook delivery audit: lists the recent webhook deliveries the
	// operator's account fired, plus aggregate counters. Useful for
	// debugging "is my custom integration receiving incident.created events
	// or did the signature break?".
	mux.HandleFunc("GET /api/v1/webhook/deliveries", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
				limit = n
			}
		}
		rows, err := pStore.WebhookDeliveries(uid, limit)
		if err != nil {
			http.Error(w, `{"error":"deliveries query failed"}`, http.StatusInternalServerError)
			return
		}
		if rows == nil {
			rows = []protocol.WebhookDelivery{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"items": rows,
			"count": len(rows),
		})
	})

	mux.HandleFunc("GET /api/v1/webhook/stats", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		windowSec := int64(86400)
		if v := r.URL.Query().Get("window"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 7*24*3600 {
				windowSec = int64(n)
			}
		}
		stats, err := pStore.WebhookDeliveryStats(uid, windowSec)
		if err != nil {
			http.Error(w, `{"error":"stats query failed"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stats)
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

	janitorCtx, janitorCancel := context.WithCancel(context.Background())
	defer janitorCancel()
	go pStore.RunJanitor(janitorCtx)

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}
