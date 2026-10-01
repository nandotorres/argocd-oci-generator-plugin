# ArgoCD OCI ApplicationSet Generator Plugin — Design

> Status: **draft**. This document drives the implementation and the eventual blog post.

## 1. Problem

ArgoCD's `ApplicationSet` can generate Applications from Git (directories/files), clusters,
SCM providers, pull requests, etc. There is **no first-class way to generate Applications from
the set of artifacts present in an OCI registry**. Teams that publish Helm charts / OCI artifacts
(one per app, one per version, one per tenant, ...) cannot say "create/refresh an Application for
every artifact matching these rules".

This project delivers that capability as an **ApplicationSet Plugin Generator** (an out-of-tree
HTTP service the ApplicationSet controller calls), giving parity with the Git generator plus
OCI-specific narrowing rules.

## 2. How ApplicationSet plugin generators work (recap)

The controller (`applicationset/generators/plugin.go`) talks to an HTTP service:

- `POST {baseUrl}/api/v1/getparams.execute`
- Header `Authorization: Bearer <token>`
- Body: `{"applicationSetName": "<name>", "input": {"parameters": { ...user rules... }}}`
- Response: `{"output": {"parameters": [ {..}, {..} ]}}`

The plugin is wired via a `ConfigMap` (keys `baseUrl`, `token`, optional `requestTimeout`)
referenced from the ApplicationSet's `plugin.configMapRef`.

The controller then, for each returned parameter map, flattens it (dot-style) when
`goTemplate: false`, or keeps it nested for `goTemplate: true`, and renders the template.

### 2.1 The critical safety property (do-not-delete-on-error)

`applicationset_controller.go` (~L199): if generation returns an **error**, the controller sets an
error condition and **returns early — it never runs create/update/delete**. Existing Applications
are left untouched. But a **successful empty result deletes everything** the appset owns.

➡️ **Design rule #1:** any fetch/auth/parse/partial failure MUST surface as a non-2xx HTTP
response (→ generator error → no deletions). We never "best-effort" a truncated list into a 200.

## 3. Responsibilities & non-goals

In scope:
- List artifacts (tags) in an OCI repository and emit one parameter set per matching artifact.
- Narrowing rules: tag regex, semver constraints, artifact/media-type filter, annotation match,
  include/exclude semantics, sort + limit (e.g. "latest N").
- Security-first, **centralized** credentials: basic auth (Artifactory / any user:pass registry)
  and ECR (IRSA / instance role + optional STS AssumeRole).
- Fail closed (rule #1). Structured logging, metrics, health endpoints.

Out of scope (v1):
- Pushing/mutating artifacts. Read-only.
- Per-ApplicationSet credentials embedded in the appset spec (see §5 threat model).
- Non-OCI registries.

## 4. Parameter model

### 4.0 Canonical example (existence check)

Given this Artifactory artifact that a team wants an App created for *iff it exists*:

```
https://artifactory.example.com/ui/repos/tree/General/
  apps-oci/orders-api/orders-api/dev/dev-current
  └ repo key ────┘└──────── image path ────────────┘ └ tag ┘
```

maps to:

| field      | value                                                              |
|------------|--------------------------------------------------------------------|
| registry   | `artifactory.example.com`                                     |
| repository | `apps-oci/orders-api/orders-api/dev`         |
| tag        | `dev-current`                                                      |
| ref        | `artifactory.example.com/apps-oci/.../dev:dev-current`|

ApplicationSet snippet:

```yaml
generators:
  - plugin:
      configMapRef: { name: oci-generator }
      requeueAfterSeconds: 120
      input:
        parameters:
          registry: artifactory.example.com
          repository: apps-oci/orders-api/orders-api/dev
          tags: [dev-current]        # exact-match convenience; existence check
```

Behavior: tag present -> 1 App; tag absent (registry OK) -> 0 Apps; registry/auth
failure -> generator error -> **no Apps deleted** (rule #1).

### 4.1 Input (from the ApplicationSet `plugin.input.parameters`)

```yaml
generators:
  - plugin:
      configMapRef: { name: oci-generator }
      requeueAfterSeconds: 300
      input:
        parameters:
          repository: my-org/my-app          # required (repo path)
          registry: myco.jfrog.io            # optional; else server default
          tags: [dev-current, stable]        # optional exact-match allowlist (existence check)
          tagFilters:                        # all must match (AND)
            - regex: "^v\\d+\\.\\d+\\.\\d+$"
            - semver: ">= 1.2.0, < 2.0.0"
          excludeTagFilters:                 # any match => excluded
            - regex: ".*-rc.*"
          artifactType: application/vnd.cncf.helm.config.v1+json  # optional
          annotationSelectors:               # OCI manifest/config annotations (AND)
            - key: org.opencontainers.image.vendor
              operator: In                   # In|NotIn|Exists|DoesNotExist
              values: [my-org]
          sort: semver                       # semver|alpha|created
          order: desc                        # asc|desc
          limit: 20                          # keep first N after sort
```

Rules are validated strictly; unknown/invalid rules => error (rule #1, fail closed).

### 4.2 Output (one map per artifact; nested so both goTemplate & flatten work)

```yaml
oci:
  registry:   myco.jfrog.io
  repository: my-org/my-app
  tag:        v1.4.2
  digest:     sha256:...
  ref:        myco.jfrog.io/my-org/my-app:v1.4.2
  pinnedRef:  myco.jfrog.io/my-org/my-app@sha256:...   # digest-pinned, recommended for GitOps
  mediaType:  application/vnd.oci.image.manifest.v1+json
  artifactType: application/vnd.cncf.helm.config.v1+json
  createdAt:  2024-05-01T12:00:00Z                     # from org.opencontainers.image.created if present
  annotations:
    org.opencontainers.image.revision: abc123
  semver:                                              # present when tag is valid semver
    major: 1
    minor: 4
    patch: 2
    prerelease: ""
```

## 4.3 Wildcards & captures (parity with the Git directory generator, extended)

The Git generator lets you write `some/repo/*/env/*` and consume the matched
segments (`path[0]`, `path[1]`, `path.basename`, ...) in the template. We keep that
ergonomics for OCI **and add named captures**, across two axes:

- **Repository namespace** (enumerated from the registry catalog):
  `repository: "apps-oci/{team}/**/{env}"`
- **Tag**: `tagPattern: "{something}-current"`

### Glob syntax (not regex; `.` is literal)

| token      | meaning                                              | capture |
|------------|------------------------------------------------------|---------|
| `*`        | one path segment (no `/`) / any run within a tag     | yes (anonymous, positional) |
| `**`       | zero or more path segments (globstar), repo only     | yes (anonymous, positional) |
| `?`        | a single character                                   | no      |
| `{name}`   | one segment (repo) / minimal run (tag), **named**    | yes     |
| other      | literal (regexp-escaped)                              | n/a     |

Canonical example `apps-oci/.**/**/.*/{something}-current` becomes, in this
syntax, e.g. `repository: "apps-oci/**/{env}"` + `tagPattern: "{something}-current"`.

### How matching drives discovery

- If `repository` contains no wildcard -> a single repository is queried directly.
- If it contains wildcards -> the registry **catalog** (`GET /v2/_catalog`) is
  listed, filtered by the literal prefix (before the first wildcard) for perf, then
  matched against the compiled pattern. Catalog access failures -> error (rule #1).
- For each matched repository, tags are listed and matched against `tagPattern` /
  `tags` / `tagFilters`.

### Exposed capture parameters (added to the output of §4.2)

```yaml
oci:
  captures:                 # merged named captures (repo + tag); names must be unique
    team: payments
    env: dev
    something: orders-api
  wildcards: [payments/x, dev]   # anonymous * / ** captures, in pattern order
  repositorySegments: [apps-oci, payments, x, dev]
```

Usage in a template: `{{ .oci.captures.env }}`, `{{ index .oci.wildcards 0 }}`.
Duplicate capture names across repo+tag patterns are rejected at request time
(validation error -> rule #1).

## 5. Security model (design rule #2: credentials never come from the appset)

Threat: ApplicationSets are often self-service. If auth config lived in the appset spec, any
appset author could point the plugin at arbitrary registries with platform creds, or exfiltrate
tokens via crafted templates. Therefore:

- The **platform team** configures a **server-side registry credential map**. The appset only
  chooses a `registry` + `repository` (both validated against an allowlist/policy).
- Credentials are resolved from **mounted k8s Secrets / env / cloud identity**, never from the wire.
- Auth providers (pluggable `Authenticator` per registry host):
  - `basic` — username/password (Artifactory & any Docker-registry-v2 with basic auth). Values
    from env/secret file refs.
  - `ecr` — uses the AWS chain (IRSA, env, instance profile). Optional `roleArn` → STS
    AssumeRole, then ECR `GetAuthorizationToken`. Honors registry roles as required.
  - `anonymous` — public registries.
- Optional `allowedRepositories` per registry (glob) to constrain what an appset may target.
- The plugin's own `token` (bearer) is validated on every request (constant-time compare).

Server config (mounted file, hot-reload optional), example:

```yaml
listen: :8080
defaultRegistry: myco.jfrog.io
registries:
  - host: myco.jfrog.io
    auth:
      type: basic
      username: ${ARTIFACTORY_USER}      # env expansion
      password: ${ARTIFACTORY_PASS}
    allowedRepositories: ["my-org/*"]
  - host: 123456789012.dkr.ecr.us-east-1.amazonaws.com
    auth:
      type: ecr
      region: us-east-1
      roleArn: arn:aws:iam::123456789012:role/argocd-ecr-reader   # optional
tls:
  insecureSkipVerify: false
```

## 6. Component architecture

```
cmd/plugin            main: load config, wire deps, run server
internal/config       config load/validate + env expansion
internal/server       HTTP server: bearer auth mw, /api/v1/getparams.execute, /healthz, /metrics
internal/auth         Authenticator interface + basic/ecr/anonymous providers + keychain resolver
internal/registry     go-containerregistry wrapper: ListTags, Head/Get manifest, annotations
internal/generator    core: apply input rules -> registry queries -> filter/sort/limit -> params
internal/oci          value objects: Artifact, Reference, param mapping
```

Key dependencies:
- `github.com/google/go-containerregistry` — registry client + `authn` abstraction + in-mem
  registry for tests.
- `github.com/awslabs/amazon-ecr-credential-helper/ecr-login` — ECR keychain.
- `github.com/Masterminds/semver/v3` — semver parsing/constraints/sort.
- stdlib `log/slog` for logging, `net/http` for server.

## 7. Error handling (rule #1 in practice)

- Any error from config resolution, auth, registry calls, or rule evaluation → HTTP 4xx/5xx with
  `{"error": "..."}`. The controller treats it as a generator error → no deletions.
- Distinguish client errors (bad input, disallowed repo → 400/403) from upstream/registry
  failures (→ 502/504). Both still prevent deletion; the status just improves debuggability.
- Empty-but-valid result (repo genuinely has zero matching tags) returns 200 with `[]`. This is
  the one case where deletion is *intended* (no artifacts → no apps). Documented + guardable via
  a `minResults`/`failOnEmpty` option for the paranoid.

## 8. Testing strategy

- Unit: filters (regex/semver/annotation), sort/limit, param mapping, config validation, auth
  provider selection.
- Component: spin up `go-containerregistry` in-memory registry, push fake artifacts w/ annotations,
  run the generator end-to-end over HTTP. Assert output + error/fail-closed behavior.
- ECR: unit-test the AssumeRole/token wiring with mocked STS+ECR clients.
- Golden tests for the HTTP contract (request/response shapes).

## 9. Release & ops

- Multi-arch container image (distroless, non-root), SBOM, cosign signing.
- `goreleaser` for binaries + image; GitHub Actions CI (lint, test, race, build) + release.
- Helm chart / raw manifests (Deployment, Service, ConfigMap, Secret example, NetworkPolicy).
- Versioned OCI plugin image published to GHCR.

## 10. Try-it-out

- `make dev` runs the server locally against a local registry (or your Artifactory/ECR).
- `hack/` scripts + example manifests to install into an existing ArgoCD and a sample
  ApplicationSet that renders one App per chart version.
```
