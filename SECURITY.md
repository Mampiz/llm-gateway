# Security Policy

## Reporting a vulnerability

Please report security issues privately through
[GitHub Security Advisories](https://github.com/Mampiz/llm-gateway/security/advisories/new)
rather than opening a public issue.

Include what you did, what happened, and what you expected. A proof of concept
helps but is not required. Expect an acknowledgement within a few days.

## What this project handles

The gateway sits between client applications and upstream LLM providers, which
makes it a credential-handling component. Findings in these areas are
especially welcome:

- **Provider API keys.** They are read from the environment at startup, held in
  memory and sent only to their own provider's endpoint. They must never appear
  in logs, error messages or responses returned to clients.
- **Request passthrough.** Fields the gateway does not model are forwarded to
  providers that understand them. The merge is defensive: an unmodelled key can
  never overwrite a field the gateway controls. A way around that is a bug.
- **Upstream error propagation.** Vendor error messages reach the client. A
  vendor leaking secrets into an error message would leak them through here too.
- **Resource exhaustion.** Request bodies and upstream error bodies are read
  under explicit caps. An unbounded read is a bug.

## What is out of scope

- Vulnerabilities in the upstream provider APIs themselves.
- Running the gateway without authentication on a public network. Client
  authentication and rate limiting are not implemented yet; until then the
  gateway is meant to run inside a trusted perimeter.

## Automated scanning

Every push and a weekly schedule run `govulncheck` (vulnerabilities reachable
from this binary's call graph), Trivy (container image CVEs) and CodeQL (static
analysis). Dependabot keeps GitHub Actions and Go modules current.
