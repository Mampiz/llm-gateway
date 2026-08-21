# llm-gateway

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

## Layout

```
cmd/gateway         entrypoint: config, wiring, graceful shutdown
internal/config     environment loading and validation
internal/server     routing, middleware, HTTP handlers
internal/provider   the Provider interface and the shared schema
  ├── openai       OpenAI-compatible client
  └── mock         offline fake provider
```
