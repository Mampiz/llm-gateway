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
    printf '  %s %-46s %s\n' "$(red '✗')" "$name" "$(red "HTTP $status, esperado $want")"
    sed 's/^/      /' /tmp/smoke.body 2>/dev/null | head -3
    fail=$((fail + 1))
  fi
}

# contains <name> <needle> <curl args...>
contains() {
  local name=$1 needle=$2; shift 2
  local body
  body=$(curl -sS "$@" 2>/dev/null)
  if printf '%s' "$body" | grep -q -- "$needle"; then
    printf '  %s %-46s %s\n' "$(green '✓')" "$name" "$(dim "contiene '$needle'")"
    pass=$((pass + 1))
  else
    printf '  %s %-46s %s\n' "$(red '✗')" "$name" "$(red "no contiene '$needle'")"
    printf '      %s\n' "$body" | head -3
    fail=$((fail + 1))
  fi
}

# absent <name> <needle> <curl args...>
absent() {
  local name=$1 needle=$2; shift 2
  local body
  body=$(curl -sS "$@" 2>/dev/null)
  if printf '%s' "$body" | grep -q -- "$needle"; then
    printf '  %s %-46s %s\n' "$(red '✗')" "$name" "$(red "filtró '$needle'")"
    fail=$((fail + 1))
  else
    printf '  %s %-46s %s\n' "$(green '✓')" "$name" "$(dim "sin '$needle'")"
    pass=$((pass + 1))
  fi
}

json() { printf '%s' "$1"; }
post() { echo -H 'Content-Type: application/json'; }

cleanup() {
  [ -n "${GW_PID:-}" ]   && kill "$GW_PID"   2>/dev/null
  [ -n "${FAKE_PID:-}" ] && kill "$FAKE_PID" 2>/dev/null
  wait 2>/dev/null
}
trap cleanup EXIT INT TERM

echo "== compilando =="
go build -o bin/gateway ./cmd/gateway || exit 1
go build -o bin/fakeupstream ./cmd/fakeupstream || exit 1

echo "== arrancando upstream falso en :${FAKE_PORT} =="
./bin/fakeupstream -addr ":${FAKE_PORT}" > /tmp/smoke-fake.log 2>&1 &
FAKE_PID=$!

echo "== arrancando gateway en :${GW_PORT} =="
GATEWAY_ADDR=":${GW_PORT}" \
GATEWAY_LOG_LEVEL=warn \
GATEWAY_API_KEYS="smoke:${GW_KEY}" \
OPENAI_API_KEY=sk-fake-openai \
OPENAI_BASE_URL="http://127.0.0.1:${FAKE_PORT}/v1" \
ANTHROPIC_API_KEY=sk-fake-anthropic \
ANTHROPIC_BASE_URL="http://127.0.0.1:${FAKE_PORT}/v1" \
  ./bin/gateway > /tmp/smoke-gw.log 2>&1 &
GW_PID=$!

for _ in $(seq 40); do
  curl -sf "${GW}/healthz" >/dev/null 2>&1 && break
  sleep 0.25
done
if ! curl -sf "${GW}/healthz" >/dev/null 2>&1; then
  red "el gateway no arrancó"; echo; cat /tmp/smoke-gw.log; exit 1
fi

CT=(-H 'Content-Type: application/json' -H "Authorization: Bearer ${GW_KEY}")
OPENAI_REQ='{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hola"}]}'
CLAUDE_REQ='{"model":"claude-sonnet-5","messages":[{"role":"system","content":"se breve"},{"role":"user","content":"hola"},{"role":"system","content":"en espanol"}]}'
MOCK_REQ='{"model":"mock-1","messages":[{"role":"user","content":"hola"}]}'

echo
echo "== salud =="
check "healthz responde"                200 "${GW}/healthz"
contains "lista los tres proveedores"   "anthropic" "${GW}/healthz"

echo
echo "== enrutado por modelo =="
check    "gpt-*     -> openai"          200 "${CT[@]}" -d "$OPENAI_REQ" "${GW}/v1/chat/completions"
contains "respuesta viene de openai"    "openai heard" "${CT[@]}" -d "$OPENAI_REQ" "${GW}/v1/chat/completions"
check    "claude-*  -> anthropic"       200 "${CT[@]}" -d "$CLAUDE_REQ" "${GW}/v1/chat/completions"
contains "respuesta viene de anthropic" "anthropic heard" "${CT[@]}" -d "$CLAUDE_REQ" "${GW}/v1/chat/completions"
check    "mock-*    -> mock"            200 "${CT[@]}" -d "$MOCK_REQ" "${GW}/v1/chat/completions"

echo
echo "== traduccion Anthropic =="
absent   "bloques thinking filtrados"   "MUST NOT REACH" "${CT[@]}" -d "$CLAUDE_REQ" "${GW}/v1/chat/completions"
contains "formato OpenAI de vuelta"     '"object":"chat.completion"' "${CT[@]}" -d "$CLAUDE_REQ" "${GW}/v1/chat/completions"
contains "usage traducido y sumado"     '"total_tokens":16' "${CT[@]}" -d "$CLAUDE_REQ" "${GW}/v1/chat/completions"

echo
echo "== autenticacion =="
NOAUTH=(-H 'Content-Type: application/json')
check "sin clave -> 401"                401 "${NOAUTH[@]}" -d "$OPENAI_REQ" "${GW}/v1/chat/completions"
check "clave invalida -> 401"           401 "${NOAUTH[@]}" -H 'Authorization: Bearer gw_wrong' -d "$OPENAI_REQ" "${GW}/v1/chat/completions"
check "healthz sigue abierto"           200 "${GW}/healthz"

echo
echo "== validacion =="
check "JSON malformado -> 400"          400 "${CT[@]}" -d '{"model":' "${GW}/v1/chat/completions"
check "sin model -> 400"                400 "${CT[@]}" -d '{"messages":[{"role":"user","content":"h"}]}' "${GW}/v1/chat/completions"
check "sin messages -> 400"             400 "${CT[@]}" -d '{"model":"gpt-4o"}' "${GW}/v1/chat/completions"
check "campos desconocidos aceptados"   200 "${CT[@]}" -d '{"model":"gpt-4o","messages":[{"role":"user","content":"h"}],"top_p":0.9,"presence_penalty":1}' "${GW}/v1/chat/completions"
check "GET en el endpoint -> 405"       405 "${GW}/v1/chat/completions"
check "ruta inexistente -> 404"         404 "${GW}/nope"

echo
echo "== streaming =="
STREAM_REQ='{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"hola"}]}'
check    "stream devuelve 200"             200 "${CT[@]}" -d "$STREAM_REQ" "${GW}/v1/chat/completions"
contains "emite tramas SSE"                "data: " "${CT[@]}" -d "$STREAM_REQ" "${GW}/v1/chat/completions"
contains "termina con [DONE]"              "[DONE]" "${CT[@]}" -d "$STREAM_REQ" "${GW}/v1/chat/completions"
contains "el texto llega en deltas"        '"delta"' "${CT[@]}" -d "$STREAM_REQ" "${GW}/v1/chat/completions"

CLAUDE_STREAM='{"model":"claude-sonnet-5","stream":true,"messages":[{"role":"user","content":"hola"}]}'
check    "claude-* tambien hace streaming"   200 "${CT[@]}" -d "$CLAUDE_STREAM" "${GW}/v1/chat/completions"
contains "traducido a formato OpenAI"      '"chat.completion.chunk"' "${CT[@]}" -d "$CLAUDE_STREAM" "${GW}/v1/chat/completions"
absent   "los ping no llegan al cliente"    '"ping"' "${CT[@]}" -d "$CLAUDE_STREAM" "${GW}/v1/chat/completions"

echo
echo "== propagacion de errores del upstream =="
kill "$FAKE_PID" 2>/dev/null; wait "$FAKE_PID" 2>/dev/null
./bin/fakeupstream -addr ":${FAKE_PORT}" -fail 429 >> /tmp/smoke-fake.log 2>&1 &
FAKE_PID=$!
sleep 1
check    "429 del upstream se propaga"  429 "${CT[@]}" -d "$OPENAI_REQ" "${GW}/v1/chat/completions"
contains "mensaje del vendor conservado" "told to fail" "${CT[@]}" -d "$OPENAI_REQ" "${GW}/v1/chat/completions"

kill "$FAKE_PID" 2>/dev/null; wait "$FAKE_PID" 2>/dev/null
check "upstream caido -> 502"           502 "${CT[@]}" -d "$OPENAI_REQ" "${GW}/v1/chat/completions"

echo
if [ "$fail" -eq 0 ]; then
  echo "$(green "TODO BIEN")  ${pass} comprobaciones"
else
  echo "$(red "FALLOS")  ${pass} bien, ${fail} mal"
fi
echo "$(dim "logs: /tmp/smoke-gw.log  /tmp/smoke-fake.log")"
exit "$fail"
