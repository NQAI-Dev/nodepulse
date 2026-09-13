package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/NQAI-Dev/nodepulse/pkg/store"
)

// inviteCreateRequest is the JSON body for POST /api/v1/invites.
// All fields optional — an empty body mints a permanent invite that
// auto-creates a user on redemption (operator-friendly default).
type inviteCreateRequest struct {
	TargetUserID    int64  `json:"target_user_id"`
	AutoCreateUser  bool   `json:"auto_create_user"`
	DefaultUsername string `json:"default_username"`
	ExpiresInHours  int    `json:"expires_in_hours"`
}

// inviteRedeemRequest is the JSON body for POST /api/v1/invites/redeem.
// The chat_id is the Telegram chat that clicked the deep-link; the
// server stamps it onto the invite row so audits can answer "which chat
// redeemed this token" without joining against api_tokens or users.
type inviteRedeemRequest struct {
	Token  string `json:"token"`
	ChatID int64  `json:"chat_id"`
}

// handleInviteCreate is the master-token-gated endpoint that mints new
// invite tokens. Operator usage:
//
//	curl -X POST -H "Authorization: Bearer $MASTER" \
//	     https://pulse.nqai.es-cloud.ru/api/v1/invites \
//	     -d '{"target_user_id": 7, "expires_in_hours": 168}'
//
// Returns the raw token plus a deep-link ready for sharing in a Telegram
// message. Token is high-entropy (128 bits) and one-time-use; if the
// caller wants audit-grade pre-issuance, they can also pass
// `default_username` which gets recorded for the redeemer's eventual
// user row.
func handleInviteCreate(pStore *store.PersistentStore, w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		tok = r.URL.Query().Get("token")
	}
	if !pStore.IsMasterToken(tok) {
		http.Error(w, `{"error":"master token required"}`, http.StatusUnauthorized)
		return
	}

	var req inviteCreateRequest
	// Body is optional — an empty body is the operator-friendly default
	// ("mint me a permanent, auto-create-on-redeem invite"). EOF means
	// the caller sent Content-Length: 0, which is a valid empty body.
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
			http.Error(w, fmt.Sprintf(`{"error":"invalid body: %s"}`, err.Error()), http.StatusBadRequest)
			return
		}
	}

	// 32 hex chars of randomness — 128 bits of entropy, well past the
	// threshold where guessing is computationally infeasible. Same prefix
	// as our other token namespaces ("np_") so leaked secrets are easy
	// to spot in log scrapers.
	var rb [16]byte
	if _, err := rand.Read(rb[:]); err != nil {
		http.Error(w, `{"error":"rng failure"}`, http.StatusInternalServerError)
		return
	}
	token := "np_inv_" + hex.EncodeToString(rb[:])

	var expiresAt int64
	if req.ExpiresInHours > 0 {
		expiresAt = time.Now().Add(time.Duration(req.ExpiresInHours) * time.Hour).Unix()
	}

	inv, err := pStore.CreateInvite(store.Invite{
		Token:           token,
		CreatedBy:       1, // master operator
		TargetUserID:    req.TargetUserID,
		AutoCreateUser:  req.AutoCreateUser,
		DefaultUsername: req.DefaultUsername,
		ExpiresAt:       expiresAt,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	log.Printf("[invites] minted token=%s target_uid=%d auto_create=%v expires_at=%d",
		token, req.TargetUserID, req.AutoCreateUser, expiresAt)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token":            token,
		"deep_link":        "https://t.me/nodepulse_mon_bot?start=" + token,
		"target_user_id":   inv.TargetUserID,
		"auto_create_user": inv.AutoCreateUser,
		"expires_at":       inv.ExpiresAt,
	})
}

// inviteListEntry is the masked token shape returned by GET /api/v1/invites.
// The raw token never leaves the create endpoint; the list view shows
// only the first 12 chars + ellipsis so operators can eyeball which token
// is which without leaking the secret into logs.
type inviteListEntry struct {
	TokenMasked      string `json:"token_masked"`
	CreatedBy        int64  `json:"created_by"`
	TargetUserID     int64  `json:"target_user_id"`
	AutoCreateUser   bool   `json:"auto_create_user"`
	DefaultUsername  string `json:"default_username"`
	CreatedAt        int64  `json:"created_at"`
	ExpiresAt        int64  `json:"expires_at"`
	RedeemedAt       int64  `json:"redeemed_at"`
	RedeemedByChatID int64  `json:"redeemed_by_chat_id"`
}

// handleInviteList is the operator listing endpoint. Most-recent first,
// capped at 1000 entries so a runaway loop can't OOM the server.
func handleInviteList(pStore *store.PersistentStore, w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		tok = r.URL.Query().Get("token")
	}
	if !pStore.IsMasterToken(tok) {
		http.Error(w, `{"error":"master token required"}`, http.StatusUnauthorized)
		return
	}
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	invites, err := pStore.ListInvites(limit)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	out := make([]inviteListEntry, 0, len(invites))
	for _, inv := range invites {
		t := inv.Token
		if len(t) > 12 {
			t = t[:12] + "…"
		}
		out = append(out, inviteListEntry{
			TokenMasked:      t,
			CreatedBy:        inv.CreatedBy,
			TargetUserID:     inv.TargetUserID,
			AutoCreateUser:   inv.AutoCreateUser,
			DefaultUsername:  inv.DefaultUsername,
			CreatedAt:        inv.CreatedAt,
			ExpiresAt:        inv.ExpiresAt,
			RedeemedAt:       inv.RedeemedAt,
			RedeemedByChatID: inv.RedeemedByChatID,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"invites": out})
}

// handleInviteRedeem is the bot-side endpoint that atomically marks an
// invite as redeemed and returns the binding information the bot needs to
// know which user/tenant the chat belongs to. The invite token itself is
// the credential — 128 bits of randomness + one-time-use semantics means
// "knowledge of the token == authorization to redeem it", same model as
// Telegram's deep-link `start` payloads.
//
// HTTP status mapping (for the bot's error-handling path):
//
//	200 + {ok: true, target_user_id, ...}  → first successful redeem
//	400 + {error: "token required"}        → caller didn't send a token
//	404 + {error: "invite not found"}      → token never existed
//	409 + {error: "invite already redeemed", redeemed_by_chat_id}
//	                                       → race or replay attempt
//	410 + {error: "invite expired"}         → expires_at is in the past
//	500                                     → DB error
func handleInviteRedeem(pStore *store.PersistentStore, w http.ResponseWriter, r *http.Request) {
	var req inviteRedeemRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token == "" {
		http.Error(w, `{"error":"token required"}`, http.StatusBadRequest)
		return
	}

	inv, err := pStore.RedeemInvite(req.Token, req.ChatID)
	switch {
	case err == store.ErrInviteNotFound:
		http.Error(w, `{"error":"invite not found"}`, http.StatusNotFound)
		return
	case err == store.ErrInviteExpired:
		http.Error(w, `{"error":"invite expired"}`, http.StatusGone)
		return
	case err == store.ErrInviteAlreadyRedeemed:
		http.Error(w, fmt.Sprintf(`{"error":"invite already redeemed","redeemed_by_chat_id":%d}`,
			inv.RedeemedByChatID), http.StatusConflict)
		return
	case err != nil:
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
		return
	}
	log.Printf("[invites] redeemed token=%s chat_id=%d target_uid=%d",
		inv.Token, req.ChatID, inv.TargetUserID)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":               true,
		"target_user_id":   inv.TargetUserID,
		"auto_create_user": inv.AutoCreateUser,
		"default_username": inv.DefaultUsername,
		"redeemed_at":      inv.RedeemedAt,
	})
}
