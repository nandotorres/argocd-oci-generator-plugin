# Design

Install and usage are in [README.md](README.md). This file is the rationale
and the HTTP contract.

## 1. Problem

Argo CD doesn't provide a first-class ApplicationSet generator for OCI
registries. For example, if your teams publish Helm charts or other OCI
artifacts, there's no built-in way to create an Application per artifact that
matches some rules, the way the Git generator does for files and directories.

This project adds that as an ApplicationSet plugin generator: an HTTP service the
ApplicationSet controller calls, with parity to the Git generator plus
OCI-specific narrowing (tags, semver, annotations, artifact types).

## 2. How plugin generators work

The controller calls an HTTP service:

- `POST {baseUrl}/api/v1/getparams.execute`
- Header `Authorization: Bearer <token>`
- Body: `{"applicationSetName": "<name>", "input": {"parameters": { ... }}}`
- Response: `{"output": {"parameters": [ {..}, {..} ]}}`

The plugin is referenced from the ApplicationSet via a ConfigMap (keys `baseUrl`,
`token`, optional `requestTimeout`) named in `plugin.configMapRef`. For each
returned parameter map the controller renders the template, flattened to
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

- Resolve one `registry/repository:tag` and report whether it exists, emitting
  one parameter set when it does.
- Existence-only variants (`match: repository`, `match: tagged`) that report
  whether a repository exists, or exists with at least one tag, without
  resolving a manifest. Opt-in, because they reverse the prune semantics of a
  missing repository (§4.3).
- Centralized credentials: basic auth (any registry-v2 with user/password) and
  ECR (IRSA / Pod Identity, optionally assuming a role).
- Fail closed (§2.1). Structured logging, health endpoints, Prometheus metrics.

Out of scope, deliberately:

- **Discovery.** The repository and tag are given, not searched for. Enumerating
  repositories would need `GET /v2/_catalog`, a Docker Registry v2 extension
  that is not part of the OCI distribution spec and that several major
  registries do not serve.
- Reading artifact *contents*. Argo CD's own OCI source does that; this only
  decides whether an Application should exist.
- Pushing or mutating artifacts. Read-only.
- Per-ApplicationSet credentials (see §5).

## 4. Parameter model

### 4.1 Input

```yaml
generators:
  - plugin:
      configMapRef: { name: oci-generator }
      requeueAfterSeconds: 300
      input:
        parameters:
          registry: registry.example.com   # optional; else the server default
          repository: apps-oci/my-app      # required, literal
          tag: production-current          # required for match: tag (default)
          match: tag                       # tag (default) | repository | tagged
```

`match` selects the question. `tag` (the default) resolves a single tag;
`repository` checks only that the repository exists; `tagged` checks that it
exists and holds at least one tag. For the repository modes `tag` must be empty,
and the output omits the manifest-derived fields (§4.2). `match` is required
explicitly rather than inferred from an empty `tag`, because the repository
modes reverse what a missing repository means (§4.3).

The ApplicationSet controller interpolates the surrounding generator's
parameters into these fields before the plugin is called, so a matrix varies
the repository or tag per cluster without the plugin doing any templating.

Input is validated strictly; unknown or missing fields are a 400 (§2.1).

### 4.2 Output

One parameter map when the artifact exists, nested so both `goTemplate` modes
work:

```yaml
oci:
  registry:   registry.example.com
  repository: apps-oci/my-app
  tag:        production-current
  digest:     sha256:...
  ref:        registry.example.com/apps-oci/my-app:production-current
  pinnedRef:  registry.example.com/apps-oci/my-app@sha256:...
  mediaType:  application/vnd.oci.image.manifest.v1+json
```

Everything here comes from a manifest `HEAD`, so the cost is one round trip and
does not grow with the number of tags in the repository.

For `match: repository` and `match: tagged` no manifest is resolved, so the map
is reduced to what is known — there is no tag or digest to report:

```yaml
oci:
  registry:   registry.example.com
  repository: apps-oci/my-app
  ref:        registry.example.com/apps-oci/my-app
```

Consumers that need an immutable, digest-pinned reference must therefore use
`match: tag`.

### 4.3 Existence, and what counts as an answer

For the default `match: tag`:

| Registry says | Meaning | Result |
|---|---|---|
| manifest resolves | artifact is published | one parameter set |
| tag absent, repository present | not published here | empty, no Application |
| repository absent | anomaly, not an answer | error, nothing deleted |
| 401/403/5xx/network | no answer obtained | error, nothing deleted |

A `HEAD` carries no body, and registries disagree on whether an absent tag in
an absent repository reports `MANIFEST_UNKNOWN` or `NAME_UNKNOWN`, so a miss is
confirmed with a single tag-list probe that reads only the status code.

This mirrors Git: a missing *file* in a repository that exists is an empty
result, while a missing *repository* is an error. Keeping the repository
constant and varying the tag per environment therefore gives decommissioning
the same semantics as deleting a file.

The repository modes ask a different question, so a missing repository is an
*answer*, not an anomaly:

| `match: repository` / `tagged` says | Result |
|---|---|
| repository present (and, for `tagged`, has ≥1 tag) | one parameter set |
| repository present but empty (`tagged` only) | empty, no Application |
| repository absent | empty, no Application (prune) |
| 401/403/5xx/network | error, nothing deleted |

This reversal — repository-absent moving from *error* to *empty* — is exactly why
`match` is required explicitly and never inferred. Both modes are a single
tag-list probe (`?n=1`); `repository` reads only the status code, `tagged`
also decodes the first page to see whether any tag is present. Neither grows
with the number of tags.

## 5. Security model

ApplicationSets are often self-service. If auth lived in the ApplicationSet spec,
any author could point the plugin at arbitrary registries with platform
credentials, or try to exfiltrate tokens through a crafted template. So:

- The platform team configures a server-side registry/credential map. The
  ApplicationSet only chooses a `registry` and `repository`, both validated.
- Credentials come from mounted Secrets, env, or cloud identity, never from the
  request.
- Auth providers, per registry host:
  - `basic`: username/password, from env/Secret references.
  - `ecr`: the AWS credential chain (IRSA, env, instance profile); optional
    `roleArn` to assume a role before calling `GetAuthorizationToken`.
  - `anonymous`: public registries.
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
internal/registry   go-containerregistry wrapper: tag/repository existence, manifest HEAD
internal/generator  validate input → resolve tag or probe repository → parameters
internal/oci        artifact value object + parameter mapping
internal/metrics    Prometheus collectors
internal/pattern    glob matcher for the allowedRepositories policy
```

Main dependencies:

- `github.com/google/go-containerregistry`: registry client, `authn`
  abstraction, and an in-memory registry for tests.
- `github.com/aws/aws-sdk-go-v2`: AWS config, STS AssumeRole, and ECR
  `GetAuthorizationToken`.
- `github.com/prometheus/client_golang`: metrics.
- stdlib `log/slog` and `net/http`.

## 7. Error handling

- Any error from config, auth, registry calls, or rule evaluation becomes a
  4xx/5xx response with `{"error": "..."}`, which the controller treats as a
  generator error (no deletions).
- Client errors (bad input, disallowed repository) are 400/403; upstream/registry
  failures are 502/504. Both prevent deletion; the distinction only helps
  debugging.
- An empty-but-valid result (the repository exists, the tag does not) is a 200
  with `[]`. This is the one case where deletion is intended, and it is how an
  environment is decommissioned.

## 8. Testing

- Unit: input validation, parameter mapping, allowlist policy, auth provider
  selection.
- Component: push artifacts into an in-memory registry and drive the generator
  end-to-end over HTTP; assert the present/absent/error outcomes.
- ECR: the AssumeRole/token wiring, including reuse of a still-valid token when
  a refresh fails, with mocked STS and ECR clients.
- Fuzzing on the glob matcher, which parses untrusted-ish policy input.
- `make e2e`: kind + Argo CD + registry, including a deliberate registry outage
  showing existing Applications survive.

## 9. Release

- Multi-arch container image (minimal, non-root), SBOM, and cosign signing.
- `goreleaser` for binaries and image; GitHub Actions for CI and release.
- Raw manifests (Deployment, Service, ConfigMaps, Secret example).
- Image published to GHCR.
