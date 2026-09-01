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

A single OpenAI-compatible endpoint in front of several LLM providers: routing, automatic fallback, distributed rate limiting, response caching, streaming and metrics.

## Status

Phase 3 — streaming. Work in progress.

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

## Testing

```bash
make test        # unit suite
make test-race   # same, under the race detector
make e2e         # real binary, real socket, fake upstreams
make cover       # coverage profile + total
make lint        # gofmt, go vet, golangci-lint
make vuln        # govulncheck
make ci          # everything the pipeline runs
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

## License

MIT — see [LICENSE](LICENSE).

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
cmd/fakeupstream    offline stand-in for both vendor APIs
internal/config     environment loading and validation
internal/server     routing, middleware, HTTP handlers
internal/provider   the Provider interface and the shared schema
  ├── openai       OpenAI-compatible client
  ├── anthropic    Messages API client and its translation layer
  └── mock         offline fake provider
scripts/            dev.sh and smoke.sh
test/e2e            end-to-end suite (build tag: e2e)
```
