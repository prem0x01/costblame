#!/usr/bin/env bash
# test-scenarios.sh — end-to-end scenario testing for costblame
#
# Usage:
#   ./scripts/test-scenarios.sh              # run all scenarios
#   ./scripts/test-scenarios.sh health       # single scenario
#   WEBHOOK_SECRET=mysecret ./scripts/test-scenarios.sh webhook
#
# Requires: curl, jq, sqlite3, openssl

set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:7890}"
WEBHOOK_SECRET="${WEBHOOK_SECRET:-changeme}"
DB_PATH="${DB_PATH:-}"   # leave blank to skip direct DB seeding

# ── helpers ───────────────────────────────────────────────────────────────────

GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[1;33m'; NC='\033[0m'
pass() { echo -e "${GREEN}✓${NC} $1"; }
fail() { echo -e "${RED}✗${NC} $1"; exit 1; }
info() { echo -e "${YELLOW}→${NC} $1"; }

# Compute GitHub-style HMAC-SHA256 webhook signature.
sign_payload() {
  local secret="$1" payload="$2"
  echo -n "$payload" | openssl dgst -sha256 -hmac "$secret" | awk '{print "sha256="$2}'
}

require() {
  command -v "$1" &>/dev/null || { echo "Required: $1 not found"; exit 1; }
}

require curl; require jq; require openssl

# ── scenario 1: health check ──────────────────────────────────────────────────
scenario_health() {
  info "Scenario 1: Health check"
  resp=$(curl -sf "$BASE_URL/healthz")
  [ "$resp" = "ok" ] && pass "GET /healthz → ok" || fail "health check failed: $resp"
}

# ── scenario 2: github webhook (deploy event ingestion) ───────────────────────
scenario_webhook() {
  info "Scenario 2: GitHub webhook — workflow_run completed on main"

  PAYLOAD=$(cat <<'EOF'
{
  "action": "completed",
  "workflow_run": {
    "id": 9001,
    "name": "Deploy to Production",
    "head_branch": "main",
    "head_sha": "abc123def456abc123def456abc123def456abc1",
    "status": "completed",
    "conclusion": "success",
    "updated_at": "2026-06-10T12:00:00Z",
    "pull_requests": [
      { "number": 42, "head": { "sha": "abc123def456abc123def456abc123def456abc1" } }
    ]
  },
  "repository": {
    "full_name": "myorg/payment-service",
    "name": "payment-service"
  },
  "sender": { "login": "alice" }
}
EOF
)

  SIG=$(sign_payload "$WEBHOOK_SECRET" "$PAYLOAD")

  STATUS=$(curl -s -o /dev/null -w "%{http_code}" \
    -X POST "$BASE_URL/webhooks/github" \
    -H "Content-Type: application/json" \
    -H "X-GitHub-Event: workflow_run" \
    -H "X-Hub-Signature-256: $SIG" \
    -d "$PAYLOAD")

  [ "$STATUS" = "202" ] && pass "POST /webhooks/github → 202 Accepted" \
                         || fail "webhook returned HTTP $STATUS (expected 202)"
}

# ── scenario 3: wrong signature (must be rejected) ────────────────────────────
scenario_bad_signature() {
  info "Scenario 3: Webhook with invalid signature (must be rejected)"

  PAYLOAD='{"action":"completed","workflow_run":{"status":"completed","conclusion":"success","head_branch":"main"}}'
  STATUS=$(curl -s -o /dev/null -w "%{http_code}" \
    -X POST "$BASE_URL/webhooks/github" \
    -H "Content-Type: application/json" \
    -H "X-GitHub-Event: workflow_run" \
    -H "X-Hub-Signature-256: sha256=badhash" \
    -d "$PAYLOAD")

  [ "$STATUS" = "401" ] && pass "Bad signature rejected with 401" \
                         || fail "expected 401, got $STATUS"
}

# ── scenario 4: non-deploy branch (must be ignored, still 202) ────────────────
scenario_non_deploy_branch() {
  info "Scenario 4: Webhook on feature branch (accepted but not stored as deploy)"

  PAYLOAD=$(cat <<'EOF'
{
  "action": "completed",
  "workflow_run": {
    "id": 9002,
    "head_branch": "feature/my-experiment",
    "head_sha": "deadbeef",
    "status": "completed",
    "conclusion": "success",
    "updated_at": "2026-06-10T12:01:00Z",
    "pull_requests": []
  },
  "repository": { "full_name": "myorg/payment-service" },
  "sender": { "login": "bob" }
}
EOF
)
  SIG=$(sign_payload "$WEBHOOK_SECRET" "$PAYLOAD")

  STATUS=$(curl -s -o /dev/null -w "%{http_code}" \
    -X POST "$BASE_URL/webhooks/github" \
    -H "Content-Type: application/json" \
    -H "X-GitHub-Event: workflow_run" \
    -H "X-Hub-Signature-256: $SIG" \
    -d "$PAYLOAD")

  [ "$STATUS" = "202" ] && pass "Feature branch webhook accepted (202) — filtered internally" \
                         || fail "expected 202, got $STATUS"
}

# ── scenario 5: seed a cost anomaly + query blame ─────────────────────────────
scenario_seed_anomaly() {
  info "Scenario 5: Seed cost anomaly directly into SQLite, then query /blame"

  if [ -z "$DB_PATH" ]; then
    echo "  Skipping — set DB_PATH to the costblame.db file path to run this scenario."
    echo "  Example: docker exec costblame-costblame-1 find /data -name '*.db'"
    return
  fi

  require sqlite3

  SNAP_ID=$(python3 -c "import uuid; print(uuid.uuid4())" 2>/dev/null \
            || cat /proc/sys/kernel/random/uuid)

  sqlite3 "$DB_PATH" <<SQL
INSERT OR IGNORE INTO cost_snapshots
  (id, collected_at, period_start, period_end, source, service, region,
   amount_usd, prev_amount_usd, delta_usd, delta_pct,
   is_anomaly, anomaly_score, granularity)
VALUES (
  '$SNAP_ID',
  datetime('now'),
  datetime('now', '-1 day'),
  datetime('now'),
  'aws', 'AWSLambda', 'us-east-1',
  380.0, 155.0, 225.0, 145.2,
  1, 3.8, 'DAILY'
);
SQL

  pass "Inserted cost anomaly for AWSLambda (+145%, \$225 delta) with id=$SNAP_ID"

  RESP=$(curl -sf "$BASE_URL/anomalies")
  COUNT=$(echo "$RESP" | jq 'length' 2>/dev/null || echo "?")
  pass "GET /anomalies → $COUNT anomaly(ies) returned"
}

# ── scenario 6: list blame edges ──────────────────────────────────────────────
scenario_list_blame() {
  info "Scenario 6: List blame edges via REST API"

  RESP=$(curl -sf "$BASE_URL/blame")
  echo "$RESP" | jq . 2>/dev/null && pass "GET /blame → valid JSON" \
                                  || { echo "$RESP"; fail "invalid JSON from /blame"; }
}

# ── scenario 7: ping event (GitHub webhook setup) ─────────────────────────────
scenario_ping() {
  info "Scenario 7: GitHub ping event (sent when webhook is first configured)"

  PAYLOAD='{"zen":"Practicality beats purity.","hook_id":123}'
  SIG=$(sign_payload "$WEBHOOK_SECRET" "$PAYLOAD")

  STATUS=$(curl -s -o /dev/null -w "%{http_code}" \
    -X POST "$BASE_URL/webhooks/github" \
    -H "Content-Type: application/json" \
    -H "X-GitHub-Event: ping" \
    -H "X-Hub-Signature-256: $SIG" \
    -d "$PAYLOAD")

  [ "$STATUS" = "200" ] && pass "Ping event → 200 OK" \
                         || fail "ping returned HTTP $STATUS"
}

# ── main ──────────────────────────────────────────────────────────────────────
SCENARIO="${1:-all}"

case "$SCENARIO" in
  health)           scenario_health ;;
  webhook)          scenario_webhook ;;
  bad_sig)          scenario_bad_signature ;;
  branch)           scenario_non_deploy_branch ;;
  anomaly)          scenario_seed_anomaly ;;
  blame)            scenario_list_blame ;;
  ping)             scenario_ping ;;
  all)
    scenario_health
    scenario_ping
    scenario_webhook
    scenario_bad_signature
    scenario_non_deploy_branch
    scenario_seed_anomaly
    scenario_list_blame
    echo ""
    echo -e "${GREEN}All scenarios completed.${NC}"
    ;;
  *)
    echo "Unknown scenario: $SCENARIO"
    echo "Available: health, webhook, bad_sig, branch, anomaly, blame, ping, all"
    exit 1
    ;;
esac
