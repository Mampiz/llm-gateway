# llm-gateway

[![Tests](https://github.com/Mampiz/llm-gateway/actions/workflows/tests.yml/badge.svg)](https://github.com/Mampiz/llm-gateway/actions/workflows/tests.yml)
[![E2E Tests](https://github.com/Mampiz/llm-gateway/actions/workflows/e2e.yml/badge.svg)](https://github.com/Mampiz/llm-gateway/actions/workflows/e2e.yml)
[![Lint](https://github.com/Mampiz/llm-gateway/actions/workflows/lint.yml/badge.svg)](https://github.com/Mampiz/llm-gateway/actions/workflows/lint.yml)
[![Security](https://github.com/Mampiz/llm-gateway/actions/workflows/security.yml/badge.svg)](https://github.com/Mampiz/llm-gateway/actions/workflows/security.yml)
[![CodeQL](https://github.com/Mampiz/llm-gateway/actions/workflows/codeql.yml/badge.svg)](https://github.com/Mampiz/llm-gateway/actions/workflows/codeql.yml)
[![Release image](https://github.com/Mampiz/llm-gateway/actions/workflows/release.yml/badge.svg)](https://github.com/Mampiz/llm-gateway/actions/workflows/release.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/Mampiz/llm-gateway)](https://goreportcard.com/report/github.com/Mampiz/llm-gateway)
[![Go Reference](https://pkg.go.dev/badge/github.com/Mampiz/llm-gateway.svg)](https://pkg.go.dev/github.com/Mampiz/llm-gateway)
[![Go](https://img.shields.io/github/go-mod/go-version/Mampiz/llm-gateway)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

One OpenAI-compatible endpoint in front of several LLM providers, written to
learn Go concurrency properly rather than to ship another gateway.

## What this demonstrates

A streaming proxy is the smallest realistic program where concurrency is not
optional. It has to read from one socket, write to another, notice when either
end walks away, measure silence, and keep a connection alive through a
long pause — all at once, in one request. Everything here exists to make that
problem concrete.

**Concurrency where a sequential loop cannot reach.** The streaming handler
runs a producer goroutine over the upstream stream and multiplexes its channel
against a heartbeat ticker, an idle timer and the request context in a single
`select`. Two tests exist purely to prove a sequential loop fails: one where
the provider goes silent and one where it is slow enough that the connection
would be dropped without keep-alives.

**Ownership instead of locks.** The race detector caught a real defect during
development — the handler closing a stream while the producer goroutine was
reading it. The fix was to give the stream one owner, not to add a mutex. The
`Stream` contract says so in writing.

**Atomicity that has to be distributed.** The rate limiter is a token bucket
evaluated as a Lua script inside Redis, because read-modify-write from Go is a
race across replicas: two gateways reading "one token left" both allow, and
the effective limit multiplies by the instance count. There is a test that
fails if that ever becomes true again.

**Collapsing duplicate work.** `singleflight` in front of the cache turns a
burst of identical questions into one upstream call. It is the case a plain
lookup cannot help with, because none of the copies has stored anything yet.

**An anti-corruption layer that holds.** OpenAI and Anthropic disagree on the
endpoint, the auth header, where the system prompt lives, whether `max_tokens`
is required, how content is shaped, what the stop reasons are called and how a
stream ends. Each vendor package owns its own dialect; the core knows none of
them. Adding a third provider touches no existing file.

**Failure handled on purpose.** Retries back off with jitter, only retryable
errors fail over, a circuit breaker takes a sick provider out of rotation with
one probe at a time, and a limiter or cache outage degrades rather than
cascades. Each of those is a decision with a reason, written down in
[PROGRESS.md](PROGRESS.md).

## Quick start

Needs Go and nothing else — no API key, no vendor network access, no database:

```bash
./scripts/bootstrap.sh
```

It builds, runs the unit and end-to-end suites, drives the real binary through
49 checks against a fake upstream, and prints a working API key.

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

## Local development

No API key, no network, no bill: a fake upstream in `cmd/fakeupstream` speaks
both dialects at once.

```bash
make dev     # gateway + fake upstream, left running to poke at by hand
make smoke   # start everything, check every route, tear it down again
```

`make smoke` is the one-command answer to "does it actually work": it builds
both binaries, wires them together, drives twenty checks through the real HTTP
surface — routing, translation, validation, upstream failures — prints a
pass/fail line for each, and cleans up after itself.

The fake can also misbehave on request, which is how failure paths get
exercised:

```bash
go run ./cmd/fakeupstream -fail 429      # every call is rate limited
go run ./cmd/fakeupstream -latency 5s    # slow enough to trip timeouts
```

## Routing

One OpenAI-shaped endpoint, several vendors behind it. The provider is chosen
per request from the `model` field:

| Model prefix | Provider |
|---|---|
| `gpt-`, `o1-`, `o3-`, `o4-`, `chatgpt-` | OpenAI |
| `claude-` | Anthropic |
| `mock-` | built-in fake, no key required |

Longer prefixes win over shorter ones, so a specific rule always beats a
general one regardless of wiring order. Models matching nothing fall back to
`GATEWAY_PROVIDER`.

Every vendor speaks its own dialect — different endpoints, auth headers,
message shapes and stop-reason vocabularies. Each provider package owns the
translation to and from the gateway's canonical schema, and no vendor
vocabulary is visible outside it. Request fields the gateway does not model
(`top_p`, `presence_penalty`, `user`, ...) are preserved and forwarded to
providers that understand them rather than being rejected or dropped.

## Authentication

Clients authenticate with keys the gateway issues, never with a provider's
credentials. A leaked client key costs one revocation; a leaked OpenAI key
costs a rotation and whatever was spent in between.

```bash
go run ./cmd/gateway -genkey        # gw_a4573d31...
```

```bash
GATEWAY_API_KEYS="alice:gw_a4573d31...,ci:gw_91ab..." make run
```

```bash
curl localhost:8080/v1/chat/completions \
  -H 'Authorization: Bearer gw_a4573d31...' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'
```

Secrets are stored as SHA-256 digests, so the running process holds nothing
usable. `/healthz` stays open: a probe that needs a credential stops working
exactly when something is wrong. Starting with neither keys nor an explicit
`GATEWAY_AUTH_DISABLED=true` is a startup error, so an open gateway is always
a deliberate choice.

## Rate limiting

Every caller gets a token bucket: up to `GATEWAY_RATE_LIMIT_BURST` requests at
once, refilled continuously at `GATEWAY_RATE_LIMIT_RPS` per second. A bucket
absorbs bursts while capping the sustained rate, which a fixed window per
minute cannot do — there, a caller spends a whole minute's allowance in the
last second of one window and again in the first second of the next.

With `GATEWAY_REDIS_URL` set, the bucket lives in Redis and every replica
shares it. Without it the bucket is per process, so N replicas allow N times
the intended rate — correct for one instance, wrong for a deployment.

The check-and-consume runs as a Lua script inside Redis rather than as
read-modify-write in Go. Two gateways reading "one token left" at the same
instant would both allow their request; a script executes atomically, so the
decision is indivisible however many instances are asking.

Denials carry `Retry-After` and `X-RateLimit-*`, and a limiter outage fails
open: briefly over-serving is a smaller failure than refusing every request
because a side channel is down.

## Automatic fallback

When a provider fails, rate-limits or times out, the request is retried and
then handed to the next model in its chain:

```bash
GATEWAY_FALLBACK_MODELS="gpt-4o-mini:claude-sonnet-5,claude-sonnet-5:gpt-4o-mini"
```

Falling back changes the model as well as the provider, because the same model
rarely exists on two vendors. The client never learns any of it happened.

Three rules keep this from making things worse:

- **Only retryable failures fail over.** A 400 is the caller's mistake;
  asking a second vendor the same malformed question wastes their time too.
- **Backoff is jittered.** Without jitter, every client that failed at the
  same instant retries at the same instant, and the upstream takes a
  synchronised wave exactly when it can least afford one.
- **A circuit breaker takes a failing provider out of rotation.** After
  `GATEWAY_BREAKER_THRESHOLD` consecutive failures it is skipped entirely for
  `GATEWAY_BREAKER_COOLDOWN`, then a single probe decides whether it is back.
  Hammering a struggling upstream slows its recovery and costs every caller
  the latency of a doomed attempt.

`/healthz` reports the state of every circuit, which is the first thing worth
looking at when the gateway starts answering slowly or from the wrong vendor.

Streaming fails over only before the first frame reaches the client. After
that the response is committed, and switching vendors mid-answer would splice
two different completions together.

## Response cache

Off by default; `GATEWAY_CACHE_TTL` turns it on. The key is a digest of
everything that determines the answer, vendor parameters included, so a hit is
always an answer to exactly the same question. Hits are marked `X-Cache: HIT`.

Identical **concurrent** requests are collapsed with `singleflight`: a cold
cache hit by a hundred copies of the same question fetches one answer rather
than a hundred. That is the case a plain lookup misses, because none of the
hundred has stored anything yet.

`GATEWAY_CACHE_SCOPE` chooses between `shared`, which raises the hit rate, and
`caller`, which keeps entries private. Sharing carries a subtle oracle: a
caller can tell that somebody asked a given question by noticing an unusually
fast reply.

Streaming responses are not cached. Replaying one faithfully would need the
frame timing too, and accumulating a copy while forwarding adds a failure mode
to the hot path for a case that repeats far less often.

A cache failure degrades to a miss: a broken cache makes the gateway slower,
never broken.

## Metrics

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

Two decisions worth naming. The duration buckets run to 160 seconds, because
the defaults stop at 10 and would put most streamed answers in `+Inf`. And the
model label is capped at 100 distinct values, collapsing into `other` beyond
that: a label taken from user input is the classic way to kill a Prometheus
server, since one fresh model name per request is one fresh time series per
request.

## Testing

```bash
make test              # unit suite
make test-race         # same, under the race detector
make test-integration  # the Redis-backed limiter, against a real Redis
make e2e               # real binary, real socket, fake upstreams
make smoke             # start everything, 49 checks, tear it down
make cover             # coverage profile + total
make lint              # gofmt, go vet, golangci-lint
make vuln              # govulncheck
make ci                # everything the pipeline runs
```

Unit tests use `httptest.NewServer`, so the whole `net/http` stack runs against
a real loopback listener rather than a mocked transport. The end-to-end suite
under `test/e2e` goes further: it compiles the actual binary, starts it as a
subprocess on a kernel-assigned port with fake upstreams behind it, and drives
it over HTTP. No API key and no network access are needed, and nothing is
billed.

## Security

The gateway holds provider API keys, so security is part of the build:

- **govulncheck** — vulnerabilities reachable from this binary's call graph, in
  dependencies and in the Go standard library itself.
- **Trivy** — known CVEs in the container image, failing on HIGH or CRITICAL
  with a fix available.
- **CodeQL** — static analysis with the security-and-quality query set.
- **Dependabot** — weekly updates for GitHub Actions, Go modules and the base
  image.

The scanners also run on a weekly schedule, because advisories land long after
the last commit. Release images ship with an SBOM and build provenance, and
release binaries with SHA-256 checksums.

To report a vulnerability, see [SECURITY.md](SECURITY.md).

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
cmd/fakeupstream    offline stand-in for both vendor APIs, SSE included
internal/config     environment loading and validation
internal/auth       gateway API keys, stored as digests
internal/ratelimit  token bucket, in-process and Redis-backed
internal/cache      response cache, in-process and Redis-backed
internal/metrics    Prometheus collectors
internal/server     routing, middleware, HTTP handlers, SSE writing
internal/provider   the Provider interface, the canonical schema,
                    the registry, the fallback router and the breaker
  ├── openai       OpenAI-compatible client, buffered and streaming
  ├── anthropic    Messages API client and its translation layer
  └── mock         offline fake provider
scripts/            bootstrap.sh, dev.sh, smoke.sh
test/e2e            end-to-end suite (build tag: e2e)
```

The dependency arrow only ever points inwards: `internal/provider` imports no
vendor package, and Go turns that rule into a compile error rather than a
convention, since the reverse would be an import cycle.

## Where the reasoning lives

[PROGRESS.md](PROGRESS.md) records every design decision and why it was made,
including the ones that were rejected. [PLAN.md](PLAN.md) is the phase plan
with the verifier for each one.

## License

MIT — see [LICENSE](LICENSE).
