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
| `ocigen_generated_parameters` | histogram | - | Parameter sets per successful generation: 1 when the artifact exists, 0 when it does not. |
| `ocigen_registry_requests_total` | counter | `operation`, `outcome` | Upstream calls: `head` (tag resolution) and `list_tags` (repository probe). |
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

An individual empty result is normal: it is how an unpublished environment
yields no Application, and how a decommissioned one is pruned. Many
ApplicationSets going empty at once is not, so alert on the aggregate rather
than on any single generation:

```yaml
      # Normal individually; never normal in bulk.
      - alert: OCIGeneratorManyEmptyResults
        expr: sum(rate(ocigen_generated_parameters_bucket{le="0"}[10m])) > 5
        for: 15m
        labels: { severity: warning }
        annotations:
          summary: Many ApplicationSets are generating no Applications
```

## Reading the numbers

- Split `ocigen_getparams_duration_seconds` against
  `ocigen_registry_request_duration_seconds` to see whether latency is ours or
  the registry's. It is usually the registry: the work is I/O bound.
- `operation="list_tags"` is the probe issued only when a `head` misses, to tell
  an absent tag apart from an absent repository. A rising ratio of it means more
  environments are unpublished than you expect.

See the README's operating notes for measured throughput and sizing guidance.
