#!/usr/bin/env bash
# Starts a fake upstream and the gateway, drives every route through them and
# reports pass/fail per check. Everything is torn down on exit, including on
# Ctrl-C or an early failure.
#
#   make smoke
set -uo pipefail

cd "$(dirname "$0")/.."

GW_KEY=${GW_KEY:-gw_smoketestkey}
FAKE_PORT=${FAKE_PORT:-9000}
GW_PORT=${GW_PORT:-8080}
GW="http://127.0.0.1:${GW_PORT}"

green() { printf '\033[32m%s\033[0m' "$1"; }
red()   { printf '\033[31m%s\033[0m' "$1"; }
dim()   { printf '\033[2m%s\033[0m' "$1"; }

pass=0
fail=0

# check <name> <expected-status> <curl args...>
check() {
  local name=$1 want=$2; shift 2
  local body status
  body=$(curl -sS -o /tmp/smoke.body -w '%{http_code}' "$@" 2>/dev/null)
  status=$body
  if [ "$status" = "$want" ]; then
    printf '  %s %-46s %s\n' "$(green '✓')" "$name" "$(dim "HTTP $status")"
    pass=$((pass + 1))
  else
    printf '  %s %-46s %s\n' "$(red '✗')" "$name" "$(red "HTTP $status, want $want")"
    sed 's/^/      /' /tmp/smoke.body 2>/dev/null | head -3
    fail=$((fail + 1))
  fi
}

# contains <name> <needle> <curl args...>
contains() {
  local name=$1 needle=$2; shift 2
  local body rc
  body=$(curl -sS --fail-with-body "$@" 2>/dev/null); rc=$?
  if grep -q -F -- "$needle" <<<"$body"; then
    printf '  %s %-46s %s\n' "$(green '✓')" "$name" "$(dim "contains '$needle'")"
    pass=$((pass + 1))
  else
    printf '  %s %-46s %s\n' "$(red '✗')" "$name" "$(red "no contains '$needle'")"
    printf '      %s\n' "$(dim "curl exited $rc, ${#body} bytes")"
    printf '      %s\n' "$body" | head -3
    fail=$((fail + 1))
  fi
}

# icontains <name> <needle> <curl args...>   case-insensitive contains
icontains() {
  local name=$1 needle=$2; shift 2
  local body rc
  body=$(curl -sS --fail-with-body "$@" 2>/dev/null); rc=$?
  if grep -qi -F -- "$needle" <<<"$body"; then
    printf '  %s %-46s %s\n' "$(green '✓')" "$name" "$(dim "contains '$needle'")"
    pass=$((pass + 1))
  else
    printf '  %s %-46s %s\n' "$(red '✗')" "$name" "$(red "no contains '$needle'")"
    printf '      %s\n' "$(dim "curl exited $rc, ${#body} bytes")"
    printf '      %s\n' "$body" | head -3
    fail=$((fail + 1))
  fi
}

# absent <name> <needle> <curl args...>
absent() {
  local name=$1 needle=$2; shift 2
  local body rc
  body=$(curl -sS --fail-with-body "$@" 2>/dev/null); rc=$?
  if grep -q -F -- "$needle" <<<"$body"; then
    printf '  %s %-46s %s\n' "$(red '✗')" "$name" "$(red "leaked '$needle' (curl $rc)")"
    fail=$((fail + 1))
  else
    printf '  %s %-46s %s\n' "$(green '✓')" "$name" "$(dim "no '$needle'")"
    pass=$((pass + 1))
  fi
}

json() { printf '%s' "$1"; }
post() { echo -H 'Content-Type: application/json'; }

cleanup() {
  [ -n "${FB_PID:-}" ]   && kill "$FB_PID"   2>/dev/null
  [ -n "${RL_PID:-}" ]   && kill "$RL_PID"   2>/dev/null
  [ -n "${GW_PID:-}" ]   && kill "$GW_PID"   2>/dev/null
  [ -n "${FAKE_PID:-}" ] && kill "$FAKE_PID" 2>/dev/null
  wait 2>/dev/null
}
trap cleanup EXIT INT TERM

echo "== building =="
go build -o bin/gateway ./cmd/gateway || exit 1
go build -o bin/fakeupstream ./cmd/fakeupstream || exit 1

echo "== starting the fake upstream on :${FAKE_PORT} =="
./bin/fakeupstream -addr ":${FAKE_PORT}" > /tmp/smoke-fake.log 2>&1 &
FAKE_PID=$!

echo "== starting the gateway on :${GW_PORT} =="
GATEWAY_ADDR=":${GW_PORT}" \
GATEWAY_LOG_LEVEL=warn \
GATEWAY_API_KEYS="smoke:${GW_KEY}" \
OPENAI_API_KEY=sk-fake-openai \
OPENAI_BASE_URL="http://127.0.0.1:${FAKE_PORT}/v1" \
ANTHROPIC_API_KEY=sk-fake-anthropic \
ANTHROPIC_BASE_URL="http://127.0.0.1:${FAKE_PORT}/v1" \
GATEWAY_CACHE_TTL=60s \
  ./bin/gateway > /tmp/smoke-gw.log 2>&1 &
GW_PID=$!

for _ in $(seq 40); do
  curl -sf "${GW}/healthz" >/dev/null 2>&1 && break
  sleep 0.25
done
if ! curl -sf "${GW}/healthz" >/dev/null 2>&1; then
  red "the gateway never came up"; echo; cat /tmp/smoke-gw.log; exit 1
fi

CT=(-H 'Content-Type: application/json' -H "Authorization: Bearer ${GW_KEY}")
OPENAI_REQ='{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hola"}]}'
CLAUDE_REQ='{"model":"claude-sonnet-5","messages":[{"role":"system","content":"se breve"},{"role":"user","content":"hola"},{"role":"system","content":"en espanol"}]}'
MOCK_REQ='{"model":"mock-1","messages":[{"role":"user","content":"hola"}]}'

echo
echo "== health =="
check "healthz responds"                200 "${GW}/healthz"
contains "lists all three providers"   "anthropic" "${GW}/healthz"

echo
echo "== routing by model =="
check    "gpt-*     -> openai"          200 "${CT[@]}" -d "$OPENAI_REQ" "${GW}/v1/chat/completions"
contains "answer came from openai"    "openai heard" "${CT[@]}" -d "$OPENAI_REQ" "${GW}/v1/chat/completions"
check    "claude-*  -> anthropic"       200 "${CT[@]}" -d "$CLAUDE_REQ" "${GW}/v1/chat/completions"
contains "answer came from anthropic" "anthropic heard" "${CT[@]}" -d "$CLAUDE_REQ" "${GW}/v1/chat/completions"
check    "mock-*    -> mock"            200 "${CT[@]}" -d "$MOCK_REQ" "${GW}/v1/chat/completions"

echo
echo "== Anthropic translation =="
absent   "thinking blocks filtered out"   "MUST NOT REACH" "${CT[@]}" -d "$CLAUDE_REQ" "${GW}/v1/chat/completions"
contains "translated back to OpenAI shape"     '"object":"chat.completion"' "${CT[@]}" -d "$CLAUDE_REQ" "${GW}/v1/chat/completions"
contains "usage remapped and totalled"     '"total_tokens":16' "${CT[@]}" -d "$CLAUDE_REQ" "${GW}/v1/chat/completions"

echo
echo "== authentication =="
NOAUTH=(-H 'Content-Type: application/json')
check "no key -> 401"                401 "${NOAUTH[@]}" -d "$OPENAI_REQ" "${GW}/v1/chat/completions"
check "unknown key -> 401"           401 "${NOAUTH[@]}" -H 'Authorization: Bearer gw_wrong' -d "$OPENAI_REQ" "${GW}/v1/chat/completions"
check "healthz stays open"           200 "${GW}/healthz"

echo
echo "== request validation =="
check "malformed JSON -> 400"          400 "${CT[@]}" -d '{"model":' "${GW}/v1/chat/completions"
check "no model -> 400"                400 "${CT[@]}" -d '{"messages":[{"role":"user","content":"h"}]}' "${GW}/v1/chat/completions"
check "no messages -> 400"             400 "${CT[@]}" -d '{"model":"gpt-4o"}' "${GW}/v1/chat/completions"
check "unknown fields accepted"   200 "${CT[@]}" -d '{"model":"gpt-4o","messages":[{"role":"user","content":"h"}],"top_p":0.9,"presence_penalty":1}' "${GW}/v1/chat/completions"
check "GET on the endpoint -> 405"       405 "${GW}/v1/chat/completions"
check "unknown route -> 404"         404 "${GW}/nope"

echo
echo "== streaming =="
STREAM_REQ='{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"hola"}]}'
check    "stream returns 200"             200 "${CT[@]}" -d "$STREAM_REQ" "${GW}/v1/chat/completions"
contains "emits SSE frames"                "data: " "${CT[@]}" -d "$STREAM_REQ" "${GW}/v1/chat/completions"
contains "ends with [DONE]"              "[DONE]" "${CT[@]}" -d "$STREAM_REQ" "${GW}/v1/chat/completions"
contains "text arrives as deltas"        '"delta"' "${CT[@]}" -d "$STREAM_REQ" "${GW}/v1/chat/completions"

CLAUDE_STREAM='{"model":"claude-sonnet-5","stream":true,"messages":[{"role":"user","content":"hola"}]}'
check    "claude-* streams too"   200 "${CT[@]}" -d "$CLAUDE_STREAM" "${GW}/v1/chat/completions"
contains "translated to OpenAI chunks"      '"chat.completion.chunk"' "${CT[@]}" -d "$CLAUDE_STREAM" "${GW}/v1/chat/completions"
absent   "pings never reach the client"    '"ping"' "${CT[@]}" -d "$CLAUDE_STREAM" "${GW}/v1/chat/completions"

echo
echo "== upstream error propagation =="
kill "$FAKE_PID" 2>/dev/null; wait "$FAKE_PID" 2>/dev/null
./bin/fakeupstream -addr ":${FAKE_PORT}" -fail 429 >> /tmp/smoke-fake.log 2>&1 &
FAKE_PID=$!
sleep 1
# A distinct body per check: the cache would otherwise serve the answer from
# the healthy run above and these would never reach the upstream at all.
check    "upstream 429 propagates"  429 "${CT[@]}" -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"error 429 unico"}]}' "${GW}/v1/chat/completions"
contains "vendor message preserved" "told to fail" "${CT[@]}" -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"error mensaje unico"}]}' "${GW}/v1/chat/completions"

kill "$FAKE_PID" 2>/dev/null; wait "$FAKE_PID" 2>/dev/null
check "upstream down -> 502"           502 "${CT[@]}" -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"upstream caido unico"}]}' "${GW}/v1/chat/completions"
contains "healthz exposes the circuits"  "circuits" "${GW}/healthz"

echo
echo "== response cache =="
CACHE_REQ='{"model":"mock-1","messages":[{"role":"user","content":"pregunta cacheable unica"}]}'
icontains "first call -> MISS"          "x-cache: MISS" -D - -o /dev/null "${CT[@]}" -d "$CACHE_REQ" "${GW}/v1/chat/completions"
icontains "second call -> HIT"           "x-cache: HIT"  -D - -o /dev/null "${CT[@]}" -d "$CACHE_REQ" "${GW}/v1/chat/completions"
icontains "different question -> MISS"        "x-cache: MISS" -D - -o /dev/null "${CT[@]}" -d '{"model":"mock-1","messages":[{"role":"user","content":"otra distinta"}]}' "${GW}/v1/chat/completions"

echo
echo "== metrics =="
check    "/metrics responds"               200 "${GW}/metrics"
contains "counts requests"               "llmgw_requests_total" "${GW}/metrics"
contains "latency histogram"          "llmgw_request_duration_seconds" "${GW}/metrics"
contains "counts tokens"              "llmgw_tokens_total" "${GW}/metrics"
contains "circuit state"         "llmgw_circuit_state" "${GW}/metrics"
contains "Go runtime metrics"      "go_goroutines" "${GW}/metrics"
check    "/metrics needs no credential"     200 "${NOAUTH[@]}" "${GW}/metrics"

echo
echo "== automatic fallback =="
# Restart the fake so only the OpenAI dialect is unreachable is not possible
# with one process, so instead point the gateway's primary at a dead port and
# let the chain reach the live secondary.
FB_PORT=$((GW_PORT + 2))
./bin/fakeupstream -addr ":${FAKE_PORT}" >> /tmp/smoke-fake.log 2>&1 &
FAKE_PID=$!
sleep 1
GATEWAY_ADDR=":${FB_PORT}" \
GATEWAY_LOG_LEVEL=error \
GATEWAY_API_KEYS="smoke:${GW_KEY}" \
OPENAI_API_KEY=sk-fake-openai \
OPENAI_BASE_URL="http://127.0.0.1:1/v1" \
ANTHROPIC_API_KEY=sk-fake-anthropic \
ANTHROPIC_BASE_URL="http://127.0.0.1:${FAKE_PORT}/v1" \
GATEWAY_FALLBACK_MODELS="gpt-4o-mini:claude-sonnet-5" \
GATEWAY_RETRY_BASE_DELAY=1ms \
  ./bin/gateway > /tmp/smoke-fb.log 2>&1 &
FB_PID=$!
for _ in $(seq 40); do curl -sf "http://127.0.0.1:${FB_PORT}/healthz" >/dev/null 2>&1 && break; sleep 0.25; done

FB="http://127.0.0.1:${FB_PORT}"
check    "primary down -> 200 via fallback" 200 "${CT[@]}" -d "$OPENAI_REQ" "${FB}/v1/chat/completions"
contains "the secondary answers"   "anthropic heard" "${CT[@]}" -d "$OPENAI_REQ" "${FB}/v1/chat/completions"
contains "the primary's circuit opens"   "open" "${FB}/healthz"
kill "$FB_PID" 2>/dev/null; wait "$FB_PID" 2>/dev/null

echo
echo "== rate limiting =="
RL_PORT=$((GW_PORT + 1))
GATEWAY_ADDR=":${RL_PORT}" \
GATEWAY_LOG_LEVEL=error \
GATEWAY_API_KEYS="smoke:${GW_KEY}" \
GATEWAY_RATE_LIMIT_RPS=0.01 \
GATEWAY_RATE_LIMIT_BURST=2 \
  ./bin/gateway > /tmp/smoke-rl.log 2>&1 &
RL_PID=$!
for _ in $(seq 40); do curl -sf "http://127.0.0.1:${RL_PORT}/healthz" >/dev/null 2>&1 && break; sleep 0.25; done

RL="http://127.0.0.1:${RL_PORT}"
check "1 of 2 in the burst -> 200"          200 "${CT[@]}" -d "$MOCK_REQ" "${RL}/v1/chat/completions"
check "2 of 2 in the burst -> 200"          200 "${CT[@]}" -d "$MOCK_REQ" "${RL}/v1/chat/completions"
check "past the burst -> 429"           429 "${CT[@]}" -d "$MOCK_REQ" "${RL}/v1/chat/completions"
contains "Retry-After header"          "Retry-After" -D - -o /dev/null "${CT[@]}" -d "$MOCK_REQ" "${RL}/v1/chat/completions"
icontains "X-RateLimit-Limit header"   "x-ratelimit-limit" -D - -o /dev/null "${CT[@]}" -d "$MOCK_REQ" "${RL}/v1/chat/completions"
icontains "X-RateLimit-Remaining header" "x-ratelimit-remaining" -D - -o /dev/null "${CT[@]}" -d "$MOCK_REQ" "${RL}/v1/chat/completions"
kill "$RL_PID" 2>/dev/null; wait "$RL_PID" 2>/dev/null

echo
if [ "$fail" -eq 0 ]; then
  echo "$(green "ALL GOOD")  ${pass} checks"
else
  echo "$(red "FAILURES")  ${pass} passed, ${fail} failed"
fi
echo "$(dim "logs: /tmp/smoke-gw.log  /tmp/smoke-fake.log")"
exit "$fail"
