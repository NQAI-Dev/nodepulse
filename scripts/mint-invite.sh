#!/usr/bin/env bash
#
# mint-invite.sh — operator one-liner for minting NodePulse invite tokens.
#
# Wraps POST /api/v1/invites so the operator never has to hand-craft
# curl + parse JSON. Prints the deep-link ready to paste into Telegram.
#
# Usage:
#   ./scripts/mint-invite.sh                                    # permanent, auto-create-on-redeem
#   ./scripts/mint-invite.sh --target-uid 7 --expires-hours 1   # bind chat to uid=7, expires in 1h
#   ./scripts/mint-invite.sh --auto-create --username newuser   # auto-create new tenant user
#   ./scripts/mint-invite.sh --server https://staging.example.com --dry-run
#
# Auth: requires the master token. Read from (in priority order):
#   1. --master-token <token> flag
#   2. NODEPULSE_MASTER_TOKEN env var
#   3. /etc/nodepulse/nodepulse-server.env (operator's env file, mode 0600)
#
# Requirements: bash, curl, jq. Stdout is the deep-link; everything else
# goes to stderr so the script composes well (e.g. `$(./mint-invite.sh)`
# captures only the link).

set -euo pipefail

# --- defaults ---------------------------------------------------------------
SERVER="${NODEPULSE_API_BASE:-https://pulse.nqai.es-cloud.ru}"
MASTER_TOKEN=""
TARGET_UID=""
AUTO_CREATE=""
USERNAME=""
EXPIRES_HOURS=""
DRY_RUN=false

usage() {
  cat >&2 <<'EOF'
Usage: mint-invite.sh [options]

Options:
  --server URL           API base (default: $NODEPULSE_API_BASE or prod)
  --master-token TOKEN   Master token (default: env or /etc/nodepulse/nodepulse-server.env)
  --target-uid N         Bind redeemed chat to existing user N
  --auto-create          Auto-create a fresh user when the chat redeems
  --username NAME        Default username for the auto-created user
  --expires-hours N      Token expires in N hours (default: never)
  --dry-run              Print the curl invocation, don't actually call the server
  -h, --help             Show this help

Output: the Telegram deep-link on stdout. Logs go to stderr.
EOF
  exit "${1:-0}"
}

# --- arg parsing ------------------------------------------------------------
while [ $# -gt 0 ]; do
  case "$1" in
    --server)         SERVER="$2"; shift 2 ;;
    --master-token)   MASTER_TOKEN="$2"; shift 2 ;;
    --target-uid)     TARGET_UID="$2"; shift 2 ;;
    --auto-create)    AUTO_CREATE="true"; shift ;;
    --username)       USERNAME="$2"; shift 2 ;;
    --expires-hours)  EXPIRES_HOURS="$2"; shift 2 ;;
    --dry-run)        DRY_RUN=true; shift ;;
    -h|--help)        usage 0 ;;
    *)                echo "unknown flag: $1" >&2; usage 1 ;;
  esac
done

# --- master token discovery -------------------------------------------------
if [ -z "$MASTER_TOKEN" ]; then
  if [ -n "${NODEPULSE_MASTER_TOKEN:-}" ]; then
    MASTER_TOKEN="$NODEPULSE_MASTER_TOKEN"
  elif [ -r /etc/nodepulse/nodepulse-server.env ]; then
    # shellcheck disable=SC1091
    MASTER_TOKEN="$(. /etc/nodepulse/nodepulse-server.env && printf '%s' "${NODEPULSE_TOKEN:-}")"
  fi
fi

if [ -z "$MASTER_TOKEN" ] && [ "$DRY_RUN" = false ]; then
  echo "error: master token not found" >&2
  echo "  set NODEPULSE_MASTER_TOKEN, pass --master-token, or ensure /etc/nodepulse/nodepulse-server.env is readable" >&2
  exit 2
fi

# --- dependencies ------------------------------------------------------------
for cmd in curl; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "error: required command not found: $cmd" >&2
    exit 3
  fi
done

# --- build JSON body --------------------------------------------------------
# Open with `{`, append each field with a leading comma (except the first),
# close with `}`. Empty body → just `{}`.
build_body() {
  local body="{"
  local first=true

  append() {
    if [ "$first" = true ]; then
      body="$body$1"
      first=false
    else
      body="$body,$1"
    fi
  }

  if [ -n "$TARGET_UID" ]; then
    append "$(printf '"target_user_id":%s' "$TARGET_UID")"
  fi
  if [ -n "$AUTO_CREATE" ]; then
    append "$(printf '"auto_create_user":%s' "$AUTO_CREATE")"
  fi
  if [ -n "$USERNAME" ]; then
    append "$(printf '"default_username":"%s"' "$USERNAME")"
  fi
  if [ -n "$EXPIRES_HOURS" ]; then
    append "$(printf '"expires_in_hours":%s' "$EXPIRES_HOURS")"
  fi

  body="$body}"
  printf '%s' "$body"
}

BODY="$(build_body)"

# --- dry-run path ------------------------------------------------------------
if [ "$DRY_RUN" = true ]; then
  echo "[dry-run] would POST $SERVER/api/v1/invites with body: $BODY" >&2
  if [ -n "$MASTER_TOKEN" ]; then
    echo "[dry-run] auth: Authorization: Bearer ***redacted***" >&2
  else
    echo "[dry-run] auth: <none — would fail with 401>" >&2
  fi
  exit 0
fi

# --- actual call -------------------------------------------------------------
HTTP_FILE="$(mktemp)"
trap 'rm -f "$HTTP_FILE"' EXIT

# -g turns off globbing so brackets in the URL are literal
# -sS = silent but show errors
# -w '%{http_code}' writes just the status code to stdout of the subshell
HTTP_CODE="$(
  curl -g -sS -o "$HTTP_FILE" -w '%{http_code}' \
    -X POST \
    -H "Authorization: Bearer $MASTER_TOKEN" \
    -H "Content-Type: application/json" \
    --data "$BODY" \
    "$SERVER/api/v1/invites"
)" || {
  echo "error: curl failed (server unreachable?)" >&2
  exit 4
}

RESPONSE="$(cat "$HTTP_FILE")"

if [ "$HTTP_CODE" != "200" ]; then
  echo "error: server returned HTTP $HTTP_CODE" >&2
  echo "$RESPONSE" >&2
  exit 5
fi

# Parse the deep-link. Prefer jq; fall back to grep/sed for hosts without it.
if command -v jq >/dev/null 2>&1; then
  DEEP_LINK="$(printf '%s' "$RESPONSE" | jq -r '.deep_link // empty')"
  TOKEN="$(printf '%s' "$RESPONSE" | jq -r '.token // empty')"
  EXPIRES_AT="$(printf '%s' "$RESPONSE" | jq -r '.expires_at // 0')"
else
  # Crude fallback: extract "deep_link":"..." value. Sufficient for our
  # single-field server output but not a general JSON parser.
  DEEP_LINK="$(printf '%s' "$RESPONSE" | sed -n 's/.*"deep_link":"\([^"]*\)".*/\1/p')"
  TOKEN="$(printf '%s' "$RESPONSE" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
  EXPIRES_AT="$(printf '%s' "$RESPONSE" | sed -n 's/.*"expires_at":\([0-9]*\).*/\1/p')"
fi

if [ -z "$DEEP_LINK" ] || [ -z "$TOKEN" ]; then
  echo "error: server response missing deep_link or token" >&2
  echo "$RESPONSE" >&2
  exit 6
fi

# Friendly operator log to stderr.
if [ -n "${EXPIRES_AT:-}" ] && [ "$EXPIRES_AT" != "0" ]; then
  HUMAN_EXP="$(date -u -d "@$EXPIRES_AT" '+%Y-%m-%d %H:%M:%S UTC' 2>/dev/null || echo "$EXPIRES_AT")"
  echo "[mint-invite] token=$TOKEN expires=$HUMAN_EXP" >&2
else
  echo "[mint-invite] token=$TOKEN expires=never" >&2
fi

# Stdout is the deep-link — easy to capture, paste, or pipe.
printf '%s\n' "$DEEP_LINK"
