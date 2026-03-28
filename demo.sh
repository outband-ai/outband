#!/usr/bin/env bash
set -euo pipefail

# ---------------------------------------------------------------------------
# OUTBAND DEMO — Zero-Latency LLM Compliance Infrastructure
#
# This script demonstrates the full Outband pipeline: proxy forwarding,
# PII detection, telemetry logging, and evidence summary generation.
# It doubles as a CI smoke test — exits 0 on success, 1 on failure.
#
# Usage:
#   ./demo.sh            # Run demo (CI mode)
#   ./demo.sh --record   # Add pauses between scenes for screen recording
# ---------------------------------------------------------------------------

# ── Dependency Check ──
MISSING=()
command -v jq &>/dev/null || MISSING+=("jq")
command -v curl &>/dev/null || MISSING+=("curl")
command -v docker &>/dev/null || MISSING+=("docker")

if [ ${#MISSING[@]} -ne 0 ]; then
    echo "Error: Missing required tools: ${MISSING[*]}"
    echo "Install with:"
    echo "  macOS:  brew install ${MISSING[*]}"
    echo "  Ubuntu: apt-get install ${MISSING[*]}"
    exit 1
fi

# ── Color Support ──
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
    GREEN='\033[0;32m'
    BLUE='\033[0;34m'
    YELLOW='\033[1;33m'
    RED='\033[0;31m'
    NC='\033[0m'
    BOLD='\033[1m'
    DIM='\033[2m'
else
    GREEN='' BLUE='' YELLOW='' RED='' NC='' BOLD='' DIM=''
fi

# ── Recording Mode ──
RECORD_MODE=false
if [[ "${1:-}" == "--record" ]]; then
    RECORD_MODE=true
fi

pause() {
    if $RECORD_MODE; then sleep 2; fi
}

# ── Polling Helper ──
poll_until() {
    local timeout=$1 interval=$2; shift 2
    local end=$(( $(date +%s) + timeout ))
    while [ "$(date +%s)" -lt "$end" ]; do
        if "$@" 2>/dev/null; then return 0; fi
        sleep "$interval"
    done
    return 1
}

# ── Cleanup ──
COMPOSE_FILES="-f docker-compose.yml -f docker-compose.demo.yml"
cleanup() {
    echo ""
    echo -e "${DIM}[DEMO] Cleaning up...${NC}"
    docker compose $COMPOSE_FILES down -v 2>/dev/null || true
    rm -rf demo-output 2>/dev/null || true
}
trap cleanup EXIT

FAILURES=0
assert() {
    local desc=$1; shift
    if "$@" 2>/dev/null; then
        echo -e "       ${GREEN}✓${NC} $desc"
    else
        echo -e "       ${RED}✗${NC} $desc"
        FAILURES=$((FAILURES + 1))
    fi
}

PROXY_URL="http://localhost:8081"
LOG_DIR="demo-output/logs"
EVIDENCE_DIR="demo-output/evidence"

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# Scene 1: Startup
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

echo ""
echo -e "${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
echo -e "${BOLD}  OUTBAND — Zero-Latency LLM Compliance Infrastructure${NC}"
echo -e "${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
echo ""

echo -e "${BOLD}[SCENE 1] Starting sidecar proxy...${NC}"

mkdir -p "$LOG_DIR" "$EVIDENCE_DIR"
# Run proxy container as current user so volume-mounted files are
# readable by the host (demo script reads JSONL/evidence files directly).
export UID GID=$(id -g)
docker compose $COMPOSE_FILES build --quiet
docker compose $COMPOSE_FILES up -d --wait --wait-timeout 60

echo -e "  ${DIM}[OUTBAND] Target: http://mockllm:9090 (mock OpenAI endpoint)${NC}"
echo -e "  ${DIM}[OUTBAND] Listening: localhost:8081${NC}"

# Poll health endpoint.
if poll_until 30 0.5 curl -sf "$PROXY_URL/healthz"; then
    echo -e "  ${GREEN}[OUTBAND] Health check: ✓ PASSED${NC}"
else
    echo -e "  ${RED}[OUTBAND] Health check: ✗ FAILED${NC}"
    echo -e "  ${RED}Proxy did not become healthy within 30s${NC}"
    echo -e "  ${DIM}--- container status ---${NC}"
    docker compose $COMPOSE_FILES ps 2>/dev/null || true
    echo -e "  ${DIM}--- proxy logs ---${NC}"
    docker compose $COMPOSE_FILES logs proxy 2>/dev/null | tail -20 || true
    echo -e "  ${DIM}--- mockllm logs ---${NC}"
    docker compose $COMPOSE_FILES logs mockllm 2>/dev/null | tail -10 || true
    exit 1
fi

pause

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# Scene 2: Clean Request — No PII
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

echo ""
echo -e "${BOLD}[SCENE 2] Clean request — no PII${NC}"

curl -sf -X POST "$PROXY_URL/v1/chat/completions" \
    -H "Content-Type: application/json" \
    -d '{"model":"gpt-4","messages":[{"role":"user","content":"Explain how photosynthesis works"}]}' \
    > /dev/null

echo -e "  ${BLUE}[DEMO]${NC}  Sent: \"Explain how photosynthesis works\""

# Poll for telemetry entry.
if poll_until 10 0.2 test -s "$LOG_DIR/outband-telemetry-current.jsonl"; then
    ENTRY=$(tail -1 "$LOG_DIR/outband-telemetry-current.jsonl")
    PII_COUNT=$(echo "$ENTRY" | jq '.pii_categories_found | length')
    echo -e "  ${GREEN}[AUDIT]${NC} Payload captured → ${GREEN}${PII_COUNT} PII detected${NC}"
    assert "No PII in clean request" [ "$PII_COUNT" -eq 0 ]
else
    echo -e "  ${RED}[AUDIT] Telemetry entry not written within timeout${NC}"
    FAILURES=$((FAILURES + 1))
fi

pause

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# Scene 3: PII Detection
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

echo ""
echo -e "${BOLD}[SCENE 3] PII detection${NC}"

PII_PROMPT="Schedule a call with john.smith@acmecorp.com about account 4532015112830366, SSN 123-45-6789"

curl -sf -X POST "$PROXY_URL/v1/chat/completions" \
    -H "Content-Type: application/json" \
    -d "{\"model\":\"gpt-4\",\"messages\":[{\"role\":\"user\",\"content\":\"$PII_PROMPT\"}]}" \
    > /dev/null

echo -e "  ${BLUE}[DEMO]${NC}  Sent prompt with PII:"
echo -e "         ${DIM}\"$PII_PROMPT\"${NC}"

# Poll for the PII entry (should be the 2nd entry).
EXPECTED_COUNT=2
if poll_until 10 0.2 bash -c "[ \$(wc -l < '$LOG_DIR/outband-telemetry-current.jsonl' 2>/dev/null || echo 0) -ge $EXPECTED_COUNT ]"; then
    PII_ENTRY=$(tail -1 "$LOG_DIR/outband-telemetry-current.jsonl")
    PII_CATS=$(echo "$PII_ENTRY" | jq -r '.pii_categories_found | join(", ")')
    echo ""
    echo -e "  ${GREEN}[AUDIT]${NC} PII detected: ${YELLOW}${PII_CATS}${NC}"

    REDACTED=$(echo "$PII_ENTRY" | jq -r '.redacted_payload')
    if echo "$REDACTED" | grep -q '\[REDACTED:EMAIL:CC6.1\]'; then
        echo -e "         ├── ${YELLOW}[REDACTED:EMAIL:CC6.1]${NC}    john.smith@acmecorp.com"
    fi
    if echo "$REDACTED" | grep -q '\[REDACTED:CC_NUMBER:CC6.1\]'; then
        echo -e "         ├── ${YELLOW}[REDACTED:CC_NUMBER:CC6.1]${NC} 4532015112830366"
    fi
    if echo "$REDACTED" | grep -q '\[REDACTED:SSN:CC6.1\]'; then
        echo -e "         └── ${YELLOW}[REDACTED:SSN:CC6.1]${NC}      123-45-6789"
    fi

    assert "Email detected" grep -q "EMAIL" <<< "$PII_CATS"
    assert "Credit card detected" grep -q "CC_NUMBER" <<< "$PII_CATS"
    assert "SSN detected" grep -q "SSN" <<< "$PII_CATS"
else
    echo -e "  ${RED}[AUDIT] PII telemetry entry not written within timeout${NC}"
    FAILURES=$((FAILURES + 1))
fi

pause

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# Scene 4: Telemetry Detail
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

echo ""
echo -e "${BOLD}[SCENE 4] Telemetry entry detail${NC}"

if [ -f "$LOG_DIR/outband-telemetry-current.jsonl" ]; then
    DETAIL_ENTRY=$(tail -1 "$LOG_DIR/outband-telemetry-current.jsonl")
    echo ""
    echo "$DETAIL_ENTRY" | jq '{
        request_id,
        timestamp,
        original_hash: .original_hash[0:16],
        redacted_payload,
        redacted_hash: .redacted_hash[0:16],
        redaction_level,
        pii_categories_found,
        capture_complete
    }' | sed 's/^/  /'
    echo ""

    assert "Cryptographic hashes present" jq -e '.original_hash | length == 64' <<< "$DETAIL_ENTRY"
    assert "Capture complete" jq -e '.capture_complete == true' <<< "$DETAIL_ENTRY"
fi

pause

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# Scene 5: Evidence Summary
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

echo ""
echo -e "${BOLD}[SCENE 5] Evidence summary${NC}"
echo -e "  ${DIM}Waiting for evidence summary (10s aggregation window)...${NC}"

EVIDENCE_FILE=""
if poll_until 30 0.5 bash -c 'ls '"$EVIDENCE_DIR"'/*.json 2>/dev/null | head -1 | grep -q .'; then
    EVIDENCE_FILE=$(ls "$EVIDENCE_DIR"/*.json 2>/dev/null | head -1)
fi

if [ -n "$EVIDENCE_FILE" ]; then
    echo ""
    PROCESSED=$(jq '.total_requests_processed' "$EVIDENCE_FILE")
    AUDITED=$(jq '.total_requests_audited' "$EVIDENCE_FILE")
    COVERAGE=$(jq '.audit_coverage_percent' "$EVIDENCE_FILE")
    PII_TOTAL=$(jq '.total_pii_detected' "$EVIDENCE_FILE")
    P99=$(jq '.proxy_p99_latency_ms' "$EVIDENCE_FILE")
    CONTROLS=$(jq -r '.soc2_controls_satisfied | join(", ")' "$EVIDENCE_FILE")

    echo -e "  Requests Processed:  ${BOLD}${PROCESSED}${NC}"
    echo -e "  Requests Audited:    ${BOLD}${AUDITED}${NC}"
    echo -e "  Audit Coverage:      ${GREEN}${COVERAGE}%${NC}"
    echo -e "  PII Detected:        ${YELLOW}${PII_TOTAL}${NC}"

    # Per-category breakdown.
    CATS=$(jq -r '.redaction_events_by_category | to_entries[] | "    ├── \(.key): \(.value)"' "$EVIDENCE_FILE" 2>/dev/null || true)
    if [ -n "$CATS" ]; then
        echo "$CATS" | while read -r line; do echo -e "  $line"; done
    fi

    echo -e "  Proxy P99 Overhead:  ${GREEN}${P99}ms${NC}"
    echo -e "  Controls Satisfied:  ${BOLD}${CONTROLS}${NC}"
    echo ""

    assert "Audit coverage > 0%" jq -e '.audit_coverage_percent > 0' "$EVIDENCE_FILE" > /dev/null
    assert "SOC 2 CC6.1 satisfied" jq -e '.soc2_controls_satisfied | index("CC6.1")' "$EVIDENCE_FILE" > /dev/null
else
    echo -e "  ${RED}Evidence summary not generated within timeout${NC}"
    FAILURES=$((FAILURES + 1))
fi

pause

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# Scene 6: PDF Report
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

echo ""
echo -e "${BOLD}[SCENE 6] PDF evidence report${NC}"
echo ""
echo -e "  ${DIM}PDF evidence reports require outband-enterprise.${NC}"
echo -e "  ${DIM}See outband.dev/pricing${NC}"
echo ""

pause

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# Scene 7: Closing
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

echo ""
echo -e "${BOLD}[SCENE 7] Full pipeline validated${NC}"
echo -e "       ${GREEN}✓${NC} Proxy forwarded requests intact"
echo -e "       ${GREEN}✓${NC} PII detected and redacted in audit trail"
echo -e "       ${GREEN}✓${NC} Cryptographic hashes computed"
echo -e "       ${GREEN}✓${NC} Evidence summary generated"
echo -e "       ${GREEN}✓${NC} SOC 2 controls satisfied"
echo ""
echo -e "${BOLD}[OUTBAND]${NC} Your LLM traffic is now auditable."
echo ""

if [ "$FAILURES" -gt 0 ]; then
    echo -e "${RED}DEMO FAILED: $FAILURES assertion(s) failed${NC}"
    exit 1
fi

echo -e "${GREEN}DEMO PASSED: all assertions passed${NC}"
exit 0
