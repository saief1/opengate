#!/usr/bin/env bash
# Local demo: mock upstream + OpenGate + seeded traffic. No real API keys used.
#   ./scripts/demo.sh          # run demo, leave servers running (Ctrl-C to stop)
#   KEEP=0 ./scripts/demo.sh   # seed and exit (servers stopped)
set -euo pipefail
cd "$(dirname "$0")/.."
BIN=./bin/opengate
[ -x "$BIN" ] || make build
GW=http://127.0.0.1:8080
export OPENGATE_ADMIN_TOKEN=${OPENGATE_ADMIN_TOKEN:-demo-admin-token-change-me}
export OPENGATE_MASTER_KEY=${OPENGATE_MASTER_KEY:-$($BIN keygen 2>/dev/null | sed -n 's/^OPENGATE_MASTER_KEY=//p')}
rm -f demo.db demo.db-wal demo.db-shm

$BIN mock-provider -listen 127.0.0.1:9090 -jitter 250ms >/tmp/opengate-mock.log 2>&1 &
MOCK=$!
$BIN serve -config examples/demo.yaml >/tmp/opengate.log 2>&1 &
GATE=$!
cleanup() { kill $MOCK $GATE 2>/dev/null || true; }
trap cleanup EXIT
for _ in $(seq 50); do curl -fs $GW/healthz >/dev/null && break; sleep 0.1; done

json() { python3 -c "import json,sys; print(json.load(sys.stdin)$1)"; }
admin() { curl -fsS -X "$1" "$GW/admin/api$2" -H "Authorization: Bearer $OPENGATE_ADMIN_TOKEN" -H 'Content-Type: application/json' ${3:+-d "$3"}; }

echo "== provider keys (stored AES-GCM encrypted; fake values for the mock)"
for p in openai anthropic gemini openrouter; do admin POST /provider-keys "{\"provider\":\"$p\",\"label\":\"demo\",\"key\":\"sk-demo-$p-0000\"}" >/dev/null; done

echo "== projects & keys"
RESEARCH=$(admin POST /projects '{"name":"research-agents","log_bodies":true}' | json "['id']")
SUPPORT=$(admin POST /projects '{"name":"support-bot"}' | json "['id']")
key() { admin POST /keys "$1"; }
PLANNER=$(key "{\"project_id\":\"$RESEARCH\",\"name\":\"planner-prod\",\"agent\":\"planner\",\"limits\":{\"daily_usd\":5,\"monthly_usd\":60}}" | json "['key']")
SCRAPER_J=$(key "{\"project_id\":\"$RESEARCH\",\"name\":\"web-scraper\",\"agent\":\"scraper\",\"limits\":{\"rpm\":120}}")
SCRAPER=$(echo "$SCRAPER_J" | json "['key']"); SCRAPER_ID=$(echo "$SCRAPER_J" | json "['virtual_key']['id']")
TRIAGE=$(key "{\"project_id\":\"$SUPPORT\",\"name\":\"support-triage\",\"agent\":\"triage\",\"user\":\"helpdesk\"}" | json "['key']")
LOOPER=$(key "{\"project_id\":\"$SUPPORT\",\"name\":\"retry-loop-bot\",\"agent\":\"retry-loop\",\"limits\":{\"daily_usd\":0.01}}" | json "['key']")
admin PUT /limits "{\"scope\":\"project\",\"scope_id\":\"$SUPPORT\",\"daily_usd\":20,\"monthly_usd\":300}" >/dev/null
admin PUT /limits "{\"scope\":\"agent\",\"scope_id\":\"$RESEARCH/scraper\",\"daily_usd\":3}" >/dev/null

echo "== synthetic history (previous days only)"
$BIN demo-seed -config examples/demo.yaml -days 30 -per-day 90

chat() { # key model stream text
  curl -s -o /dev/null -w '%{http_code}' $GW/v1/chat/completions -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$2\",\"stream\":$3,\"messages\":[{\"role\":\"system\",\"content\":\"You are a helpful agent.\"},{\"role\":\"user\",\"content\":\"$4\"}]}"
}
echo "== live traffic through the gateway"
MODELS=(openai/gpt-4o-mini anthropic/claude-haiku-4-5 gemini/gemini-2.5-flash smart cheap openrouter/meta-llama/llama-3.3-70b-instruct openai/gpt-4o-flaky anthropic/claude-sonnet-4-5)
for i in $(seq 1 40); do
  k=$PLANNER; [ $((i % 3)) = 0 ] && k=$SCRAPER; [ $((i % 4)) = 0 ] && k=$TRIAGE
  m=${MODELS[$((i % ${#MODELS[@]}))]}; s=false; [ $((i % 2)) = 0 ] && s=true
  printf '%s ' "$(chat "$k" "$m" $s "Summarize research note number $i for the weekly report, please")"
done; echo
echo "== runaway agent hits its daily budget"
for i in $(seq 1 25); do printf '%s ' "$(chat "$LOOPER" anthropic/claude-sonnet-4-5 false "retry the same failing tool call again and again, attempt $i, with a lot of context to burn tokens")"; done; echo
echo "== kill switch on web-scraper"
admin POST /keys/$SCRAPER_ID/kill >/dev/null
for i in 1 2 3; do printf '%s ' "$(chat "$SCRAPER" openai/gpt-4o-mini false "scrape page $i")"; done; echo

echo
echo "Dashboard: $GW/ui/   admin token: $OPENGATE_ADMIN_TOKEN"
echo "Planner key (for curl / SDK tests): $PLANNER"
if [ "${KEEP:-1}" = 1 ]; then echo "Servers running; Ctrl-C to stop."; wait $GATE; fi
