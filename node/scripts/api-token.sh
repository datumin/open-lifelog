#!/usr/bin/env bash
# api-token.sh — mint an OAuth access token for local API/MCP testing by driving
# the full flow (DCR -> owner login -> consent -> code -> token) with curl. No
# DCR-capable client needed; the node speaks plain HTTP.
#
# Usage:
#   OLF_SECRET=<owner-secret> ./api-token.sh [BASE_URL] [CAPABILITY] [SURFACE]
#
#   BASE_URL    default http://127.0.0.1:8787
#   CAPABILITY  default '*:rw'   (e.g. 'meal:r,sleep:r,steps:r')
#   SURFACE     default api      (api | mcp)
#
# Prints the access token to stdout. Approves every permission the consent screen
# offers for the capability (you are the owner, testing your own node).
#
# Example:
#   export OLF_SECRET=...                      # shown once on first `olf serve`
#   TOKEN=$(./api-token.sh http://127.0.0.1:8787 'meal:r,sleep:r,steps:r')
#   curl -s -H "Authorization: Bearer $TOKEN" \
#     "http://127.0.0.1:8787/api/meal:r,sleep:r,steps:r/query/meal" | jq
set -euo pipefail

BASE="${1:-http://127.0.0.1:8787}"
CAP="${2:-*:rw}"
SURFACE="${3:-api}"
SECRET="${OLF_SECRET:?set OLF_SECRET to the owner secret printed on first 'olf serve'}"
REDIRECT="http://app.test/cb"
RESOURCE="$BASE/$SURFACE/$CAP"

jar=$(mktemp); trap 'rm -f "$jar"' EXIT

# PKCE (S256)
verifier="test-verifier-$(date +%s)-0000000000000000000000000000"
challenge=$(printf '%s' "$verifier" | openssl dgst -binary -sha256 | openssl base64 | tr '+/' '-_' | tr -d '=')

# 1. Dynamic Client Registration -> client_id (public client, no secret)
client=$(curl -fsS -X POST "$BASE/register" -H 'content-type: application/json' \
  -d '{"client_name":"api-token.sh","redirect_uris":["'"$REDIRECT"'"]}' \
  | sed -E 's/.*"client_id":"([^"]+)".*/\1/')

# 2. owner login -> session cookie
curl -fsS -c "$jar" -X POST "$BASE/login" --data-urlencode "secret=$SECRET" -o /dev/null

# 3. consent page -> CSRF token + the list of offered permissions
consent=$(curl -fsS -b "$jar" --get "$BASE/authorize" \
  --data-urlencode "response_type=code" --data-urlencode "client_id=$client" \
  --data-urlencode "redirect_uri=$REDIRECT" --data-urlencode "code_challenge=$challenge" \
  --data-urlencode "code_challenge_method=S256" \
  --data-urlencode "scope=lifelog:read:* lifelog:write:*" \
  --data-urlencode "resource=$RESOURCE" --data-urlencode "state=s")
csrf=$(printf '%s' "$consent" | sed -nE 's/.*name="csrf_token" value="([^"]+)".*/\1/p' | head -1)

# 4. approve every permission the screen offered (capability already bounds it)
grant_args=()
while IFS= read -r g; do grant_args+=(--data-urlencode "grant=$g"); done < <(
  printf '%s' "$consent" | grep -oE 'name="grant" value="[^"]+"' | sed -E 's/.*value="([^"]+)"/\1/')
[ "${#grant_args[@]}" -gt 0 ] || { echo "consent offered no permissions for $RESOURCE" >&2; exit 1; }

loc=$(curl -fsS -b "$jar" -o /dev/null -D - -X POST "$BASE/authorize" \
  --data-urlencode "response_type=code" --data-urlencode "client_id=$client" \
  --data-urlencode "redirect_uri=$REDIRECT" --data-urlencode "code_challenge=$challenge" \
  --data-urlencode "code_challenge_method=S256" \
  --data-urlencode "scope=lifelog:read:* lifelog:write:*" \
  --data-urlencode "resource=$RESOURCE" --data-urlencode "state=s" \
  --data-urlencode "action=approve" --data-urlencode "csrf_token=$csrf" \
  "${grant_args[@]}" | sed -nE 's/^[Ll]ocation: (.*)/\1/p' | tr -d '\r')
code=$(printf '%s' "$loc" | sed -E 's/.*[?&]code=([^&]+).*/\1/')

# 5. exchange code -> access token
curl -fsS -X POST "$BASE/token" \
  --data-urlencode "grant_type=authorization_code" --data-urlencode "code=$code" \
  --data-urlencode "redirect_uri=$REDIRECT" --data-urlencode "client_id=$client" \
  --data-urlencode "code_verifier=$verifier" \
  | sed -E 's/.*"access_token":"([^"]+)".*/\1/'
