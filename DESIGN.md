# Design

## 1. Problem

Argo CD's ApplicationSet can generate Applications from Git, clusters, SCM
providers, and pull requests, but not from the set of artifacts in an OCI
registry. Teams that publish Helm charts or other OCI artifacts — one per app,
version, tenant, or environment — have no first-class way to say "create or
refresh an Application for every artifact that matches these rules".

This project provides that as an ApplicationSet plugin generator: an HTTP service
the ApplicationSet controller calls, with parity to the Git generator plus
OCI-specific narrowing.

## 2. How plugin generators work

The controller calls an HTTP service:

- `POST {baseUrl}/api/v1/getparams.execute`
- Header `Authorization: Bearer <token>`
- Body: `{"applicationSetName": "<name>", "input": {"parameters": { ... }}}`
- Response: `{"output": {"parameters": [ {..}, {..} ]}}`

The plugin is referenced from the ApplicationSet via a ConfigMap (keys `baseUrl`,
`token`, optional `requestTimeout`) named in `plugin.configMapRef`. For each
returned parameter map the controller renders the template — flattened to
dot-style keys when `goTemplate: false`, or kept nested when `goTemplate: true`.

### 2.1 Do not delete on error

This is the property the whole design turns on. When generation returns an
**error**, the controller records an error condition and returns early: it does
not create, update, or delete anything, and existing Applications are left in
place. A **successful but empty** result, on the other hand, deletes everything
the ApplicationSet owns.

So the rule is: any fetch, auth, parse, or partial failure must surface as a
non-2xx response, which the controller treats as a generator error and makes no
changes. A truncated or best-effort list is never returned as a 200.

## 3. Scope

In scope:

- List tags in an OCI repository and emit one parameter set per matching artifact.
- Narrowing rules: tag regex, semver constraints, artifact/media-type filter,
  annotation matching, include/exclude, sort, and limit (for example "latest N").
- Centralized credentials: basic auth (any registry-v2 with user/password) and
  ECR (IRSA / instance role, optionally assuming a role).
- Fail closed (§2.1). Structured logging and health endpoints.

Out of scope:

- Pushing or mutating artifacts; this is read-only.
- Per-ApplicationSet credentials in the spec (see §5).
- Non-OCI registries.

## 4. Parameter model

### 4.0 Existence check

The simplest case: create an Application only if a specific tag exists. Given a
reference `registry.example.com/my-org/my-app:stable`, the ApplicationSet is:

```yaml
generators:
  - plugin:
      configMapRef: { name: oci-generator }
      requeueAfterSeconds: 120
      input:
        parameters:
          registry: registry.example.com
          repository: my-org/my-app
          tags: [stable]        # exact-match existence check
```

Behavior: tag present → one Application; tag absent but registry reachable → zero
Applications; registry or auth failure → generator error → no Applications
deleted (§2.1).

### 4.1 Input

```yaml
generators:
  - plugin:
      configMapRef: { name: oci-generator }
      requeueAfterSeconds: 300
      input:
        parameters:
          repository: my-org/my-app          # required
          registry: registry.example.com     # optional; else server default
          tags: [stable, dev]                # optional exact-match (existence check)
          tagFilters:                        # all must match (AND)
            - regex: "^v\\d+\\.\\d+\\.\\d+$"
            - semver: ">= 1.2.0, < 2.0.0"
          excludeTagFilters:                 # any match excludes the tag
            - regex: ".*-rc.*"
          artifactType: application/vnd.cncf.helm.config.v1+json  # optional
          annotationSelectors:               # OCI annotations (AND)
            - key: org.opencontainers.image.vendor
              operator: In                   # In | NotIn | Exists | DoesNotExist
              values: [my-org]
          sort: semver                       # semver | alpha | created
          order: desc                        # asc | desc
          limit: 20                          # keep first N after sort
```

Input is validated strictly; unknown or invalid fields are an error (§2.1).

### 4.2 Output

One parameter map per artifact, nested so both `goTemplate` modes work:

```yaml
oci:
  registry:   registry.example.com
  repository: my-org/my-app
  tag:        v1.4.2
  digest:     sha256:...
  ref:        registry.example.com/my-org/my-app:v1.4.2
  pinnedRef:  registry.example.com/my-org/my-app@sha256:...   # pin to this for GitOps
  mediaType:  application/vnd.oci.image.manifest.v1+json
  artifactType: application/vnd.cncf.helm.config.v1+json
  createdAt:  2024-05-01T12:00:00Z                     # from org.opencontainers.image.created
  annotations:
    org.opencontainers.image.revision: abc123
  semver:                                              # present when the tag is semver
    major: 1
    minor: 4
    patch: 2
    prerelease: ""
```

### 4.3 Wildcards and captures

The Git directory generator lets you write `some/repo/*/env/*` and consume the
matched segments in the template. This keeps that ergonomics for OCI and adds
named captures, across two axes:

- repository namespace, enumerated from the registry catalog:
  `repository: "apps/{team}/**/{env}"`
- tag: `tagPattern: "{name}-current"`

Glob syntax (not regex; `.` is literal):

| token    | meaning                                              | captured         |
|----------|------------------------------------------------------|------------------|
| `*`      | one path segment, or a run within a tag              | yes (positional) |
| `**`     | zero or more path segments (repository only)         | yes (positional) |
| `?`      | a single character                                   | no               |
| `{name}` | one segment (repository) or a run (tag), named       | yes              |
| other    | literal                                              | —                |

Discovery:

- A `repository` with no wildcard is queried directly.
- A `repository` with wildcards lists the registry catalog (`GET /v2/_catalog`),
  filters it by the literal prefix before the first wildcard, then matches the
  compiled pattern. Catalog failures are an error (§2.1).
- For each matched repository, tags are listed and matched against `tagPattern`,
  `tags`, and `tagFilters`.

Captures are added to the output:

```yaml
oci:
  captures:                    # merged named captures (repository + tag)
    team: payments
    env: prod
    name: web
  wildcards: [payments/x, prod]   # positional captures, in pattern order
  repositorySegments: [apps, payments, x, prod]
```

Template usage: `{{ .oci.captures.env }}`, `{{ index .oci.wildcards 0 }}`.
Duplicate capture names across the repository and tag patterns are rejected at
request time (§2.1).

## 5. Security model

ApplicationSets are often self-service. If auth lived in the ApplicationSet spec,
any author could point the plugin at arbitrary registries with platform
credentials, or try to exfiltrate tokens through a crafted template. So:

- The platform team configures a server-side registry/credential map. The
  ApplicationSet only chooses a `registry` and `repository`, both validated.
- Credentials come from mounted Secrets, env, or cloud identity — never from the
  request.
- Auth providers, per registry host:
  - `basic` — username/password, from env/Secret references.
  - `ecr` — the AWS credential chain (IRSA, env, instance profile); optional
    `roleArn` to assume a role before calling `GetAuthorizationToken`.
  - `anonymous` — public registries.
- Optional `allowedRepositories` per registry (glob) constrains what an
  ApplicationSet may target.
- The plugin's bearer `token` is checked on every request with a constant-time
  compare.

Example server config:

```yaml
listen: :8080
defaultRegistry: registry.example.com
registries:
  - host: registry.example.com
    auth:
      type: basic
      username: ${REGISTRY_USER}
      password: ${REGISTRY_PASS}
    allowedRepositories: ["my-org/*"]
  - host: 123456789012.dkr.ecr.us-east-1.amazonaws.com
    auth:
      type: ecr
      region: us-east-1
      roleArn: arn:aws:iam::123456789012:role/argocd-ecr-reader   # optional
tls:
  insecureSkipVerify: false
```

## 6. Architecture

```
cmd/plugin          load config, wire dependencies, run the server
internal/config     config load/validate + ${VAR} expansion
internal/server     HTTP server: bearer auth, /api/v1/getparams.execute, health
internal/auth       Authenticator interface + basic/ecr/anonymous providers
internal/registry   go-containerregistry wrapper: catalog, tags, manifests
internal/generator  apply input rules → registry queries → filter/sort/limit
internal/oci        artifact value object + parameter mapping
internal/pattern    glob-with-captures matcher
```

Main dependencies:

- `github.com/google/go-containerregistry` — registry client, `authn`
  abstraction, and an in-memory registry for tests.
- `github.com/aws/aws-sdk-go-v2` — AWS config, STS AssumeRole, and ECR
  `GetAuthorizationToken`.
- `github.com/Masterminds/semver/v3` — semver parsing, constraints, and sorting.
- stdlib `log/slog` and `net/http`.

## 7. Error handling

- Any error from config, auth, registry calls, or rule evaluation becomes a
  4xx/5xx response with `{"error": "..."}`, which the controller treats as a
  generator error (no deletions).
- Client errors (bad input, disallowed repository) are 400/403; upstream/registry
  failures are 502/504. Both prevent deletion; the distinction only helps
  debugging.
- An empty-but-valid result (the repository genuinely has no matching tags) is a
  200 with `[]`. This is the one case where deletion is intended. Set
  `failOnEmpty` to turn an empty result into an error instead.

## 8. Testing

- Unit: filters (regex/semver/annotation), sort/limit, parameter mapping, config
  validation, auth provider selection.
- Component: push fake artifacts into an in-memory registry and run the generator
  end-to-end over HTTP; assert output and fail-closed behavior.
- ECR: the AssumeRole/token wiring with mocked STS and ECR clients.
- The HTTP request/response contract.

## 9. Release

- Multi-arch container image (minimal, non-root), SBOM, and cosign signing.
- `goreleaser` for binaries and image; GitHub Actions for CI and release.
- Raw manifests (Deployment, Service, ConfigMaps, Secret example).
- Image published to GHCR.
