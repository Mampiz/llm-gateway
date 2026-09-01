#!/usr/bin/env bash
# Takes a fresh clone to a verified, running gateway.
#
# Needs Go and nothing else: no API key, no network access to any vendor, no
# database. Redis and Docker are optional and only unlock the tests that need
# them.
#
#   ./scripts/bootstrap.sh
set -uo pipefail

cd "$(dirname "$0")/.."

green() { printf '\033[32m%s\033[0m' "$1"; }
red()   { printf '\033[31m%s\033[0m' "$1"; }
dim()   { printf '\033[2m%s\033[0m' "$1"; }

step() { printf '\n%s %s\n' "$(dim '==')" "$1"; }
ok()   { printf '  %s %s\n' "$(green '✓')" "$1"; }
die()  { printf '  %s %s\n' "$(red '✗')" "$1"; exit 1; }

step "Checking the toolchain"
command -v go >/dev/null 2>&1 || die "Go is not installed. See https://go.dev/dl/"

required="1.26"
have=$(go env GOVERSION | sed 's/^go//')
if [ "$(printf '%s\n%s\n' "$required" "$have" | sort -V | head -1)" != "$required" ]; then
  die "Go $have found, $required or newer required"
fi
ok "Go $have"

step "Downloading dependencies"
go mod download || die "go mod download failed"
ok "modules ready"

step "Building"
go build -o bin/gateway ./cmd/gateway || die "the gateway did not build"
go build -o bin/fakeupstream ./cmd/fakeupstream || die "the fake upstream did not build"
ok "bin/gateway and bin/fakeupstream"

step "Running the unit suite"
go test ./... -count=1 >/dev/null 2>&1 || die "unit tests failed. Run: go test ./..."
ok "unit tests pass"

step "Running the end-to-end suite"
go test -tags e2e ./test/e2e/ -count=1 >/dev/null 2>&1 || die "e2e tests failed. Run: go test -tags e2e ./test/e2e/"
ok "end-to-end tests pass"

step "Driving the real binary"
./scripts/smoke.sh >/tmp/bootstrap-smoke.log 2>&1 || {
  cat /tmp/bootstrap-smoke.log
  die "the smoke run failed"
}
ok "$(grep -oE '[0-9]+ comprobaciones' /tmp/bootstrap-smoke.log | tail -1) passed against a running gateway"

step "Optional extras"
if command -v docker >/dev/null 2>&1; then
  ok "docker found: 'make test-integration' will exercise the Redis-backed limiter"
else
  printf '  %s %s\n' "$(dim '-')" "$(dim 'docker not found: the Redis integration tests will be skipped')"
fi

key=$(./bin/gateway -genkey)

cat <<BANNER

$(green 'Ready.')

  Start it, with a fake upstream and no API keys needed:

    make dev

  Or point it at the real thing:

    export GATEWAY_API_KEYS="me:${key}"
    export OPENAI_API_KEY=sk-...
    make run

  Then:

    curl localhost:8080/v1/chat/completions \\
      -H "Authorization: Bearer ${key}" \\
      -H 'Content-Type: application/json' \\
      -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'

BANNER
