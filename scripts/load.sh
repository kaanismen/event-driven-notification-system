#!/usr/bin/env bash
# Load generator: posts BATCHES batches of SIZE notifications (mixed channels and
# priorities), then prints the queue depth and delivery counters from /metrics every
# few seconds until the queue is empty. Start the stack first (`make up`).
#
#   make load                      # 10 batches of 1000
#   BATCHES=3 SIZE=500 make load
#
# Point the app at the mock provider first, or this sends every message to the
# configured PROVIDER_URL (webhook.site has request quotas):
#   PROVIDER_URL=http://mockprovider:8081/ docker compose up -d app
# Needs bash, curl and awk.
set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8080}"
BATCHES="${BATCHES:-10}"
SIZE="${SIZE:-1000}"
INTERVAL="${INTERVAL:-2}"

if ((SIZE < 1 || SIZE > 1000)); then
	echo "SIZE must be between 1 and 1000 (the batch limit)" >&2
	exit 1
fi

BODY_FILE="$(mktemp)"
trap 'rm -f "$BODY_FILE"' EXIT

# One batch body, reused for every request. Channels rotate sms, email, push; priorities
# follow a 20/60/20 high/normal/low mix, so the weighted claim has something to do.
{
	printf '{"notifications":['
	for ((i = 0; i < SIZE; i++)); do
		case $((i % 3)) in
		0) item="\"channel\":\"sms\",\"recipient\":\"+905551234567\",\"content\":\"Load test $i\"" ;;
		1) item="\"channel\":\"email\",\"recipient\":\"load@example.com\",\"content\":\"Load test $i\"" ;;
		2) item="\"channel\":\"push\",\"recipient\":\"device-token-load\",\"content\":\"Load test $i\"" ;;
		esac
		case $((i % 5)) in
		0) prio=high ;;
		4) prio=low ;;
		*) prio=normal ;;
		esac
		((i > 0)) && printf ','
		printf '{%s,"priority":"%s"}' "$item" "$prio"
	done
	printf ']}'
} >"$BODY_FILE"

echo "== posting $BATCHES batches of $SIZE to $BASE_URL"
start=$(date +%s)
for ((b = 1; b <= BATCHES; b++)); do
	code="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE_URL/v1/batches" \
		-H 'Content-Type: application/json' -H "X-Request-ID: load-$start-$b" --data @"$BODY_FILE")"
	if [ "$code" != 202 ]; then
		echo "batch $b: HTTP $code" >&2
		exit 1
	fi
done
echo "accepted $((BATCHES * SIZE)) notifications in $(($(date +%s) - start)) s"

# sums the values of every series of one metric whose labels contain FILTER.
sum_metric() {
	awk -v name="$1" -v filter="${2:-}" '
		index($0, name) == 1 && (filter == "" || index($0, filter)) { s += $NF }
		END { printf "%d", s }' <<<"$METRICS"
}

echo "== watching $BASE_URL/metrics every ${INTERVAL}s (Ctrl+C to stop)"
printf '%6s %9s %11s %9s %7s %8s\n' "t(s)" "pending" "processing" "sent" "retry" "failed"
while :; do
	METRICS="$(curl -sS "$BASE_URL/metrics")"
	pending="$(sum_metric 'notifier_queue_depth{' 'status="pending"')"
	processing="$(sum_metric 'notifier_queue_depth{' 'status="processing"')"
	printf '%6d %9s %11s %9s %7s %8s\n' "$(($(date +%s) - start))" "$pending" "$processing" \
		"$(sum_metric 'notifier_deliveries_total{' 'outcome="sent"')" \
		"$(sum_metric 'notifier_deliveries_total{' 'outcome="retry"')" \
		"$(sum_metric 'notifier_deliveries_total{' 'outcome="failed"')"
	if [ "$pending" = 0 ] && [ "$processing" = 0 ]; then
		break
	fi
	sleep "$INTERVAL"
done

echo "== queue drained in $(($(date +%s) - start)) s; latency histograms:"
grep -E '^notifier_(delivery_latency|provider_request_duration)_seconds_(sum|count)' <<<"$METRICS" || true
