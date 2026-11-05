#!/usr/bin/env bash
# Smoke test: calls every API endpoint against a running stack and checks the status
# codes. Start the stack first (`make up`), then run `make smoke` or this script.
# Needs bash and curl only. VERBOSE=1 prints every response body.
set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8080}"
RUN_ID="smoke-$(date +%s)-$$"
BODY_FILE="$(mktemp)"
HEADER_FILE="$(mktemp)"
trap 'rm -f "$BODY_FILE" "$HEADER_FILE"' EXIT

checks=0

# check METHOD PATH WANT_STATUS [BODY] [extra curl args...]
check() {
	local method="$1" path="$2" want="$3" body="${4:-}"
	shift $(($# < 4 ? $# : 4))
	local args=(-sS -o "$BODY_FILE" -D "$HEADER_FILE" -w '%{http_code}' -X "$method"
		-H 'Content-Type: application/json' -H "X-Request-ID: $RUN_ID")
	if [ -n "$body" ]; then
		args+=(--data "$body")
	fi
	local got
	got="$(curl "${args[@]}" "$@" "$BASE_URL$path")"
	if [ "$got" != "$want" ]; then
		echo "FAIL $method $path: got $got, want $want" >&2
		cat "$BODY_FILE" >&2
		exit 1
	fi
	checks=$((checks + 1))
	printf 'ok   %-6s %-58s %s\n' "$method" "$path" "$got"
	if [ "${VERBOSE:-0}" = 1 ]; then
		cat "$BODY_FILE"
	fi
}

# field NAME prints the first string value of NAME in the last response body.
field() {
	sed -n 's/.*"'"$1"'":"\([^"]*\)".*/\1/p' "$BODY_FILE" | head -n 1
}

require_header() {
	if ! grep -qi "^$1" "$HEADER_FILE"; then
		echo "FAIL missing response header: $1" >&2
		exit 1
	fi
}

sms='{"channel":"sms","recipient":"+905551234567","content":"Your code is 1234","priority":"high"}'
email='{"channel":"email","recipient":"user@example.com","content":"Hello from the smoke test"}'
push='{"channel":"push","recipient":"device-token-abc","content":"Flash sale starts now","priority":"low"}'

echo "== health"
check GET /health 200
require_header "X-Request-Id: $RUN_ID"

echo "== create"
check POST /v1/notifications 202 "$sms"
id="$(field id)"
require_header "Location: /v1/notifications/$id"
check GET "/v1/notifications/$id" 200
check POST /v1/notifications 422 '{"channel":"sms","recipient":"12345","content":""}'
check POST /v1/notifications 400 '{"channel":"sms","recipient":"+905551234567","content":"hi","priorty":"high"}'
check POST /v1/notifications 400 '{"channel":'

echo "== idempotency"
key="$RUN_ID-key"
check POST /v1/notifications 202 "$email" -H "Idempotency-Key: $key"
first="$(field id)"
check POST /v1/notifications 200 "$email" -H "Idempotency-Key: $key"
require_header "Idempotent-Replayed: true"
if [ "$(field id)" != "$first" ]; then
	echo "FAIL replay returned a different notification" >&2
	exit 1
fi
check POST /v1/notifications 409 "$push" -H "Idempotency-Key: $key"

echo "== batch"
check POST /v1/batches 202 "{\"notifications\":[$sms,$email,$push]}" -H "Idempotency-Key: $RUN_ID-batch"
batch="$(field batchId)"
check POST /v1/batches 200 "{\"notifications\":[$sms,$email,$push]}" -H "Idempotency-Key: $RUN_ID-batch"
check GET "/v1/batches/$batch" 200
check POST /v1/batches 422 "{\"notifications\":[$sms,{\"channel\":\"fax\",\"recipient\":\"x\",\"content\":\"y\"}]}"
check POST /v1/batches 422 '{"notifications":[]}'
check GET "/v1/batches/00000000-0000-0000-0000-000000000000" 404

echo "== cancel"
check POST "/v1/notifications/$id/cancel" 200
check POST "/v1/notifications/$id/cancel" 200
check POST "/v1/notifications/00000000-0000-0000-0000-000000000000/cancel" 404

echo "== list"
check GET "/v1/notifications?limit=2" 200
cursor="$(field nextCursor)"
if [ -z "$cursor" ]; then
	echo "FAIL expected a nextCursor with more than 2 notifications stored" >&2
	exit 1
fi
check GET "/v1/notifications?limit=2&cursor=$cursor" 200
check GET "/v1/notifications?status=cancelled&channel=sms" 200
check GET "/v1/notifications?from=2026-01-01T00:00:00Z&to=2099-01-01T00:00:00Z" 200
check GET "/v1/notifications?stauts=failed" 400
check GET "/v1/notifications?limit=500" 400
check GET "/v1/notifications/not-a-uuid" 400

echo "all $checks checks passed against $BASE_URL"
