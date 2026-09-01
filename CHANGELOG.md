# Changelog

Notable changes to llm-gateway. Format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.0] 2026-09-01

The first release with every capability in place and verified end to end.

### Added

- **One OpenAI-compatible endpoint** in front of OpenAI and Anthropic, with the
  provider chosen per request from the model name. Longest prefix wins, so
  wiring order never changes routing.
- **A canonical schema and an anti-corruption layer per vendor.** Endpoints,
  auth headers, system prompt placement, content shape, stop-reason vocabulary
  and stream framing all differ; none of that vocabulary leaves its package.
- **Streaming over SSE for both vendors**, with heartbeats, an idle timeout, a
  hard cap on total duration, and prompt teardown when a client disconnects.
- **Gateway-issued API keys**, held as SHA-256 digests, with a `-genkey` flag.
- **Per-caller rate limiting** as a token bucket, evaluated as a Lua script
  inside Redis so the limit holds across replicas.
- **Automatic fallback** with jittered exponential backoff and a per-provider
  circuit breaker.
- **A response cache** with `singleflight`, so a burst of identical questions
  costs one upstream call.
- **Prometheus metrics** at `/metrics`, including time to first token.
- **A Helm chart, alerting rules and a Grafana dashboard** under `deploy/`.
- **Vendor parameter passthrough**: fields the gateway does not model reach the
  providers that understand them rather than being rejected or dropped.

### Fixed

Found by auditing every package, each with the test that reproduces it:

- A cancelled probe left a circuit stranded half-open forever, so a recovered
  provider never returned to rotation.
- The in-process rate limiter grew a bucket per caller without bound, and its
  sweep was O(n) per request once over the bound.
- `singleflight` failed every waiting caller when whichever one happened to
  arrive first disconnected.
- A streamed answer had no cap on total duration, and the multiplexer did not
  watch its own context, so a departed client kept paying for tokens.
- A panic partway through a response spliced an error object into it; it now
  aborts the connection instead.
- SIGTERM during any stream exited non-zero, which an orchestrator reads as a
  crash. Streams are now told to wind up first.
- Zero-valued duration settings were accepted and quietly disabled the gateway.
- Response bodies were closed without being drained, so connections could not
  be reused.
- The smoke script reported found matches as failures: `grep -q` closing the
  pipe killed `printf` with SIGPIPE, and `set -o pipefail` promoted that to the
  pipeline's status.

### Changed

- Verification scripts print in English, matching the rest of the repository.

### Security

- Client keys are never stored or logged in plaintext, and a rejected key is
  never echoed back.
- Starting with neither keys nor an explicit opt-out is a startup error.
- Request bodies, upstream error bodies and SSE lines are all read under caps.
- Vendor parameter passthrough cannot overwrite a field the gateway controls.
- The container is built from `scratch` and runs unprivileged with a read-only
  root filesystem.

[1.0.0]: https://github.com/Mampiz/llm-gateway/releases/tag/v1.0.0
