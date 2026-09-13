package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/alerter"
	"github.com/NQAI-Dev/nodepulse/pkg/billing"
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

	pStore, err := store.NewPersistentStore(*dbPath, token, chatID)
	if err != nil {
		log.Fatalf("Store initialization failure: %v", err)
	}
	pStore.InitBillingSchema()

	// Wire Slack + Discord fan-out. The chat dispatcher reuses the
	// WebhookRecorder owned by the store so audit rows land in the
	// same buffer as the generic webhook deliveries — operators get a
	// single /api/v1/webhook/deliveries feed across all four channels.
	pStore.SetChatDispatcher(alerter.NewChatDispatcher(pStore.Recorder()))

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

	// Telegram Login Widget callback: GET /api/v1/tg-callback?id=...&first_name=...&...&hash=...
	// Validates the HMAC-SHA256 signature using the bot token, then provisions
	// (or reuses) a user keyed by tg_id and redirects to the dashboard with
	// the new API token in the URL fragment (dashboard JS reads it into
	// localStorage so the operator never has to copy/paste it).
	mux.HandleFunc("GET /api/v1/tg-callback", func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			http.Error(w, `{"error":"tg-callback not configured: set NODEPULSE_TG_TOKEN or -tg-token"}`, http.StatusServiceUnavailable)
			return
		}
		q := r.URL.Query()
		hash := q.Get("hash")
		if hash == "" {
			http.Error(w, `{"error":"missing hash"}`, http.StatusBadRequest)
			return
		}

		// Build the canonical check string: all fields except `hash`, sorted
		// alphabetically, joined with \n as `key=value` pairs.
		q.Del("hash")
		keys := make([]string, 0, len(q))
		for k := range q {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, k+"="+q.Get(k))
		}
		checkString := strings.Join(parts, "\n")

		// Telegram spec: secret_key = sha256(bot_token), then HMAC-SHA256 the
		// check string with that secret. Compare with the supplied hash in
		// constant time so timing leaks can't help an attacker guess fields.
		secretKey := sha256.Sum256([]byte(token))
		mac := hmac.New(sha256.New, secretKey[:])
		mac.Write([]byte(checkString))
		computed := hex.EncodeToString(mac.Sum(nil))
		if subtle.ConstantTimeCompare([]byte(computed), []byte(hash)) != 1 {
			http.Error(w, `{"error":"invalid hash"}`, http.StatusUnauthorized)
			return
		}

		// auth_date freshness check: 5 minutes. Telegram documents do not
		// mandate this, but accepting week-old signatures would let a
		// screenshot of the widget authorize forever.
		authDateStr := q.Get("auth_date")
		if authDate, err := strconv.ParseInt(authDateStr, 10, 64); err == nil {
			if time.Now().Unix()-authDate > 300 {
				http.Error(w, `{"error":"auth_date too old"}`, http.StatusUnauthorized)
				return
			}
		}

		tgID, err := strconv.ParseInt(q.Get("id"), 10, 64)
		if err != nil || tgID == 0 {
			http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
			return
		}
		uid, tok, err := pStore.RegisterByTelegram(tgID, q.Get("first_name"), q.Get("username"))
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
			return
		}
		log.Printf("[tg-callback] login ok tg_id=%d uid=%d", tgID, uid)

		// Redirect to dashboard with token in URL fragment so the dashboard
		// JS picks it up via `window.location.hash` and stashes it in
		// localStorage without ever sending it to a third-party server log.
		fragment := fmt.Sprintf("token=%s&user_id=%d&tg_id=%d", tok, uid, tgID)
		http.Redirect(w, r, "/index.html#"+fragment, http.StatusFound)
	})

	// 1b. Invite endpoints. These back the nodepulse-bot `/start <token>`
	// deep-link onboarding flow: an operator mints an invite via
	// POST /api/v1/invites (master-token auth) and shares the resulting
	// `https://t.me/nodepulse_mon_bot?start=<token>` URL with a user.
	// When the user clicks, the bot calls POST /api/v1/invites/redeem
	// with the token + chat_id; the server marks the invite redeemed
	// and returns the bound user_id so the bot knows which tenant the
	// chat belongs to. Handler bodies live in invites_handlers.go so
	// they can be unit-tested without spinning up the full mux.
	mux.HandleFunc("POST /api/v1/invites", func(w http.ResponseWriter, r *http.Request) {
		handleInviteCreate(pStore, w, r)
	})
	mux.HandleFunc("GET /api/v1/invites", func(w http.ResponseWriter, r *http.Request) {
		handleInviteList(pStore, w, r)
	})
	mux.HandleFunc("POST /api/v1/invites/redeem", func(w http.ResponseWriter, r *http.Request) {
		handleInviteRedeem(pStore, w, r)
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
		usage, _ := pStore.GetPlanUsage(uid)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(usage)
	})

	// 3. Ingestion endpoint for agents (validates token, evaluates autoheal & limits).
	// Handler body lives in file-scope handleHeartbeatIngest so it can be
	// unit-tested against an injected *store.PersistentStore without
	// spinning up the full mux.
	mux.HandleFunc("POST /api/v1/ingest", func(w http.ResponseWriter, r *http.Request) {
		handleHeartbeatIngest(w, r, pStore)
	})

	mux.HandleFunc("POST /api/v1/autoheal/log", func(w http.ResponseWriter, r *http.Request) {
		handleAutohealLog(w, r, pStore)
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
	// Prometheus scrape endpoint. Unauthenticated on purpose — like the
	// `/metrics` convention from the reference servers, this assumes the
	// scraper lives inside the trusted network. If you expose this to the
	// public internet, gate it behind a reverse-proxy ACL or a bearer
	// token middleware that scrapers can carry. Cache 10s to avoid
	// hammering SQLite on a sub-second scrape interval.
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		body, err := pStore.PrometheusMetrics()
		if err != nil {
			http.Error(w, "metrics render failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=10")
		w.Write([]byte(body))
	})

	mux.HandleFunc("GET /api/v1/public/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pStore.GetPublicStatus())
	})

	mux.HandleFunc("GET /api/v1/public/fleet-summary", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=10")
		json.NewEncoder(w).Encode(pStore.GetPublicFleetSummary())
	})

	// Public Atom feed for incident tracking and syndication
	renderFeed := func(w http.ResponseWriter, r *http.Request) {
		limit := 100
		sinceUnix := time.Now().Unix() - int64(30*24*3600) // last 30 days
		items, err := pStore.GetPublicIncidentHistory(sinceUnix, limit)
		if err != nil {
			http.Error(w, "feed generation failed", http.StatusInternalServerError)
			return
		}
		scheme := "https"
		if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" && r.Host != "pulse.nqai.es-cloud.ru" {
			scheme = "http"
		}
		baseURL := fmt.Sprintf("%s://%s", scheme, r.Host)
		feedBytes, err := store.RenderAtomFeed(baseURL, "NodePulse System Status", items)
		if err != nil {
			http.Error(w, "feed encoding failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=60")
		w.Write(feedBytes)
	}
	mux.HandleFunc("GET /api/v1/public/feed.atom", renderFeed)
	mux.HandleFunc("GET /status/feed.atom", renderFeed)
	mux.HandleFunc("GET /feed.atom", renderFeed)

	renderRss := func(w http.ResponseWriter, r *http.Request) {
		items, err := pStore.GetPublicIncidentHistory(0, 50)
		if err != nil {
			http.Error(w, "feed fetch failed", http.StatusInternalServerError)
			return
		}
		scheme := "https"
		if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" && r.Host != "pulse.nqai.es-cloud.ru" {
			scheme = "http"
		}
		baseURL := fmt.Sprintf("%s://%s", scheme, r.Host)
		rssBytes, err := store.RenderRSSFeed(baseURL, "NodePulse System Status", items)
		if err != nil {
			http.Error(w, "feed encoding failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=60")
		w.Write(rssBytes)
	}
	mux.HandleFunc("GET /api/v1/public/feed.rss", renderRss)
	mux.HandleFunc("GET /status/feed.rss", renderRss)
	mux.HandleFunc("GET /feed.rss", renderRss)

	mux.HandleFunc("GET /api/v1/public/badge", func(w http.ResponseWriter, r *http.Request) {
		label := r.URL.Query().Get("label")
		if label == "" {
			label = "status"
		}
		status := pStore.GetPublicFleetSummary().Status
		if v := r.URL.Query().Get("type"); v == "uptime" {
			label = "uptime"
			status = fmt.Sprintf("%.1f%%", pStore.GetPublicFleetSummary().Uptime7dPct)
		}
		svg := store.RenderSVGStatusBadge(label, status)
		w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=60, s-maxage=60")
		w.Write([]byte(svg))
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

	mux.HandleFunc("GET /api/v1/public/maintenance", func(w http.ResponseWriter, r *http.Request) {
		notices, err := pStore.GetPublicMaintenanceWindows()
		if err != nil {
			http.Error(w, `{"error":"maintenance query failed"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=30")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"items": notices,
			"count": len(notices),
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

	mux.HandleFunc("GET /api/v1/public/probes", func(w http.ResponseWriter, r *http.Request) {
		windowSecs := int64(86400)
		if v := r.URL.Query().Get("window"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 && n <= 604800 {
				windowSecs = n
			}
		}
		// Optional kind filter (http|tcp|tls|dns). Unknown values yield an empty
		// list rather than 400 — the public page should never break on
		// a typo in an upstream URL parameter.
		kind := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("kind")))
		var summaries []store.ProbeSummary
		var err error
		if kind == "" {
			summaries, err = pStore.ProbeSummaries(windowSecs)
		} else {
			summaries, err = pStore.ProbeSummariesByKind(windowSecs, kind)
		}
		if err != nil {
			http.Error(w, `{"error":"probes query failed"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// short TTL — probe fleet can change fast and the page polls
		w.Header().Set("Cache-Control", "public, max-age=15")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"items": summaries,
			"count": len(summaries),
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
		if err := pStore.UpdateSettings(uid, req.TelegramChatID, req.WebhookURL, req.WebhookSecret, req.SlackWebhookURL, req.DiscordWebhookURL, req.NotifyCritical, req.NotifyWarning); err != nil {
			http.Error(w, `{"error":"failed to update settings"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
	})

	// Test-settings endpoint: fires a real Telegram message and/or signed
	// webhook using the user's saved config so they can verify wiring
	// without waiting for an actual incident. Body fields are optional and
	// let the UI override the saved target for one-off tests (e.g. forward
	// to a second channel); only the secret is required to be the saved one
	// when supplied (so a forged chat_id can't leak the real secret).
	mux.HandleFunc("POST /api/v1/settings/test", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		var req struct {
			Channel   string `json:"channel"` // "telegram", "webhook", "slack", "discord", or "all"
			ChatID    string `json:"chat_id"`
			WebhookURL string `json:"webhook_url"`
			WebhookSecret string `json:"webhook_secret"`
			SlackURL    string `json:"slack_url"`
			DiscordURL  string `json:"discord_url"`
			NodeID      string `json:"node_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		channel := req.Channel
		if channel == "" {
			channel = "all"
		}
		nodeID := req.NodeID
		if nodeID == "" {
			nodeID = "(unknown)"
		}

		settings, err := pStore.GetSettings(uid)
		if err != nil {
			http.Error(w, `{"error":"failed to fetch settings"}`, http.StatusInternalServerError)
			return
		}

		type channelResult struct {
			Channel string `json:"channel"`
			OK      bool   `json:"ok"`
			Error   string `json:"error,omitempty"`
		}
		var results []channelResult

		if channel == "telegram" || channel == "all" {
			chatID := req.ChatID
			if chatID == "" {
				chatID = settings.TelegramChatID
			}
			var cid int64
			if chatID != "" {
				cid, _ = strconv.ParseInt(chatID, 10, 64)
			}
			if disp, ok := pStore.Alerter().(*alerter.Dispatcher); ok {
				if disp.BotToken() == "" {
					results = append(results, channelResult{Channel: "telegram", OK: false, Error: "telegram bot not configured on server"})
				} else if err := disp.SendTestAlert(cid); err != nil {
					results = append(results, channelResult{Channel: "telegram", OK: false, Error: err.Error()})
				} else {
					results = append(results, channelResult{Channel: "telegram", OK: true})
				}
			} else {
				results = append(results, channelResult{Channel: "telegram", OK: false, Error: "alerter dispatcher unavailable"})
			}
		}

		if channel == "webhook" || channel == "all" {
			url := req.WebhookURL
			secret := req.WebhookSecret
			if url == "" {
				url = settings.WebhookURL
				secret = settings.WebhookSecret
			}
			if url == "" {
				results = append(results, channelResult{Channel: "webhook", OK: false, Error: "webhook URL is empty"})
			} else if err := pStore.Recorder().SendTestWebhook(uid, url, secret); err != nil {
				results = append(results, channelResult{Channel: "webhook", OK: false, Error: err.Error()})
			} else {
				results = append(results, channelResult{Channel: "webhook", OK: true})
			}
		}

		// Slack + Discord test branches. The chat dispatcher is optional
		// (older binaries / tests don't wire it); the saved URLs come from
		// settings unless the operator passes overrides for one-off testing
		// against a second workspace.
		if chat := pStore.ChatDispatcher(); chat != nil {
			if channel == "slack" || channel == "all" {
				slackURL := req.SlackURL
				if slackURL == "" {
					slackURL = settings.SlackWebhookURL
				}
				if slackURL == "" {
					results = append(results, channelResult{Channel: "slack", OK: false, Error: "slack webhook URL is empty"})
				} else if err := chat.SendTest("slack", slackURL, nodeID); err != nil {
					results = append(results, channelResult{Channel: "slack", OK: false, Error: err.Error()})
				} else {
					results = append(results, channelResult{Channel: "slack", OK: true})
				}
			}
			if channel == "discord" || channel == "all" {
				discordURL := req.DiscordURL
				if discordURL == "" {
					discordURL = settings.DiscordWebhookURL
				}
				if discordURL == "" {
					results = append(results, channelResult{Channel: "discord", OK: false, Error: "discord webhook URL is empty"})
				} else if err := chat.SendTest("discord", discordURL, nodeID); err != nil {
					results = append(results, channelResult{Channel: "discord", OK: false, Error: err.Error()})
				} else {
					results = append(results, channelResult{Channel: "discord", OK: true})
				}
			}
		} else if channel == "slack" || channel == "discord" || channel == "all" {
			if channel == "slack" || channel == "all" {
				results = append(results, channelResult{Channel: "slack", OK: false, Error: "chat dispatcher not configured on server"})
			}
			if channel == "discord" || channel == "all" {
				results = append(results, channelResult{Channel: "discord", OK: false, Error: "chat dispatcher not configured on server"})
			}
		}

		anyOK := false
		for _, r := range results {
			if r.OK {
				anyOK = true
				break
			}
		}
		status := http.StatusOK
		if !anyOK && len(results) > 0 {
			status = http.StatusBadGateway
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]interface{}{"results": results})
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

	// Metric alert rules CRUD. Each rule pins a CPU/MEM/DISK/load1
	// threshold to a node or to a tag-selected fleet. The evaluator runs
	// on every heartbeat from cmd/server/ingest; these handlers only own
	// the lifecycle and listing endpoints.
	mux.HandleFunc("GET /api/v1/alert-rules", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		nodeID := r.URL.Query().Get("node_id")
		rules, listErr := pStore.ListMetricAlertRules(uid, nodeID)
		if listErr != nil {
			http.Error(w, `{"error":"failed to list rules"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"rules": rules})
	})

	mux.HandleFunc("POST /api/v1/alert-rules", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		var req protocol.MetricAlertRuleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid payload"}`, http.StatusBadRequest)
			return
		}
		id, err := pStore.CreateMetricAlertRule(uid, req)
		if errors.Is(err, store.ErrAlertRuleInvalid) {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, `{"error":"failed to create rule"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{"id": id})
	})

	mux.HandleFunc("PUT /api/v1/alert-rules/{id}", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		idStr := r.PathValue("id")
		idInt, parseErr := strconv.ParseInt(idStr, 10, 64)
		if parseErr != nil || idInt <= 0 {
			http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
			return
		}
		var req protocol.MetricAlertRuleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid payload"}`, http.StatusBadRequest)
			return
		}
		if err := pStore.UpdateMetricAlertRule(uid, idInt, req); err != nil {
			if errors.Is(err, store.ErrAlertRuleInvalid) {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
				return
			}
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, `{"error":"rule not found"}`, http.StatusNotFound)
				return
			}
			http.Error(w, `{"error":"failed to update rule"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true}` + "\n"))
	})

	mux.HandleFunc("DELETE /api/v1/alert-rules/{id}", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		idStr := r.PathValue("id")
		idInt, parseErr := strconv.ParseInt(idStr, 10, 64)
		if parseErr != nil || idInt <= 0 {
			http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
			return
		}
		if err := pStore.DeleteMetricAlertRule(uid, idInt); err != nil {
			http.Error(w, `{"error":"failed to delete rule"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true}` + "\n"))
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
		case "snooze":
			// Snooze lives on its own prefix because the body carries an
			// extra `:<dur>` segment after the id; the generic
			// VerifyCallbackData only parses the (action, id) pair.
			snoozeID, durKey, ok := alerter.VerifySnoozeCallback(disp.CallbackSecret(), req.Data)
			if !ok {
				http.Error(w, `{"error":"invalid snooze payload"}`, http.StatusForbidden)
				return
			}
			until, err := pStore.SnoozeIncident(snoozeID, alerter.SnoozeSeconds(durKey))
			if err != nil {
				switch {
				case errors.Is(err, store.ErrSnoozeIncidentNotFound):
					disp.AnswerCallback(req.CallbackQueryID, "⚠️ Already resolved or not found")
					http.Error(w, `{"error":"incident not found"}`, http.StatusNotFound)
				case errors.Is(err, store.ErrSnoozeDurationInvalid):
					disp.AnswerCallback(req.CallbackQueryID, "⚠️ Invalid snooze duration")
					http.Error(w, `{"error":"invalid duration"}`, http.StatusBadRequest)
				default:
					http.Error(w, `{"error":"snooze failure"}`, http.StatusInternalServerError)
				}
				return
			}
			disp.AnswerCallback(req.CallbackQueryID, fmt.Sprintf("🔕 Snoozed for %s", durKey))
			log.Printf("[tg-callback] snooze incident=%s dur=%s until=%d", snoozeID, durKey, until)
		default:
			http.Error(w, `{"error":"unknown action"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true}` + "\n"))
	})

	// Incident operator notes — chat-grade comments attached to a single
	// incident. Used by the UI to render a hand-off timeline alongside the
	// existing ack/resolve events. Authenticated; user_id + username come
	// from the session so the audit trail is self-attributing.
	mux.HandleFunc("POST /api/v1/incidents/{id}/notes", func(w http.ResponseWriter, r *http.Request) {
		uid, uname, err := getUser(r)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		id := r.PathValue("id")
		if id == "" {
			http.Error(w, `{"error":"missing id"}`, http.StatusBadRequest)
			return
		}
		var req struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid payload"}`, http.StatusBadRequest)
			return
		}
		note, err := pStore.AddIncidentNote(id, uid, uname, req.Body)
		switch {
		case err == nil:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(note)
		case errors.Is(err, store.ErrIncidentNoteEmpty):
			http.Error(w, `{"error":"note body is empty"}`, http.StatusBadRequest)
		case errors.Is(err, store.ErrIncidentNoteTooLong):
			http.Error(w, `{"error":"note body exceeds 1024 chars"}`, http.StatusBadRequest)
		case errors.Is(err, store.ErrIncidentNotFound):
			http.Error(w, `{"error":"incident not found"}`, http.StatusNotFound)
		default:
			http.Error(w, `{"error":"add note failure"}`, http.StatusInternalServerError)
		}
	})

	mux.HandleFunc("GET /api/v1/incidents/{id}/notes", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		id := r.PathValue("id")
		if id == "" {
			http.Error(w, `{"error":"missing id"}`, http.StatusBadRequest)
			return
		}
		// Cheap ownership guard: admin (uid=1) sees everything; everyone
		// else only the incidents attached to their own nodes. This mirrors
		// the scoping used by GetIncidentHistory so a stale token can't
		// enumerate notes for someone else's fleet.
		owns, _ := pStore.UserOwnsIncident(uid, id)
		if uid > 1 && !owns {
			http.Error(w, `{"error":"incident not found"}`, http.StatusNotFound)
			return
		}
		notes, err := pStore.ListIncidentNotes(id)
		if err != nil {
			http.Error(w, `{"error":"list notes failure"}`, http.StatusInternalServerError)
			return
		}
		if notes == nil {
			notes = []store.IncidentNote{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(notes)
	})

	// Merged incident timeline — every ack/resolve/note event for one
	// incident, chronologically ordered. Cheaper than the UI firing three
	// parallel requests and keeps the audit hand-off story in one place.
	mux.HandleFunc("GET /api/v1/incidents/{id}/timeline", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		id := r.PathValue("id")
		if id == "" {
			http.Error(w, `{"error":"missing id"}`, http.StatusBadRequest)
			return
		}
		owns, _ := pStore.UserOwnsIncident(uid, id)
		if uid > 1 && !owns {
			http.Error(w, `{"error":"incident not found"}`, http.StatusNotFound)
			return
		}
		events, err := pStore.IncidentTimeline(id)
		if err != nil {
			http.Error(w, `{"error":"timeline failure"}`, http.StatusInternalServerError)
			return
		}
		if events == nil {
			events = []store.TimelineEvent{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(events)
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

	// Close a maintenance window early (without losing the audit row).
	// Distinct from DELETE: this stamps end_unix=now so the silence lifts
	// but the row stays in the closed history. Idempotent.
	mux.HandleFunc("POST /api/v1/maintenance/{id}/close", func(w http.ResponseWriter, r *http.Request) {
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
		n, err := pStore.CloseMaintenanceWindow(uid, id)
		if err != nil {
			http.Error(w, `{"error":"close failure"}`, http.StatusInternalServerError)
			return
		}
		if n == 0 {
			// Not id-forbidden vs not-found vs already-closed: same response,
			// because the user-facing semantics are identical (the window is
			// not active right now). Saves us from leaking ownership info.
			http.Error(w, `{"error":"not found or already closed"}`, http.StatusNotFound)
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

	// Chat-channel stats: in-memory counter snapshot from the chat
	// dispatcher. Keyed by host (no full URLs leak) so an on-call
	// engineer can see which Slack workspace is dropping pings. Returns
	// an empty object if chat channels are not configured (legacy
	// binary) — clients should treat empty as "not in use".
	mux.HandleFunc("GET /api/v1/dispatch/stats", func(w http.ResponseWriter, r *http.Request) {
		if _, _, err := getUser(r); err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		var slack, discord map[string]alerter.ChatChannelStats
		if cd := pStore.ChatDispatcher(); cd != nil {
			slack, discord = cd.ChannelStats()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"slack":   slack,
			"discord": discord,
		})
	})

	// 8b. Manual retry of a previously-failed webhook delivery. Looks up the
	// stored payload (snapshot of the original WebhookAlert) and re-dispatches
	// it to the recorded URL with the user's *current* webhook secret for
	// signing. Writes a fresh audit row so the retry outcome is visible next
	// to the original failure.
	mux.HandleFunc("POST /api/v1/webhook/deliveries/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		idStr := r.PathValue("id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, `{"error":"invalid delivery id"}`, http.StatusBadRequest)
			return
		}
		newRow, err := pStore.RetryWebhookDelivery(uid, id)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrWebhookDeliveryNotFound):
				http.Error(w, `{"error":"delivery not found"}`, http.StatusNotFound)
			case errors.Is(err, store.ErrWebhookDeliveryNoPayload):
				http.Error(w, `{"error":"delivery has no captured payload — cannot retry"}`, http.StatusUnprocessableEntity)
			case errors.Is(err, store.ErrWebhookDeliveryBadPayload):
				http.Error(w, `{"error":"delivery payload is unreadable"}`, http.StatusUnprocessableEntity)
			default:
				http.Error(w, `{"error":"retry failed"}`, http.StatusInternalServerError)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(newRow)
	})

	// Operator-only retention knob: purge a single webhook audit row. The
	// store scopes the delete by (id, user_id), so a token from one tenant
	// cannot wipe another tenant's log. Idempotent: deleting an unknown id
	// returns 200 with `deleted: false` so the UI doesn't have to special-
	// case "already gone" on a refresh.
	mux.HandleFunc("DELETE /api/v1/webhook/deliveries/{id}", func(w http.ResponseWriter, r *http.Request) {
		uid, _, err := getUser(r)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		idStr := r.PathValue("id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, `{"error":"invalid delivery id"}`, http.StatusBadRequest)
			return
		}
		deleted, err := pStore.DeleteWebhookDelivery(uid, id)
		if err != nil {
			http.Error(w, `{"error":"delete failed"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"deleted":%t}`+"\n", deleted)
	})

	// 6. Dynamic 1-line installation script generator.
	//
	// Token must be supplied via ?token= and must validate against the
	// api_tokens table. We deliberately refuse to fall back to the master
	// admin token: a bare /install.sh would otherwise leak the master token
	// in plain text and silently register the resulting agent under admin,
	// which would break per-user tenant isolation for any user who runs the
	// dashboard URL without their own token (or anyone who runs the bare URL).
	mux.HandleFunc("GET /install.sh", func(w http.ResponseWriter, r *http.Request) {
		installScript(w, r, pStore.ValidateToken)
	})

	mux.HandleFunc("GET /bin/nodepulse-agent", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "bin/nodepulse-agent")
	})

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "web/public/status.html")
	})

	mux.HandleFunc("GET /landing", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "web/public/landing.html")
	})

	mux.HandleFunc("GET /welcome", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/landing", http.StatusFound)
	})

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","system":"nodepulse-platform"}` + "\n"))
	})

	// /api/v1/ready is the readiness probe: returns 200 only if the DB
	// roundtrips within the 2s deadline. Stays separate from /health so
	// load balancers can use /health for liveness (process up, always
	// cheap) and /api/v1/ready for readiness (deps healthy, drop from
	// pool if SQLite is wedged). Cheap enough for a sub-second LB poll.
	mux.HandleFunc("GET /api/v1/ready", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pStore.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{
				"status": "not_ready",
				"reason": err.Error(),
			})
			return
		}
		w.Write([]byte(`{"status":"ready","checks":{"db":"ok"}}` + "\n"))
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

// probeURLs extracts the URL field from a batch of probe results, deduped.
// Used by the ingest handler to count distinct NEW URLs against the
// per-user probe quota. Returned slice order is unspecified.
func probeURLs(results []protocol.ProbeResult) []string {
	seen := make(map[string]struct{}, len(results))
	out := make([]string, 0, len(results))
	for _, r := range results {
		if _, ok := seen[r.URL]; ok {
			continue
		}
		seen[r.URL] = struct{}{}
		out = append(out, r.URL)
	}
	return out
}

// installScript serves a per-user agent install script for GET /install.sh.
//
// The script must only be served after a valid api_tokens row is presented,
// otherwise unauthenticated requests would leak the master admin token in
// the rendered shell script and any agent installed from a leaked URL would
// register under admin instead of the requesting user — breaking per-user
// tenant isolation.
//
// validateToken should return true exactly for tokens that exist in the
// api_tokens table (or the literal master admin token; both branches are
// safe because the master token still uniquely identifies the admin user
// and is itself never returned by this endpoint without a prior valid
// request).
func installScript(w http.ResponseWriter, r *http.Request, validateToken func(string) bool) {
	token := r.URL.Query().Get("token")
	if token == "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		http.Error(w, "missing ?token=<your-api-token> \u2014 log in at https://pulse.nqai.es-cloud.ru/ to obtain one", http.StatusBadRequest)
		return
	}
	if !validateToken(token) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		http.Error(w, "invalid or revoked token \u2014 log in again to get a fresh one", http.StatusForbidden)
		return
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
}

// handleHeartbeatIngest validates and records an agent's heartbeat.
//
// Flow: read the node token (header or ?token= fallback) → resolve the
// owning user via GetUserByToken, with master-token impersonation →
// decode the JSON heartbeat payload → enforce the per-user node limit
// (free-tier cap, requires upgrade past limit) → bind the node to the
// user (BindNode is a write — failure here used to be silently masked by
// the silent-p.db.Exec class; b6842be converted it to error-returning,
// so we surface a 500 instead of pretending the heartbeat succeeded).
// Persist the synthetic probe results best-effort (probe pipeline is
// telemetry, not a hard dependency of ingest), evaluate auto-heal
// remediation commands, and acknowledge.
//
// File-scope (not a closure inside main) so that handlers can be
// exercised in isolation by unit tests against a real *PersistentStore —
// see TestHeartbeatIngest_BindNodeFailure in main_test.go for the
// regression that pins the BindNode error path.
func handleHeartbeatIngest(w http.ResponseWriter, r *http.Request, pStore *store.PersistentStore) {
	token := r.Header.Get("X-NodePulse-Token")
	if token == "" {
		token = r.URL.Query().Get("token")
	}

	uid, _, err := pStore.GetUserByToken(token)
	if err != nil && !pStore.IsMasterToken(token) {
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
		if err := pStore.BindNode(hb.NodeID, uid); err != nil {
			// BindNode failure must surface as 500, not a silent
			// discarded-error swallow. The silent-p.db.Exec anti-pattern
			// that previously lived here masked schema drift on
			// node_owners until the 2026-09-13 prod incident forced a
			// full audit.
			log.Printf("[heartbeat] bind node %q to uid=%d failed: %v", hb.NodeID, uid, err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}
	}
	pStore.Ingest(&hb)

	// Persist synthetic HTTP probe results (if the agent sent any).
	// Failures here must NOT poison the heartbeat response — the probe
	// pipeline is best-effort telemetry, never a hard dependency of
	// ingest. Enforce the per-user probe URL budget before writing: free
	// users capped at FreeProbeLimit distinct URLs across their fleet,
	// pro is unlimited.
	if len(hb.Probes) > 0 {
		if uid > 0 && !pStore.CanAddProbe(uid, probeURLs(hb.Probes)) {
			log.Printf("probe quota exceeded for user %d on node %s", uid, hb.NodeID)
			// Drop the offending batch but keep the heartbeat ack green
			// — the operator can fix the agent config and the next
			// heartbeat will resume collection.
			hb.Probes = nil
		}
		if len(hb.Probes) > 0 {
			if err := pStore.RecordProbeResults(hb.NodeID, hb.Probes); err != nil {
				log.Printf("probe persist failed for node %s: %v", hb.NodeID, err)
			}
		}
	}

	// Evaluate auto-heal remediation commands
	commands := pStore.EvaluateAutoHeal(&hb)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(protocol.HeartbeatResponse{
		Acknowledged: true,
		Commands:     commands,
	})
}

// handleAutohealLog validates an agent's autoheal log batch and persists it
// via pStore.RecordAutoHealLogs. Handler body lives in file scope so the
// schema-drift failure path can be unit-tested without the full mux.
// Mirrors the handleHeartbeatIngest extraction pattern.
func handleAutohealLog(w http.ResponseWriter, r *http.Request, pStore *store.PersistentStore) {
	token := r.Header.Get("X-NodePulse-Token")
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	uid, _, err := pStore.GetUserByToken(token)
	if err != nil && !pStore.IsMasterToken(token) {
		http.Error(w, `{"error":"unauthorized node token"}`, http.StatusUnauthorized)
		return
	}

	var payload struct {
		NodeID string                 `json:"node_id"`
		Events []protocol.AutoHealLog `json:"events"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.NodeID == "" {
		http.Error(w, `{"error":"invalid payload"}`, http.StatusBadRequest)
		return
	}

	// RecordAutoHealLogs was made error-returning in b6842be; failure
	// here MUST surface as 500, not a silent swallowed error. The same
	// silent-p.db.Exec class that masked the 2026-09-13 ~19:30 UTC prod
	// incident has been closed across all known pkg/store sites; this
	// handler preserves the contract.
	if err := pStore.RecordAutoHealLogs(payload.NodeID, uid, payload.Events); err != nil {
		log.Printf("[autoheal] record logs node=%q uid=%d failed: %v", payload.NodeID, uid, err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"accepted": true})
}
