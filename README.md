# llm-gateway

[![CI](https://github.com/Mampiz/llm-gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/Mampiz/llm-gateway/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/Mampiz/llm-gateway.svg)](https://pkg.go.dev/github.com/Mampiz/llm-gateway)
[![Go Report Card](https://goreportcard.com/badge/github.com/Mampiz/llm-gateway)](https://goreportcard.com/report/github.com/Mampiz/llm-gateway)

A single OpenAI-compatible endpoint in front of several LLM providers: routing, automatic fallback, distributed rate limiting, response caching, streaming and metrics.

## Status

Phase 1 — basic passthrough proxy. Work in progress.

## Running

```bash
cp .env.example .env
make run
```

With `GATEWAY_PROVIDER=mock` the gateway answers with a canned response, so no API key is needed.

```bash
curl -sS localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"say hi"}]}'
```

## Testing

```bash
make test        # full suite
make test-race   # same, under the race detector
make cover       # coverage profile + total
make ci          # everything the pipeline runs: lint, race, build
```

Tests use `httptest.NewServer`, so the whole `net/http` stack is exercised
against a real loopback listener. No API key and no network access are needed,
and nothing is billed.

## Docker

```bash
docker compose up --build
```

The image is built from `scratch` and weighs about 10 MB: a statically linked
binary, CA certificates and nothing else — no shell, no package manager, no
libc to patch.

## Releases

Tagging drives everything:

```bash
git tag v0.2.0 && git push origin v0.2.0
```

That builds binaries for linux/darwin/windows on amd64 and arm64, attaches them
to a GitHub release with generated notes, and publishes a multi-arch image to
`ghcr.io/Mampiz/llm-gateway`.

## Layout

```
cmd/gateway         entrypoint: config, wiring, graceful shutdown
internal/config     environment loading and validation
internal/server     routing, middleware, HTTP handlers
internal/provider   the Provider interface and the shared schema
  ├── openai       OpenAI-compatible client
  └── mock         offline fake provider
```
