#!/usr/bin/env bash
# OpenGate Cloud demo: mock provider + mock Stripe + OpenGate in cloud mode.
# Signs up a demo founder (free plan) and sends traffic up to the quota.
# Then open the dashboard, go to "Plan & billing" and upgrade through the
# mock Checkout page; the mock delivers signed webhooks back to OpenGate.
#   ./scripts/cloud-demo.sh          # leave servers running (Ctrl-C to stop)
#   KEEP=0 ./scripts/cloud-demo.sh   # set up and exit
set -euo pipefail
cd "$(dirname "$0")/.."
BIN=./bin/opengate
[ -x "$BIN" ] || make build
GW=http://127.0.0.1:8080
export OPENGATE_ADMIN_TOKEN=${OPENGATE_ADMIN_TOKEN:-demo-admin-token-change-me}
export OPENGATE_MASTER_KEY=${OPENGATE_MASTER_KEY:-$($BIN keygen 2>/dev/null | sed -n 's/^OPENGATE_MASTER_KEY=//p')}
# Fake test-mode secrets shared by OpenGate and the mock (never real values).
export STRIPE_API_KEY=sk_test_mock_cloud_demo
export STRIPE_WEBHOOK_SECRET=whsec_mock_cloud_demo
rm -f cloud-demo.db cloud-demo.db-wal cloud-demo.db-shm

$BIN mock-provider -listen 127.0.0.1:9090 -jitter 100ms >/tmp/opengate-mock.log 2>&1 &
MOCK=$!
$BIN mock-stripe -listen 127.0.0.1:12111 -webhook $GW/cloud/stripe/webhook >/tmp/opengate-mock-stripe.log 2>&1 &
STRIPE=$!
$BIN serve -config examples/cloud-demo.yaml >/tmp/opengate-cloud.log 2>&1 &
GATE=$!
cleanup() { kill $MOCK $STRIPE $GATE 2>/dev/null || true; }
trap cleanup EXIT
for _ in $(seq 50); do curl -fs $GW/healthz >/dev/null && break; sleep 0.1; done

JAR=$(mktemp)
json() { python3 -c "import json,sys; print(json.load(sys.stdin)$1)"; }
user() { curl -fsS -b "$JAR" -c "$JAR" -X "$1" "$GW/admin/api$2" -H 'X-Requested-With: opengate' -H 'Content-Type: application/json' ${3:+-d "$3"}; }

echo "== sign up (self-serve, free plan)"
user POST /auth/signup '{"email":"demo@example.com","name":"Demo User","password":"demo-only-password","org_name":"Acme AI"}' >/dev/null
user POST /provider-keys '{"provider":"openai","label":"BYOK","key":"sk-demo-openai-0000"}' >/dev/null
KEY=$(user POST /keys '{"name":"acme-app","agent":"assistant"}' | json "['key']")

echo "== traffic until the free quota (50 requests) runs out"
for i in $(seq 1 52); do
  code=$(curl -s -o /dev/null -w '%{http_code}' $GW/v1/chat/completions -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
    -d "{\"model\":\"openai/gpt-4o-mini\",\"messages\":[{\"role\":\"user\",\"content\":\"hello $i\"}]}")
  printf '%s ' "$code"
done; echo

echo
echo "Dashboard: $GW/ui/   login: demo@example.com / demo-only-password  ->  Plan & billing  ->  Upgrade to Team"
echo "Mock Stripe: http://127.0.0.1:12111 (test mode, no real charges)   Virtual key: $KEY"
rm -f "$JAR"
if [ "${KEEP:-1}" = 1 ]; then echo "Servers running; Ctrl-C to stop."; wait $GATE; fi
