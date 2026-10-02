# argocd-oci-generator-plugin

An [Argo CD ApplicationSet plugin generator](https://argo-cd.readthedocs.io/en/latest/operator-manual/applicationset/Generators-Plugin/)
that generates Applications from the contents of an OCI registry. It lists the
repositories and tags in a registry, applies a set of narrowing rules, and emits
one parameter set per matching artifact for your ApplicationSet template.

Argo CD ships generators for Git, clusters, SCM providers, and pull requests, but
not for OCI registries. If you publish Helm charts or other OCI artifacts, this
fills that gap with two common shapes:

- an **existence check** — create an Application only if a given tag exists, and
- **discovery** — create one Application for every repository/tag that matches a
  glob, with the matched values available to the template.

Status: pre-release. See [DESIGN.md](DESIGN.md) for the rationale and internals.

## How an OCI reference maps to parameters

A tag-qualified OCI reference such as `registry.example.com/my-org/my-app:1.4.2`
splits into three parts:

| field        | value                   | notes                                  |
|--------------|-------------------------|----------------------------------------|
| `registry`   | `registry.example.com`  | the host                               |
| `repository` | `my-org/my-app`         | everything between the host and the tag |
| `tag`        | `1.4.2`                 | after the final `:`                    |

The repository can have any number of path segments, for example
`registry.example.com/team/group/app`.

## Pattern matching

Repositories and tags are matched with globs, not regexes (`.` is literal):

| token    | meaning                                              | captured              |
|----------|------------------------------------------------------|-----------------------|
| `*`      | one path segment, or a run within a tag              | yes (positional)      |
| `**`     | zero or more path segments (repository only)         | yes (positional)      |
| `?`      | a single character                                   | no                    |
| `{name}` | one segment (repository) or a run (tag), named       | yes (`captures.name`) |
| other    | literal                                              | —                     |

For example `repository: "apps/**/{env}"` with `tagPattern: "{app}-current"`
matches repositories like `apps/team/web/prod` and tags like `web-current`, and
exposes `{{ .oci.captures.env }}`, `{{ .oci.captures.app }}`, and the positional
`{{ index .oci.wildcards 0 }}` in the template.

## Quick start

Requires Docker and Go.

```bash
make smoke        # throwaway registry + a few artifacts, exercise getparams.execute
PLUGIN_TOKEN=dev-token make dev   # run the server on :8080 with deploy/config.example.yaml
```

For a full cluster walkthrough (kind + Argo CD + registry + plugin), see
`make e2e`.

## Parameters

### Input — `plugin.input.parameters`

| key                   | type       | description                                                 |
|-----------------------|------------|-------------------------------------------------------------|
| `repository`          | string     | Required. Repository path or glob.                          |
| `registry`            | string     | Registry host. Defaults to the server's `defaultRegistry`.  |
| `tags`                | []string   | Exact-match allowlist (existence check).                    |
| `tagPattern`          | string     | Tag glob with captures.                                     |
| `tagFilters`          | []filter   | `{regex}` / `{semver}`, all must match.                     |
| `excludeTagFilters`   | []filter   | Any match excludes the tag.                                 |
| `artifactType`        | string     | Filter by OCI `artifactType` (or config media type).        |
| `annotationSelectors` | []selector | `{key, operator: In\|NotIn\|Exists\|DoesNotExist, values}`. |
| `sort`                | string     | `semver` \| `alpha` \| `created`.                           |
| `order`               | string     | `asc` \| `desc`.                                            |
| `limit`               | int        | Keep the first N after sorting.                             |
| `failOnEmpty`         | bool       | Treat an empty result as an error instead of deleting apps. |

### Output — one map per matched artifact

```yaml
oci:
  registry, repository, tag, digest
  ref            # registry/repository:tag
  pinnedRef      # registry/repository@sha256:...   (pin to this for immutability)
  mediaType, artifactType, createdAt
  annotations: { ... }
  semver: { major, minor, patch, prerelease, metadata }   # when the tag is semver
  captures:   { <name>: <value> }     # named captures (repository + tag)
  wildcards:  [ ... ]                  # positional * / ** captures, in order
  repositorySegments: [ ... ]
```

With `goTemplate: false`, Argo CD flattens these to dot-style keys (`oci.tag`,
`oci.captures.env`, ...), as the Git generator does.

See `deploy/examples/` for existence-check, wildcard, and matrix ApplicationSets.

## Install

```bash
kubectl apply -k deploy/manifests            # Deployment, Service, ConfigMaps
# create the Secrets separately (see deploy/manifests/secret.example.yaml)
kubectl apply -f deploy/examples/applicationset-wildcard.yaml
```

Configuration lives in two ConfigMaps:

- **Plugin discovery** (`configmap-plugin.yaml`): what Argo CD reads to find the
  plugin (`baseUrl`, `token`, `requestTimeout`), labeled
  `app.kubernetes.io/part-of: argocd`.
- **Server config** (`configmap-server.yaml`, see `deploy/config.example.yaml`):
  registries, auth, and repository allowlists. Credentials are referenced as
  `${VAR}` and resolved from Secrets via the Deployment's env.

### ECR

Set `auth.type: ecr`, a `region`, and optionally a `roleArn` to assume. The pod
uses its AWS identity (IRSA or EKS Pod Identity recommended) to call
`GetAuthorizationToken`; tokens are cached until shortly before they expire.
`deploy/examples/ecr/` has a complete setup: IRSA / Pod Identity, same- and
cross-account, a least-privilege IAM policy, trust policies, and a matching
ApplicationSet.

## Development

```bash
make test     # unit + integration
make race     # race detector
make lint     # golangci-lint
make cover    # coverage summary
make image    # container image
```

Layout:

```
cmd/plugin          entrypoint
internal/config     config load/validate + ${VAR} expansion
internal/server     HTTP contract, bearer auth, fail-closed error mapping
internal/auth       basic / ecr / anonymous authenticators
internal/registry   go-containerregistry wrapper (catalog, tags, manifests)
internal/generator  input model, filters, sort/limit, orchestration
internal/pattern    glob-with-captures matcher
internal/oci        artifact value object + parameter mapping
```

## License

Apache-2.0. See [LICENSE](LICENSE).
