# Progress

## Done before this plan

- **Phase 1 · Basic proxy.** Single endpoint forwarding to OpenAI, full HTTP
  cycle, error taxonomy with `Retryable`, graceful shutdown.
- **Phase 2 · Multi-provider normalization.** Anthropic client, canonical
  schema, anti-corruption layer per vendor, model-prefix routing, vendor
  passthrough for unmodelled fields.
- **Phase 3 · Streaming (partial).** SSE reader for OpenAI as an iterator;
  handler multiplexes chunks, heartbeats, idle timeout and cancellation with
  a goroutine, a channel and `select`. Anthropic streaming still missing.
- **Infrastructure.** 6 CI workflows, e2e suite, smoke script, fake upstream,
  10 MB scratch image, release pipeline with SBOM, security scanning.

## Log

### F1 · Anthropic streaming — closed

Implemented `anthropic.ChatStream` and its iterator in a new `stream.go`,
mirroring the OpenAI one. Phase 3 is now complete: both vendors stream.

**Decisions**

- **Dispatch on the payload's `type` field, not the `event:` line.** The
  vendor sends both and they always agree, but keying off the JSON means one
  parser instead of two and no chance of the two drifting apart.
- **Metadata is stitched across events.** `message_start` carries the id, the
  model and `input_tokens`; `message_delta` carries the stop reason and
  `output_tokens`; the total exists in neither. The iterator holds that state
  between calls, which is why it has more fields than the OpenAI one.
- **`message_stop` is the clean end.** There is no `[DONE]` here, so a stream
  that ends without it is reported as truncated rather than complete.
- **A mid-stream `error` event becomes `Stream.Err`.** The vendor reports late
  failures inside a 200 response; surfacing them any other way would let a
  broken answer look finished.
- **Non-text deltas are dropped.** `input_json_delta` and friends belong to
  tool use, which the canonical schema does not model yet.
- **Extracted `mapStopReason`** out of `fromAnthropic` so the buffered and the
  streaming paths share one mapping table instead of two copies that could
  drift. Behaviour unchanged; the existing tests cover it.

**Verifier** — passing.

### F2 · Gateway API keys and authentication — closed

New `internal/auth` package, a `requireAuth` middleware, and the caller's name
carried in the request context for the phases that follow.

**Decisions**

- **Secrets are stored as SHA-256 digests only.** The process never holds a
  usable credential, so a memory dump or a stray state log leaks nothing.
  Comparison goes through `subtle.ConstantTimeCompare`.
- **Refusing to start beats defaulting to open.** With neither
  `GATEWAY_API_KEYS` nor `GATEWAY_AUTH_DISABLED=true`, the gateway exits with a
  message naming both options. An unauthenticated gateway is reachable, but
  only on purpose.
- **`/healthz` is not guarded.** A probe that needs a credential stops working
  exactly when something is wrong.
- **Duplicate secrets are a configuration error.** Two callers sharing one
  credential would make rate limit buckets and audit trails meaningless.
- **`-genkey` lives in the binary.** Issuing a key needs no openssl, no
  scripts, nothing else installed.
- **The 401 never echoes what was presented**, and an unknown key logs
  identically to a malformed one.

**Verifier** — passing. Smoke grew to 29 checks.

### F3 · Distributed rate limiting — closed

New `internal/ratelimit` package with two implementations behind one
interface, plus a middleware that meters every authenticated caller. First
external dependency in the project: `github.com/redis/go-redis/v9`.

**Decisions**

- **Token bucket, not a fixed window.** A window per minute lets a caller
  spend a whole allowance in the last second of one window and again in the
  first second of the next. A bucket absorbs bursts up to its size while
  capping the sustained rate.
- **The bucket runs as a Lua script inside Redis.** Read-modify-write from Go
  is a race: two replicas reading "one token left" at the same instant both
  allow, and the effective limit multiplies by the replica count. Redis
  evaluates a script atomically. `TestRedis_SharesOneBucketAcrossInstances`
  exists specifically to catch a regression here.
- **The clock comes from the gateway, not from Redis.** Keeps the script
  deterministic and replicable; instances need only agree on time to within a
  refill interval, which NTP covers.
- **Buckets carry a TTL.** Otherwise Redis grows one key per caller forever.
- **Metered on the authenticated caller, never on the address.** Clients
  behind one NAT would otherwise share an allowance, and a client that changes
  address would get a fresh one. Forwarding headers are not trusted: anyone
  can set them.
- **Fail open on limiter errors, and log loudly.** A limiter outage becoming a
  gateway outage is the larger failure.
- **Integration tests need a real Redis** and run behind the `integration`
  build tag, with a Redis service container in CI. Faking the script would
  test nothing that matters.

**Verifier** — passing. Smoke grew to 35 checks.

### F4 · Automatic fallback and circuit breaker — closed

New `provider.Router` owning the retry policy and one circuit breaker per
provider, plus `provider.Breaker` as a three-state machine. The handler now
goes through the router instead of resolving a single provider.

**Decisions**

- **Falling back rewrites the model, not just the provider.** `gpt-4o-mini`
  does not exist on Anthropic, so a chain that only swapped vendors would
  always fail. Configured as one variable: `model:fallback|fallback`.
- **Only retryable errors fail over.** `provider.Error.Retryable`, written in
  phase 1 for exactly this, decides. A 400 is returned verbatim and the chain
  stops.
- **Full jitter on the backoff.** The exponential curve matters less than the
  jitter: without it, every client that failed together retries together.
- **One probe at a time in half-open.** Letting a crowd through on the first
  hopeful moment puts the provider straight back under the load that broke it.
- **Cancellation is not a provider failure.** A caller giving up must not
  count against a circuit or spend the rest of the chain.
- **Unresolvable fallback targets are skipped, not fatal.** A chain naming a
  model this deployment does not serve should degrade, not break a request
  that could still be served.
- **Streaming fails over only before the first frame.** After that the status
  is committed and splicing two completions would corrupt the answer.
- **One generic `do` serves both paths**, so buffered and streaming cannot
  drift apart in how they fail over.
- **`/healthz` reports circuit state.** It is the first thing to look at when
  answers start coming from the wrong vendor.

**Verifier** — passing. Smoke grew to 39 checks.

### F5 · Prometheus metrics — closed

New `internal/metrics` package and a `/metrics` endpoint, with recording hooks
in both the buffered and the streaming paths.

**Decisions**

- **The model label is capped at 100 distinct values.** A label taken from user
  input is the classic way to kill a Prometheus server: one fresh model name
  per request is one fresh time series per request. Past the cap everything
  collapses into `other`, losing detail but never the server.
- **`llmgw_rate_limited_total` carries no caller label** for the same reason,
  with the added wrinkle that the dimension would be attacker-controlled.
- **Duration buckets run to 160 seconds.** The client library's defaults stop
  at 10, which would put most streamed answers in `+Inf`.
- **First-token latency is its own histogram.** On a streamed answer the total
  duration is not what a user perceives; the wait for the first word is.
- **Outcomes are a small closed set** (`ok`, `client_error`, `upstream_error`,
  `timeout`, `cancelled`). They are label values, so the set has to stay
  bounded.
- **A request served by nobody is still counted**, under provider `none`,
  or the error rate would hide the outages that never reached a vendor.
- **`/metrics` is unauthenticated**, like `/healthz`: it exposes counts rather
  than content, and a scrape endpoint needing a credential is one more thing
  for a monitoring stack to get wrong.
- **A dedicated registry, not the default one.** The process publishes what it
  chooses to, not whatever a dependency happened to register.

**Verifier** — passing. Smoke grew to 46 checks.

## Discarded ideas

_Out-of-scope thoughts land here instead of in the code._
