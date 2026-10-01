# argocd-oci-generator-plugin

An [ArgoCD ApplicationSet **Plugin Generator**](https://argo-cd.readthedocs.io/en/latest/operator-manual/applicationset/Generators-Plugin/)
that generates Applications from the **artifacts present in an OCI registry** —
giving you Git-generator-style ergonomics (globs, captured path segments) for OCI,
plus OCI-specific narrowing (tags, semver, annotations, artifact types).

> Status: early / pre-release. See [DESIGN.md](DESIGN.md) for the full rationale.

## Why

ArgoCD can generate Applications from Git, clusters, SCM, PRs — but not from the
set of artifacts in an OCI registry. If your teams publish Helm charts or OCI
artifacts (one per app, per tenant, per version, per environment), you can now say:

- *"Create an Application **iff** this tag exists"* (existence check), or
- *"Create one Application for **every** repository/tag matching these rules"*,
  using wildcards whose captured values feed your template.

## Highlights

- **Git-parity globbing with named captures.** Match across the registry namespace
  (`apps-oci/**/{env}`) and tags (`{app}-current`); use `{{ .oci.captures.env }}`,
  `{{ index .oci.wildcards 0 }}` in your template.
- **Security-first, centralized credentials.** Credentials are configured by the
  platform team (never in the ApplicationSet). Basic auth (Artifactory / any
  registry-v2) and ECR (IRSA / instance role + optional STS AssumeRole).
  Per-registry repository allowlists.
- **Fails closed — never deletes on error.** Any fetch/auth/parse failure returns
  a non-2xx response, which ArgoCD treats as a generator error and leaves your
  Applications untouched. (An intentionally empty result is still allowed.)
- **Digest-pinned refs** (`oci.pinnedRef`) for immutable GitOps.
- Tested (unit + component + full-stack integration), distroless image, SBOM,
  cosign-signed releases.

## How it maps

An Artifactory artifact browsed at:

```
https://artifactory.example.com/ui/repos/tree/General/apps-oci/orders-api/orders-api/dev/dev-current
```

is, in OCI terms:

| field      | value                                                     |
|------------|-----------------------------------------------------------|
| registry   | `artifactory.example.com`                                 |
| repository | `apps-oci/orders-api/orders-api/dev`                      |
| tag        | `dev-current`                                             |

## Quick start (local, 60 seconds)

Requires Docker + Go.

```bash
make smoke
```

This spins up a throwaway registry, publishes a few artifacts, runs the plugin,
and calls `getparams.execute` for the existence-check and wildcard scenarios.

Run the server by hand:

```bash
PLUGIN_TOKEN=dev-token make dev   # serves on :8080 with deploy/config.example.yaml
```

## Parameter reference

### Input (ApplicationSet `plugin.input.parameters`)

| key                   | type       | description                                                     |
|-----------------------|------------|-----------------------------------------------------------------|
| `repository`          | string     | **required.** Repository path or glob (`apps-oci/**/{env}`).    |
| `registry`            | string     | Registry host. Defaults to the server's `defaultRegistry`.      |
| `tags`                | []string   | Exact-match allowlist (existence check).                        |
| `tagPattern`          | string     | Tag glob with captures (`{app}-current`).                       |
| `tagFilters`          | []filter   | AND of `{regex}` / `{semver}` filters.                          |
| `excludeTagFilters`   | []filter   | Any match excludes the tag.                                     |
| `artifactType`        | string     | Filter by OCI artifactType (or config media type).             |
| `annotationSelectors` | []selector | `{key, operator: In\|NotIn\|Exists\|DoesNotExist, values}`.     |
| `sort`                | string     | `semver` \| `alpha` \| `created`.                               |
| `order`               | string     | `asc` \| `desc`.                                                |
| `limit`               | int        | Keep first N after sorting.                                     |
| `failOnEmpty`         | bool       | Treat an empty result as an error (never delete apps).          |

Glob syntax (not regex; `.` is literal): `*` one segment, `**` zero+ segments
(repository only), `?` one char, `{name}` a named capture.

### Output (per matched artifact)

```yaml
oci:
  registry, repository, tag, digest
  ref            # registry/repo:tag
  pinnedRef      # registry/repo@sha256:...   (use this for immutability)
  mediaType, artifactType, createdAt
  annotations: { ... }
  semver: { major, minor, patch, prerelease, metadata }   # when tag is semver
  captures:   { <name>: <value> }     # named captures (repo + tag)
  wildcards:  [ ... ]                  # anonymous * / ** captures, in order
  repositorySegments: [ ... ]
```

With `goTemplate: false`, ArgoCD flattens these to dot-style keys
(`oci.tag`, `oci.captures.env`, ...), mirroring the Git generator.

## Install into ArgoCD

```bash
kubectl apply -k deploy/manifests            # Deployment, Service, ConfigMaps
# create the secrets out-of-band (see deploy/manifests/secret.example.yaml)
kubectl apply -f deploy/examples/applicationset-wildcard.yaml
```

Configuration lives in two places:

- **Plugin discovery** (`deploy/manifests/configmap-plugin.yaml`): the ConfigMap
  ArgoCD reads (`baseUrl`, `token`, `requestTimeout`), labeled
  `app.kubernetes.io/part-of: argocd`.
- **Server config** (`deploy/manifests/configmap-server.yaml` +
  `deploy/config.example.yaml`): registries, auth, allowlists. Secrets are
  injected via env vars (`${VAR}`).

### ECR

Set `auth.type: ecr`, a `region`, and (optionally) a `roleArn` to assume. The pod
uses its AWS identity (IRSA recommended) to call `GetAuthorizationToken`; tokens
are cached until shortly before expiry.

## Development

```bash
make test     # unit + integration
make race     # race detector
make lint     # golangci-lint
make cover    # coverage summary
make image    # container image
```

Architecture (see [DESIGN.md](DESIGN.md)):

```
cmd/plugin            entrypoint
internal/config       config load/validate + ${ENV} expansion
internal/server       HTTP contract, bearer auth, fail-closed error mapping
internal/auth         basic / ecr / anonymous authenticators
internal/registry     go-containerregistry wrapper (catalog, tags, manifests)
internal/generator    input model, filters, sort/limit, orchestration
internal/pattern      glob-with-captures matcher (the crux)
internal/oci          Artifact value object + parameter mapping
```

## License

Apache-2.0. See [LICENSE](LICENSE).
