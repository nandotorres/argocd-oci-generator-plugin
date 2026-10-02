# Metrics

The plugin exposes Prometheus metrics on `GET /metrics`, on the same port as the
API. Like the health probes it is unauthenticated: it carries no artifact data,
and scrapers generally cannot present the plugin's bearer token. Keep it
reachable only from inside the cluster (the Service is `ClusterIP`).

## Exposed series

Standard Go and process collectors, plus:

| Metric | Type | Labels | Use |
|---|---|---|---|
| `ocigen_getparams_requests_total` | counter | `code` | Request volume and failures by HTTP status. |
| `ocigen_getparams_duration_seconds` | histogram | `code` | End-to-end latency; compare with `requestTimeoutSeconds`. |
| `ocigen_generated_parameters` | histogram | - | Parameter sets per successful generation. |
| `ocigen_registry_requests_total` | counter | `operation`, `outcome` | Upstream calls: `catalog`, `list_tags`, `head`, `get`. |
| `ocigen_registry_request_duration_seconds` | histogram | `operation` | Upstream latency, to tell registry slowness from ours. |

Status codes map to the fail-closed contract: `200` with parameters or an empty
list is a normal answer, `400`/`403` is a bad or disallowed request, and `502`
means the generator failed, so Argo CD leaves existing Applications untouched.

A **missing repository counts as `outcome="success"`**: the registry answered,
there is simply nothing published yet. Only a failure to get an answer (auth,
5xx, network) is an error, so a service that has not been onboarded does not
show up as registry trouble.

## Scraping

With the Prometheus Operator:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: oci-generator
  namespace: argocd
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: oci-generator
  endpoints:
    - port: http
      path: /metrics
      interval: 30s
```

Without the operator, annotate the pod template instead:

```yaml
metadata:
  annotations:
    prometheus.io/scrape: "true"
    prometheus.io/port: "8080"
    prometheus.io/path: "/metrics"
```

## Alerts

Three rules cover the failure modes that matter.

```yaml
groups:
  - name: oci-generator
    rules:
      # Generation is failing, so Applications are frozen at their last state.
      # Usually registry or credential trouble.
      - alert: OCIGeneratorGenerationFailing
        expr: sum(rate(ocigen_getparams_requests_total{code=~"5.."}[10m])) > 0
        for: 15m
        labels: { severity: warning }
        annotations:
          summary: OCI generator is returning errors
          description: ApplicationSets keep their current Applications while this persists.

      # Requests are approaching the server timeout; past it, generation fails.
      - alert: OCIGeneratorSlow
        expr: histogram_quantile(0.95, sum by (le) (rate(ocigen_getparams_duration_seconds_bucket[10m]))) > 30
        for: 15m
        labels: { severity: warning }
        annotations:
          summary: OCI generator p95 latency is close to the request timeout
          description: Reduce manifest fetches, or raise requestTimeoutSeconds.

      # The registry is reachable but failing calls.
      - alert: OCIGeneratorRegistryErrors
        expr: |
          sum(rate(ocigen_registry_requests_total{outcome="error"}[10m]))
            / sum(rate(ocigen_registry_requests_total[10m])) > 0.05
        for: 15m
        labels: { severity: warning }
        annotations:
          summary: Over 5% of registry calls are failing
```

Deliberately **not** alerted: `ocigen_generated_parameters` reaching zero. An
empty result is legitimate (tag removed, service not published yet) and it is
how Applications are meant to be pruned. If a given ApplicationSet must never be
empty, express that with `failOnEmpty: true` so it becomes a `502` and is caught
by the first alert.

## Reading the numbers

- Split `ocigen_getparams_duration_seconds` against
  `ocigen_registry_request_duration_seconds` to see whether latency is ours or
  the registry's. It is usually the registry: the work is I/O bound.
- `ocigen_registry_requests_total{operation="get"}` rising means queries are
  fetching manifests (`artifactType`, `annotationSelectors`, `sort: created`),
  which costs a round trip per tag. `head` is the cheap path.
- `operation="catalog"` only appears for wildcard `repository` patterns, which
  need registry catalog support.

See the README's operating notes for measured throughput and sizing guidance.
