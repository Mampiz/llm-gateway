# Plan

Remaining work to take llm-gateway from "streaming half-done" to presentable.
Phases are sequential: each one assumes the previous is green.

Every phase has a **verifier**: a concrete command with a yes/no outcome. A
phase is done when its verifier passes from a clean state, not when the code
looks finished.

## Status legend

- **REQUIRED** — the project is not presentable without it.
- **NICE** — sacrificed first if context runs out.

---

## F1 · Anthropic streaming — REQUIRED

Closes phase 3. `anthropic.ChatStream` currently returns
`ErrStreamingNotSupported`, so `claude-*` models cannot stream. Same iterator
contract as the OpenAI one, different dialect: named events, `ping` frames to
ignore, `usage` arriving late in `message_delta`, no `[DONE]` sentinel.

**Verifier**
```
go test ./internal/provider/anthropic/ -run TestChatStream -race -count=1
go test -tags e2e ./test/e2e/ -run TestStreamsAnthropic -count=1
```

---

## F2 · Gateway API keys and authentication — REQUIRED

Today anyone who reaches the port can spend the budget. Introduce keys the
gateway issues itself (`gw_...`), an auth middleware, and per-key identity in
the request context. Provider credentials stay server-side and are never
accepted from clients.

**Verifier**
```
go test ./internal/auth/ ./internal/server/ -race -count=1
./scripts/smoke.sh          # includes 401 without a key, 200 with one
```

---

## F3 · Distributed rate limiting — REQUIRED

Per-key token bucket. Two implementations behind one interface: in-memory for
single-instance and tests, Redis for multi-instance. The Redis one runs the
bucket as a Lua script so check-and-consume is atomic across replicas.

**Verifier**
```
go test ./internal/ratelimit/ -race -count=1
go test -tags integration ./internal/ratelimit/ -count=1   # against real Redis
docker compose up -d redis && ./scripts/smoke.sh           # includes a 429 burst
```

---

## F4 · Automatic fallback and circuit breaker — REQUIRED

When the primary provider fails, rate-limits or times out, retry on the next
one with exponential backoff and jitter. A circuit breaker stops hammering a
provider that is consistently failing. Builds on `provider.Error.Retryable`
and the registry, both already in place.

**Verifier**
```
go test ./internal/provider/ -run 'TestFallback|TestBreaker' -race -count=1
go test -tags e2e ./test/e2e/ -run TestFallsBackToSecondProvider -count=1
```

---

## F5 · Prometheus metrics — REQUIRED

`/metrics` with request count, latency histogram, error rate and token counts,
labelled by provider, model and outcome. This is half of what a gateway is
for: without it there is no reason to put one in the path.

**Verifier**
```
go test ./internal/metrics/ ./internal/server/ -race -count=1
curl -s localhost:8080/metrics | grep -q llmgw_requests_total
```

---

## F6 · Response cache — NICE

Exact-match cache keyed on the normalized request. `singleflight` collapses
identical concurrent requests so a cache miss is fetched once, not N times.
Streaming responses are accumulated while being forwarded, then stored.

**Verifier**
```
go test ./internal/cache/ -race -count=1
./scripts/smoke.sh          # includes a cache hit on the second identical call
```

---

## F7 · Bootstrap, README and final pass — REQUIRED

The README must explain what the project demonstrates, not list features. A
clean clone must run with one command. Full pipeline green.

**Verifier**
```
make ci                     # lint + race + e2e + vuln + build
./scripts/bootstrap.sh      # clean clone -> running gateway -> smoke passes
```

---

## Sacrifice order if context runs out

1. F6 (cache) — the gateway is complete and honest without it.
2. Nothing else. F1-F5 and F7 are the project.
