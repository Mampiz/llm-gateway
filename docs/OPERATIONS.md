# Operations

What to look at when something is wrong, and which knob moves it.

## First look

```bash
curl -s localhost:8080/healthz | jq
```

```json
{
  "status": "ok",
  "version": "v1.0.0",
  "providers": ["anthropic", "mock", "openai"],
  "circuits": { "openai": "closed", "anthropic": "closed" }
}
```

`circuits` is the field worth reading first. A provider showing `open` is out of
rotation, which explains both unexpected latency and answers arriving from the
wrong vendor. `half-open` means a probe is deciding whether it is back.

The endpoint needs no credential: a probe that stops working exactly when
something is wrong is not a probe.

## Symptoms

### Answers come from the wrong provider

The primary's circuit is open. Confirm with `/healthz`, then look at
`llmgw_upstream_errors_total` to see which status it was failing with. The
circuit closes on its own once a probe succeeds; there is nothing to restart.

### Latency jumped but nothing is failing

Compare `llmgw_stream_first_token_seconds` with
`llmgw_request_duration_seconds`. If only the total moved, the upstream is
generating more slowly and there is nothing to fix here. If the first token
moved too, the provider is slow to start, and `GATEWAY_FALLBACK_MODELS` is what
routes around it.

Check `llmgw_requests_in_flight` as well: if it is climbing, requests are
arriving faster than they finish and the process will run out of memory before
it runs out of patience.

### Clients see 429s that the vendor did not send

That is this gateway's own limiter. `llmgw_rate_limited_total` counts them.
Either the caller is genuinely over its allowance, or the limit is too tight:

```bash
GATEWAY_RATE_LIMIT_RPS=20 GATEWAY_RATE_LIMIT_BURST=40
```

With more than one replica, check `GATEWAY_REDIS_URL` is set. Without it each
replica applies the limit independently, so the effective rate is the limit
times the number of instances.

### Streams die after a minute

`GATEWAY_STREAM_IDLE_TIMEOUT` measures silence, not total time, so a long but
healthy generation never trips it. If it does trip, the upstream really did stop
sending. A stream cut at exactly `GATEWAY_STREAM_MAX_DURATION` is the hard cap
instead.

### Connections drop through a proxy mid-answer

The keep-alive interval is longer than the proxy's idle timeout. Lower
`GATEWAY_STREAM_HEARTBEAT` below it. The gateway also sends
`X-Accel-Buffering: no`, which nginx honours; other proxies may need their own
buffering switched off, and a buffered stream looks exactly like a hung one.

### The cache never hits

Hits are marked `X-Cache: HIT`. Misses on requests that look identical mean the
key differs, and the key covers every parameter that changes the answer,
vendor-specific ones included. `GATEWAY_CACHE_SCOPE=caller` also means two
callers never share an entry.

## Metrics worth alerting on

| Expression | Why |
|---|---|
| `rate(llmgw_requests_total{outcome="upstream_error"}[5m]) / rate(llmgw_requests_total[5m])` | error rate, the top-level signal |
| `llmgw_circuit_state > 0` | a provider is out of rotation |
| `histogram_quantile(0.95, rate(llmgw_stream_first_token_seconds_bucket[5m]))` | the latency users feel |
| `rate(llmgw_rate_limited_total[5m])` | callers hitting the ceiling |
| `llmgw_requests_in_flight` | concurrency, and the shape of a pile-up |
| `sum by (provider) (rate(llmgw_tokens_total[1h]))` | spend, per vendor |

Ready-made rules are in [`deploy/prometheus-rules.yaml`](../deploy/prometheus-rules.yaml)
and a dashboard in [`deploy/grafana-dashboard.json`](../deploy/grafana-dashboard.json).

## Deploying

A rolling deploy sends SIGTERM. The gateway then tells in-flight streams to wind
up, answers them with an error frame rather than cutting the connection, and
waits up to 20 seconds for the rest. It exits zero either way, so an expired
grace period is not reported as a crash.

Give the pod a `terminationGracePeriodSeconds` above that 20 seconds, or the
orchestrator will send SIGKILL while the gateway is still draining.

## Scaling

Stateless apart from what is in Redis, so replicas scale horizontally. Two
things change with more than one:

- **Set `GATEWAY_REDIS_URL`.** Without it the rate limit is per instance.
- **Circuits stay per instance.** A provider failing for one replica does not
  take it away from the others, which is intentional.

## Credentials

Client keys are `name:secret` pairs in `GATEWAY_API_KEYS`, held only as SHA-256
digests. Rotating one means issuing a new key, adding it alongside the old,
moving the caller across, then removing the old:

```bash
go run ./cmd/gateway -genkey
```

Vendor keys never reach clients and clients never send them. A leaked client key
costs one revocation; a leaked vendor key costs a rotation and whatever was
spent in between.

Starting with neither keys nor `GATEWAY_AUTH_DISABLED=true` is a startup error,
so an unauthenticated gateway is always deliberate.
