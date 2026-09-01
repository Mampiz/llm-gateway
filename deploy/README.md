# Deploying

## Helm

```bash
helm install gw ./deploy/helm/llm-gateway \
  --set secrets.gatewayApiKeys="alice:$(go run ../cmd/gateway -genkey)" \
  --set secrets.openaiApiKey="sk-..." \
  --set config.provider=openai
```

The chart brings its own Redis, which the rate limiter and the cache share.
Point `redis.url` at one you manage to use that instead, or set
`redis.enabled=false` to run without — at which point both become per instance,
and N replicas allow N times the intended rate. The chart says so on install.

Credentials belong in a Secret you manage. `secrets.existingSecret` takes one
that already holds `OPENAI_API_KEY`, `ANTHROPIC_API_KEY` and
`GATEWAY_API_KEYS`; putting them in `values.yaml` means putting them wherever
that file ends up.

```bash
make chart
```

That lints the chart and validates what it renders with `kubeconform`, which
checks against the published Kubernetes schemas without needing a cluster.
`kubectl apply --dry-run=client` sounds offline but still fetches the OpenAPI
document from an API server, so it cannot stand in for this in CI.

CI runs the same on every push, including the non-default value paths.

### What the chart sets that is easy to get wrong

- **`terminationGracePeriodSeconds: 40`.** On SIGTERM the gateway tells
  in-flight streams to wind up and waits 20 seconds for the rest. A shorter
  grace period sends SIGKILL mid-drain and cuts every answer in progress.
- **A `startupProbe` separate from the liveness one.** Without it a slow start
  reads as a crash loop.
- **`readOnlyRootFilesystem` and all capabilities dropped.** The image is built
  from `scratch`: no shell, no package manager, nothing writable.
- **A config checksum annotation**, so changing the ConfigMap rolls the pods.
  Updating it alone would leave the old values running.
- **Redis with `allkeys-lru` and no persistence.** Both users are caches:
  losing bucket state briefly gives everyone a full allowance, and losing the
  cache means paying for a few answers again. Neither is worth a volume, and an
  unbounded Redis turns a cost into an outage.

## Observability

- [`prometheus-rules.yaml`](prometheus-rules.yaml) — alerts on symptoms a user
  would notice, not on causes. A slow provider only matters if it is making
  answers slow.
- [`grafana-dashboard.json`](grafana-dashboard.json) — import into Grafana and
  pick a Prometheus data source.

Set `serviceMonitor.enabled=true` if the Prometheus Operator is installed.

## Without Kubernetes

```bash
docker compose up --build
```

Same shape: the gateway, and a Redis behind it.
