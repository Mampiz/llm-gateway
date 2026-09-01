<h1 align="center">llm-gateway</h1>

<p align="center">
  One OpenAI-compatible endpoint in front of every LLM provider you use.<br>
  Routing, streaming, failover, rate limiting, caching and metrics.
</p>

<p align="center">
  <a href="https://github.com/Mampiz/llm-gateway/actions/workflows/tests.yml"><img alt="Tests" src="https://github.com/Mampiz/llm-gateway/actions/workflows/tests.yml/badge.svg"></a>
  <a href="https://github.com/Mampiz/llm-gateway/actions/workflows/e2e.yml"><img alt="E2E Tests" src="https://github.com/Mampiz/llm-gateway/actions/workflows/e2e.yml/badge.svg"></a>
  <a href="https://github.com/Mampiz/llm-gateway/actions/workflows/lint.yml"><img alt="Lint" src="https://github.com/Mampiz/llm-gateway/actions/workflows/lint.yml/badge.svg"></a>
  <a href="https://github.com/Mampiz/llm-gateway/actions/workflows/security.yml"><img alt="Security" src="https://github.com/Mampiz/llm-gateway/actions/workflows/security.yml/badge.svg"></a>
  <a href="https://github.com/Mampiz/llm-gateway/actions/workflows/chart.yml"><img alt="Test Chart" src="https://github.com/Mampiz/llm-gateway/actions/workflows/chart.yml/badge.svg"></a>
  <br>
  <a href="https://goreportcard.com/report/github.com/Mampiz/llm-gateway"><img alt="Go Report Card" src="https://goreportcard.com/badge/github.com/Mampiz/llm-gateway"></a>
  <a href="go.mod"><img alt="Go" src="https://img.shields.io/github/go-mod/go-version/Mampiz/llm-gateway"></a>
  <a href="https://github.com/Mampiz/llm-gateway/pkgs/container/llm-gateway"><img alt="Container" src="https://img.shields.io/badge/ghcr.io-llm--gateway-2496ed?logo=docker&logoColor=white"></a>
  <a href="https://github.com/Mampiz/llm-gateway/releases/latest"><img alt="Release" src="https://img.shields.io/github/v/release/Mampiz/llm-gateway"></a>
  <a href="LICENSE"><img alt="License: MIT" src="https://img.shields.io/badge/License-MIT-blue.svg"></a>
</p>

<p align="center">
  <img src="docs/assets/streaming.gif" alt="Tokens arriving one by one from a Claude model, in OpenAI format" width="820">
</p>

<p align="center">
  <em>A Claude model, streamed through the gateway, in OpenAI's wire format.<br>
  Your client never learns which vendor answered.</em>
</p>

## Why

An application that talks to OpenAI directly is married to OpenAI. Its key is in
that application's config, its retries are that application's problem, and
nobody can say what it costs. Six applications means six of each.

A gateway takes that over. Applications speak one format to one endpoint, and
the messy parts happen once:

| | |
|---|---|
| **One dialect** | Switching from `gpt-4o-mini` to `claude-sonnet-5` is a string change, not a rewrite. |
| **One set of credentials** | Vendor keys live here. Applications get gateway keys, revocable one at a time. |
| **Resilience in one place** | Retries, failover and circuit breaking are written once instead of six times. |
| **Visibility** | Latency, error rate, tokens and cost per provider, which is impossible to assemble when every app calls out on its own. |

## Quick start

```bash
docker run --rm ghcr.io/mampiz/llm-gateway:latest -genkey
```

```bash
docker run -d -p 8080:8080 \
  -e GATEWAY_API_KEYS="app:gw_YOUR_KEY" \
  -e OPENAI_API_KEY="sk-..." \
  -e GATEWAY_PROVIDER=openai \
  ghcr.io/mampiz/llm-gateway:latest
```

```bash
curl localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer gw_YOUR_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'
```

Any OpenAI client works unchanged. Point its base URL at the gateway:

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="gw_YOUR_KEY")

client.chat.completions.create(
    model="claude-sonnet-5",                 # answered by Anthropic
    messages=[{"role": "user", "content": "hi"}],
    stream=True,
)
```

### From source

```bash
git clone https://github.com/Mampiz/llm-gateway.git && cd llm-gateway
./scripts/bootstrap.sh
```

<p align="center">
  <img src="docs/assets/bootstrap.gif" alt="bootstrap.sh building, testing and verifying a running gateway" width="620">
</p>

Go and nothing else. No API key, no vendor network access, no database. It
builds, runs both test suites, drives the real binary through 49 checks against
a fake upstream, and prints a working API key.

## What it does

### Routes by model

| Model prefix | Provider |
|---|---|
| `gpt-`, `o1-`, `o3-`, `o4-`, `chatgpt-` | OpenAI |
| `claude-` | Anthropic |
| `mock-` | built-in fake, no key needed |

Longer prefixes win, so a specific rule always beats a general one regardless of
wiring order. Anything matching nothing falls back to `GATEWAY_PROVIDER`.

Each vendor speaks its own dialect: different endpoints, auth headers, system
prompt placement, content shapes, stop-reason vocabularies and stream framing.
Every provider package owns its translation, and none of that vocabulary is
visible from outside it. Request fields the gateway does not model, such as
`top_p` or `presence_penalty`, are forwarded to providers that understand them
rather than rejected or silently dropped.

### Fails over when a provider does not

```bash
GATEWAY_FALLBACK_MODELS="gpt-4o-mini:claude-sonnet-5,claude-sonnet-5:gpt-4o-mini"
```

<p align="center">
  <img src="docs/assets/fallback.gif" alt="OpenAI is down, the answer comes from Anthropic, the circuit is open" width="760">
</p>

Falling back changes the model as well as the provider, because the same model
rarely exists on two vendors. Three rules keep it from making things worse:

- **Only retryable failures fail over.** A 400 is the caller's mistake, and
  asking a second vendor the same malformed question wastes their time too.
- **Backoff is jittered.** Without jitter every client that failed at the same
  instant retries at the same instant, and the upstream takes a synchronised
  wave exactly when it can least afford one.
- **A circuit breaker pulls a failing provider out of rotation** for
  `GATEWAY_BREAKER_COOLDOWN`, then lets exactly one probe decide whether it is
  back. Hammering a struggling upstream slows its recovery and costs every
  caller the latency of a doomed attempt.

`/healthz` reports every circuit, which is the first thing worth reading when
answers start coming from the wrong vendor.

### Streams, and keeps the connection alive

Both vendors stream. The gateway reads whichever dialect the provider speaks and
emits OpenAI-shaped frames, so clients need one parser.

While the model thinks, it sends SSE keep-alive comments so intermediaries do
not drop a connection that is merely waiting. It notices a client that walks
away and stops paying for tokens immediately. It cuts a provider that goes
silent (`GATEWAY_STREAM_IDLE_TIMEOUT`, which resets on every chunk, so a long
healthy answer is never affected) and one that never stops talking
(`GATEWAY_STREAM_MAX_DURATION`).

On SIGTERM, in-flight streams are told to wind up and answered with an
explanation rather than cut mid-frame. Give the pod a
`terminationGracePeriodSeconds` above 20.

### Meters every caller

A token bucket per caller: up to `GATEWAY_RATE_LIMIT_BURST` at once, refilled at
`GATEWAY_RATE_LIMIT_RPS` per second. A bucket absorbs bursts while capping the
sustained rate, which a fixed window per minute cannot do, since there a caller
spends a whole minute's allowance in the last second of one window and again in
the first second of the next.

With `GATEWAY_REDIS_URL` set, every replica shares one bucket. Without it the
bucket is per process, so N replicas allow N times the intended rate. The
check-and-consume runs as a Lua script inside Redis, because two gateways
reading "one token left" at the same instant would both allow.

Denials carry `Retry-After` and `X-RateLimit-*`. A limiter outage fails open:
briefly over-serving is a smaller failure than refusing everything because a
side channel is down.

### Caches, and collapses duplicates

Off by default. `GATEWAY_CACHE_TTL` turns it on, and hits come back with
`X-Cache: HIT`. The key digests everything that determines the answer, so a hit
is always an answer to exactly the same question.

Identical *concurrent* requests are collapsed with `singleflight`: a cold cache
hit by a hundred copies of the same question fetches one answer, not a hundred.
That is the case a plain lookup cannot help with, since none of the hundred has
stored anything yet.

### Reports what it is doing

`/metrics` speaks Prometheus and needs no credential, like the health probe.

| Metric | What it answers |
|---|---|
| `llmgw_requests_total` | throughput and error rate, by provider, model, outcome and mode |
| `llmgw_request_duration_seconds` | end-to-end latency |
| `llmgw_stream_first_token_seconds` | the wait a user actually feels on a streamed answer |
| `llmgw_tokens_total` | tokens in and out, the basis for cost per provider |
| `llmgw_upstream_errors_total` | which vendor is failing, and with what status |
| `llmgw_circuit_state` | which providers are out of rotation right now |
| `llmgw_rate_limited_total` | refusals by the limiter |
| `llmgw_requests_in_flight` | concurrency |

A Grafana dashboard and alerting rules are in [`deploy/`](deploy/).

## Authentication

Clients authenticate with keys the gateway issues, never with a vendor's
credentials. A leaked client key costs one revocation; a leaked vendor key costs
a rotation and whatever was spent in between.

```bash
go run ./cmd/gateway -genkey
GATEWAY_API_KEYS="alice:gw_a4573d31...,ci:gw_91ab..."
```

Secrets are stored as SHA-256 digests, so the running process holds nothing
usable, and a rejected key is never echoed back. `/healthz` and `/metrics` stay
open: a probe that needs a credential stops working exactly when something is
wrong. Starting with neither keys nor an explicit `GATEWAY_AUTH_DISABLED=true`
is a startup error, so an open gateway is always deliberate.

## Configuration

Every setting is an environment variable. [`.env.example`](.env.example) lists
them all with comments.

| Variable | Default | |
|---|---|---|
| `GATEWAY_ADDR` | `:8080` | listen address |
| `GATEWAY_API_KEYS` | none | `name:secret` pairs, comma separated |
| `GATEWAY_AUTH_DISABLED` | `false` | explicit opt-out, local development only |
| `GATEWAY_PROVIDER` | `mock` | fallback for models matching no prefix |
| `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` | none | enables that provider |
| `OPENAI_BASE_URL` / `ANTHROPIC_BASE_URL` | vendor default | point at a compatible endpoint |
| `GATEWAY_REQUEST_TIMEOUT` | `60s` | budget for one non-streaming answer |
| `GATEWAY_RATE_LIMIT_RPS` / `_BURST` | `10` / `20` | per caller; `0` disables |
| `GATEWAY_REDIS_URL` | none | shared bucket and cache |
| `GATEWAY_CACHE_TTL` | `0` | `0` disables caching |
| `GATEWAY_CACHE_SCOPE` | `shared` | or `caller` for private entries |
| `GATEWAY_STREAM_IDLE_TIMEOUT` | `60s` | silence tolerated, resets per chunk |
| `GATEWAY_STREAM_HEARTBEAT` | `15s` | keep-alive interval |
| `GATEWAY_STREAM_MAX_DURATION` | `10m` | hard cap on one answer |
| `GATEWAY_FALLBACK_MODELS` | none | `model:fallback\|fallback`, comma separated |
| `GATEWAY_RETRY_ATTEMPTS` / `_BASE_DELAY` | `2` / `200ms` | per provider |
| `GATEWAY_BREAKER_THRESHOLD` / `_COOLDOWN` | `5` / `30s` | per provider |
| `GATEWAY_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

## Deploying

### Kubernetes

```bash
helm install gw ./deploy/helm/llm-gateway \
  --set secrets.gatewayApiKeys="app:gw_YOUR_KEY" \
  --set secrets.openaiApiKey="sk-..." \
  --set config.provider=openai
```

The chart brings its own Redis, an HPA, a PodDisruptionBudget and an optional
ServiceMonitor. CI lints it and validates every rendered manifest against the
Kubernetes schemas on each push. See [`deploy/README.md`](deploy/README.md).

### Docker Compose

```bash
docker compose up --build
```

### The image

Built from `scratch`: a statically linked binary, CA certificates, and nothing
else. No shell, no package manager, no libc to patch. It runs unprivileged with
a read-only root filesystem, and every release ships an SBOM and build
provenance.

## Development

```bash
make dev     # gateway plus a fake upstream, no keys needed
make smoke   # start everything, check every route, tear it down
make ci      # everything the pipeline runs
```

<p align="center">
  <img src="docs/assets/smoke.gif" alt="make smoke running 49 checks against a live gateway" width="600">
</p>

The fake upstream in [`cmd/fakeupstream`](cmd/fakeupstream) speaks both dialects,
streams real SSE, and misbehaves on request so failure paths can be exercised:

```bash
go run ./cmd/fakeupstream -fail 429          # every call is rate limited
go run ./cmd/fakeupstream -token-delay 800ms # slow motion streaming
```

### Tests

```bash
make test              # unit
make test-race         # under the race detector
make test-integration  # against a real Redis
make e2e               # the real binary, on a real socket
```

Unit tests use `httptest.NewServer` rather than a mocked transport, so the whole
`net/http` stack is exercised. The end-to-end suite compiles the actual binary
and drives it over HTTP. The integration suite needs a real Redis, because a
fake would not exercise the Lua script's atomicity, which is the only reason
that code exists.

## Layout

```
cmd/gateway         entrypoint: config, wiring, graceful shutdown
cmd/fakeupstream    offline stand-in for both vendor APIs, SSE included
internal/auth       gateway API keys, stored as digests
internal/ratelimit  token bucket, in-process and Redis-backed
internal/cache      response cache, in-process and Redis-backed
internal/metrics    Prometheus collectors
internal/server     routing, middleware, HTTP handlers, SSE writing
internal/provider   the Provider interface, the canonical schema, the registry,
                    the fallback router and the circuit breaker
  ├── openai        OpenAI-compatible client, buffered and streaming
  ├── anthropic     Messages API client and its translation layer
  └── mock          offline fake provider
deploy/             Helm chart, alerting rules, Grafana dashboard
scripts/            bootstrap.sh, dev.sh, smoke.sh
test/e2e            end-to-end suite (build tag: e2e)
```

Dependency arrows only point inwards: `internal/provider` imports no vendor
package. Go turns that rule into a compile error rather than a convention, since
the reverse would be an import cycle.

## Documentation

| | |
|---|---|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | how it is put together, and why |
| [docs/OPERATIONS.md](docs/OPERATIONS.md) | what to look at when something is wrong |
| [docs/DECISIONS.md](docs/DECISIONS.md) | every design decision and its reasoning, including the rejected ones |
| [deploy/README.md](deploy/README.md) | running it on Kubernetes |
| [CONTRIBUTING.md](CONTRIBUTING.md) | the loop, the house style, adding a provider |
| [CHANGELOG.md](CHANGELOG.md) | what changed and when |
| [SECURITY.md](SECURITY.md) | reporting a vulnerability |

## License

MIT. See [LICENSE](LICENSE).
