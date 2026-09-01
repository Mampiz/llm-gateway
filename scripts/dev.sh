#!/usr/bin/env bash
# Runs the gateway against a fake upstream and leaves both up so you can poke
# at them by hand. Ctrl-C stops both.
#
#   make dev
set -uo pipefail

cd "$(dirname "$0")/.."

GW_KEY=${GW_KEY:-gw_devkey}
FAKE_PORT=${FAKE_PORT:-9000}
GW_PORT=${GW_PORT:-8080}

cleanup() {
  [ -n "${GW_PID:-}" ]   && kill "$GW_PID"   2>/dev/null
  [ -n "${FAKE_PID:-}" ] && kill "$FAKE_PID" 2>/dev/null
  wait 2>/dev/null
  echo
  echo "parado."
}
trap cleanup EXIT INT TERM

go build -o bin/gateway ./cmd/gateway || exit 1
go build -o bin/fakeupstream ./cmd/fakeupstream || exit 1

./bin/fakeupstream -addr ":${FAKE_PORT}" &
FAKE_PID=$!

GATEWAY_ADDR=":${GW_PORT}" \
GATEWAY_LOG_LEVEL=${GATEWAY_LOG_LEVEL:-info} \
GATEWAY_API_KEYS="dev:${GW_KEY}" \
OPENAI_API_KEY=sk-fake-openai \
OPENAI_BASE_URL="http://127.0.0.1:${FAKE_PORT}/v1" \
ANTHROPIC_API_KEY=sk-fake-anthropic \
ANTHROPIC_BASE_URL="http://127.0.0.1:${FAKE_PORT}/v1" \
  ./bin/gateway &
GW_PID=$!

sleep 1
cat <<BANNER

  gateway   http://127.0.0.1:${GW_PORT}
  upstream  http://127.0.0.1:${FAKE_PORT}
  api key   ${GW_KEY}

  curl -s localhost:${GW_PORT}/healthz | jq

  curl -s localhost:${GW_PORT}/v1/chat/completions \\
    -H 'Content-Type: application/json' \\
    -H 'Authorization: Bearer ${GW_KEY}' \\
    -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hola"}]}' | jq

  curl -s localhost:${GW_PORT}/v1/chat/completions \\
    -H 'Content-Type: application/json' \\
    -H 'Authorization: Bearer ${GW_KEY}' \\
    -d '{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hola"}]}' | jq

  Ctrl-C para parar.

BANNER

wait
