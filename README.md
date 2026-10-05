# argocd-oci-generator-plugin

Argo CD [ApplicationSet plugin](https://argo-cd.readthedocs.io/en/latest/operator-manual/applicationset/Generators-Plugin/)
that creates an Application only if an OCI artifact exists.

Argo CD can deploy from an OCI registry, but it cannot ask whether an artifact
is there. So an ApplicationSet that fans out over clusters fails as a whole the
moment one environment has not been published to. This answers that one
question:

> does `registry/repository:tag` exist?

Artifact present, one parameter set. Absent, none, so no Application. Registry
or credentials broken, an error, so Argo CD changes nothing.

Credentials live on the plugin server. The ApplicationSet only names a
`registry`, `repository` and `tag`, all checked against the server config.

Internals: [DESIGN.md](DESIGN.md).

## When to use this (and when not to)

Argo CD has a built-in OCI generator
([argoproj/argo-cd#26121](https://github.com/argoproj/argo-cd/pull/26121)),
slated for the 3.6 release. It works like
the Git generator: it reads the directories and files inside an OCI artifact and
creates one Application per match. If you want to pull content out of an artifact
and fan out over it, use that. It is native, maintained by Argo CD, and needs no
extra service.

This plugin does a different job. It does not read what is inside an artifact. It
decides whether an Application should exist, and gives you a few things the
built-in generator does not:

- **Fail-closed existence gating.** It answers one question — does
  `registry/repository:tag` exist? — and if the registry is down or auth fails,
  that is an error, so Argo CD changes nothing. A missing artifact deletes its
  Application; a broken registry never does. This is the whole point.
- **Digest pinning.** The output includes the resolved `digest` and a
  digest-pinned `pinnedRef` (`registry/repo@sha256:…`), so your Application can
  track an immutable reference even when the tag it was found by is mutable.
- **Three existence modes.** Match a specific `tag`, or just that the
  `repository` exists, or that it exists and holds at least one tag. See
  [Match modes](#match-modes).
- **Centralized credentials and an allowlist.** Registry auth (basic, ECR,
  anonymous) lives on this plugin's own Deployment — not in the ApplicationSet,
  and not in Argo CD's repo-server — and `allowedRepositories` globs limit what
  any ApplicationSet may target. Authors only name a `registry` and
  `repository` and never handle a credential.

What it does **not** do: read or filter the contents of an artifact, and it does
not filter tags (no regex or semver). It takes a literal `tag` (or no tag, for
the repository modes).

So, picking a tool:

- **Deploying a known OCI artifact?** Use Argo CD's OCI source directly. No
  generator needed.
- **Fanning out over the files inside an artifact?** Use the built-in OCI
  generator ([argoproj/argo-cd#26121](https://github.com/argoproj/argo-cd/pull/26121), Argo CD 3.6).
- **Creating an Application only when an artifact or repository exists —
  fail-closed, digest-pinned, with credentials held by the plugin?** That is what this
  plugin is for, and the built-in generator does not do it.

## Try it locally

Needs Docker and Go.

```bash
make smoke        # throwaway registry, push a few tags, call the plugin
PLUGIN_TOKEN=dev-token make dev   # server on :8080
```

`make e2e` does the same thing on kind (Argo CD + registry + plugin + a
deliberate registry outage to show existing apps are left alone). When it
finishes it prints how to open the UI (`admin` / `e2e` over HTTP).

## How a request works

The ApplicationSet controller POSTs to `/api/v1/getparams.execute` with a
bearer token. The plugin talks to the registry and returns matches.

| plugin response | what Argo CD does |
|---|---|
| 200 with one parameter set | create / update the Application |
| 200 with `[]` | delete the Applications this set owns |
| anything else (4xx/5xx) | error, **do not touch existing apps** |

A registry outage is an error, not an empty list. That distinction is the whole
point: an empty list is an instruction to prune, so a generator that cannot
reach its registry must fail rather than report "nothing here".

## Install

Argo CD must already be running. Install the plugin in the same namespace
as Argo CD (examples use `argocd`). Config is read at process start, so
restart the Deployment after you change a ConfigMap. Pin the image tag in
`deploy/manifests/kustomization.yaml` (`newTag: latest` is only a placeholder).

### Shared token

Same value on both sides: the plugin Deployment (`PLUGIN_TOKEN`) and the
Argo CD plugin ConfigMap (`token: $oci-generator-secret:token`).

```bash
TOKEN=$(openssl rand -base64 32)
kubectl -n argocd create secret generic oci-generator-secret \
  --from-literal=token="$TOKEN"
```

### Basic auth (Harbor, GHCR, Nexus, GitLab, …)

Works with any registry-v2 username/password. For GHCR the password is a PAT
with `read:packages`.

1. Registry credentials:

```bash
kubectl -n argocd create secret generic oci-generator-registry-creds \
  --from-literal=registry-user='YOUR_USER' \
  --from-literal=registry-pass='YOUR_PASSWORD_OR_TOKEN'
```

2. Edit `deploy/manifests/configmap-server.yaml`: set `host` / `defaultRegistry`
   to your registry (e.g. `ghcr.io`, `harbor.example.com`) and tighten
   `allowedRepositories`. Empty allowlist = every repo the credential can see.

3. Apply the plugin:

```bash
kubectl apply -k deploy/manifests
kubectl -n argocd rollout status deploy/oci-generator
```

4. ApplicationSet, existence check:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: orders-api-dev
  namespace: argocd
spec:
  goTemplate: true
  goTemplateOptions: ["missingkey=error"]
  generators:
    - plugin:
        configMapRef: { name: oci-generator }
        requeueAfterSeconds: 120
        input:
          parameters:
            registry: harbor.example.com
            repository: apps-oci/orders-api/dev
            tag: "dev-current"
  template:
    metadata:
      name: orders-api-dev
    spec:
      project: default
      source:
        repoURL: "{{ .oci.registry }}/{{ .oci.repository }}"
        targetRevision: "{{ .oci.tag }}"
      destination:
        server: https://kubernetes.default.svc
        namespace: orders-api
```

Full examples: `deploy/examples/applicationset-*.yaml`.

### ECR

ECR is the only cloud-identity provider. The pod uses its ServiceAccount role
to call `GetAuthorizationToken`. No static AWS keys, no
`oci-generator-registry-creds` Secret.

Same-account, IRSA:

1. Edit the placeholders in `deploy/examples/ecr/iam-trust-irsa.json`
   (account, region, EKS OIDC id) and `iam-policy-ecr-read.json` (account,
   region, repo ARN prefix).

```bash
# OIDC issuer of the cluster, then the id after /id/
aws eks describe-cluster --name YOUR_CLUSTER --query 'cluster.identity.oidc.issuer' --output text
```

2. Create the role and attach the read policy:

```bash
aws iam create-role --role-name oci-generator-irsa \
  --assume-role-policy-document file://deploy/examples/ecr/iam-trust-irsa.json
aws iam put-role-policy --role-name oci-generator-irsa \
  --policy-name ecr-read \
  --policy-document file://deploy/examples/ecr/iam-policy-ecr-read.json
```

3. Point the plugin ServiceAccount at that role (edit the ARN in the file):

```bash
kubectl apply -f deploy/examples/ecr/serviceaccount-irsa.yaml
```

4. Edit `deploy/examples/ecr/configmap-server-ecr.yaml`: ECR host, `region`,
   `allowedRepositories`. Leave `roleArn` unset so the pod uses its own role.
   Then:

```bash
kubectl apply -k deploy/manifests
kubectl apply -f deploy/examples/ecr/configmap-server-ecr.yaml
kubectl -n argocd rollout restart deploy/oci-generator
kubectl -n argocd rollout status deploy/oci-generator
```

`apply -k` ships a basic-auth ConfigMap. The second apply replaces it; the
restart is required so the process reloads. Don't re-run `apply -k` afterwards
or you will get the basic-auth config back.

Between those two applies the pod runs with the basic-auth config and no
credentials Secret, so it will crash-loop with `unset environment variable(s):
REGISTRY_USER`. That clears once the ECR ConfigMap is applied and the
Deployment restarts.

5. ApplicationSet, same shape as basic auth; `registry` is the ECR host:

```yaml
registry: "111122223333.dkr.ecr.us-east-1.amazonaws.com"
repository: "apps-oci/my-app"
tag: "production-current"
```

See `deploy/examples/ecr/applicationset-ecr.yaml`.

**Pod Identity instead of IRSA:** skip the SA annotation; create an association
(`aws eks create-pod-identity-association`) for `argocd/oci-generator`.

**Cross-account ECR:** the pod role assumes a role in the ECR account. Set
`auth.roleArn` on that registry, trust it from the pod role
(`iam-trust-crossaccount.json`), and grant the pod role `sts:AssumeRole`.
Details in [`deploy/examples/ecr/README.md`](deploy/examples/ecr/README.md).

### Did it work?

```bash
kubectl -n argocd logs deploy/oci-generator
kubectl -n argocd get applicationset
kubectl -n argocd describe applicationset orders-api-dev
```

`generated parameters count=…` in the logs means the plugin answered. Errors
show up on the ApplicationSet (condition `ErrorOccurred`, reason
`ApplicationGenerationFromParamsError`) and existing apps stay put.

## Parameters

Input (`plugin.input.parameters`):

| key | type | |
|---|---|---|
| `repository` | string | required. Literal repository path. |
| `tag` | string | required for `match: tag` (the default); must be empty otherwise. |
| `match` | string | `tag` (default), `repository`, or `tagged`. See [Match modes](#match-modes). |
| `registry` | string | host. Defaults to the server's `defaultRegistry`. |

The ApplicationSet controller interpolates the surrounding generator's
parameters into these before the plugin sees them, so a matrix can vary the
repository or the tag per cluster:

```yaml
repository: "apps-oci/web-app"
tag: "{{ .name }}-current"
```

Output, one map when the artifact exists and none when it does not:

```yaml
oci:
  registry:   registry.example.com
  repository: apps-oci/web-app
  tag:        production-current
  digest:     sha256:...
  ref:        registry.example.com/apps-oci/web-app:production-current
  pinnedRef:  registry.example.com/apps-oci/web-app@sha256:...
  mediaType:  application/vnd.oci.image.manifest.v1+json
```

With `goTemplate: false`, Argo CD flattens these (`oci.tag`, `oci.digest`).
`digest` is worth recording on the Application: it captures what a mutable tag
actually pointed at.

### Match modes

By default the plugin asks *"does this `tag` exist?"*. `match` lets it instead
ask *"does this repository exist?"* — useful when an Application should appear as
soon as a team publishes anything, or when the tag is decided downstream (a
mutable channel, a bot-maintained pointer) and you don't want the generator to
pin it.

| `match` | question | `tag` | present → | absent → |
|---|---|---|---|---|
| `tag` (default) | does `repository:tag` resolve? | required | one param set (with `digest`/`pinnedRef`) | empty, prune |
| `repository` | does `repository` exist at all? | must be empty | one param set (no digest) | empty, prune |
| `tagged` | does `repository` exist **and** hold ≥1 tag? | must be empty | one param set (no digest) | empty, prune |

You must set `match` explicitly — it is never inferred from an empty `tag` —
because the repository modes **reverse the prune rule**: a missing repository
becomes an empty result (prune), whereas in `tag` mode it is an error (change
nothing). Opting in is the point.

When to reach for each:

- **`match: tag`** — the normal case. Per-environment deploys where the tag
  varies per cluster, and you want a digest-pinned, immutable reference.
- **`match: repository`** — onboarding / fan-out: create the Application the
  moment the repository appears, and let `targetRevision` follow a channel you
  control. Fewest round trips (status-only), so cheapest at scale.
- **`match: tagged`** — same, but don't onboard an empty placeholder repository;
  wait until something is actually published. Same cost and output shape as
  `repository`.

Both repository modes are a single tag-list probe (`?n=1`), so cost does not
grow with the number of tags, and the output carries **no** `tag`, `digest`,
`pinnedRef` or `mediaType`: no manifest is resolved, so there is nothing to pin
to. Templates needing immutability should use `match: tag`.

Example: [`deploy/examples/applicationset-repository-existence.yaml`](deploy/examples/applicationset-repository-existence.yaml).

## Repository allowlist

`allowedRepositories` in the server config is a glob (not a regex; `.` is
literal), matched against the repository an ApplicationSet asks for:

| token | matches |
|---|---|
| `*` | one path segment |
| `**` | zero or more path segments |
| `?` | one character |

`apps-oci/**` permits anything under `apps-oci/`. An empty allowlist permits
every repository the credential can see, so set one.

## Operating notes

### A missing tag is "nothing here"; a missing repository is an error

If the repository exists and the tag does not, the result is empty: no
Application is created, and an existing one is pruned. That is how an
environment is decommissioned, and it mirrors removing a file from a Git
repository.

If the *repository* does not exist, that is an anomaly rather than an answer,
so it fails closed and nothing is deleted. The practical consequence is to keep
the repository constant and put the per-environment dimension in the tag
(`web-app:production-current`), not in the repository path
(`web-app/production:current`).

The exception is `match: repository` / `match: tagged`, where a missing
repository *is* the answer (empty, prune). That reversal is why those modes are
opt-in; see [Match modes](#match-modes).

Auth failures, 5xx and network errors always fail closed.

### Sizing and performance

The plugin is a **separate Deployment** with its own requests/limits, unrelated
to the ApplicationSet controller's. The work is **I/O bound**: nearly all wall
time is spent waiting on the registry, and memory tracks artifacts in flight,
not the number of ApplicationSets.

Measured against a real Artifactory, single replica:

| Workload | Memory (RSS) | Time |
|---|---|---|
| idle | 12 MB | - |
| tag exists | 17 MB | ~250 ms |
| tag absent (repository exists) | 17 MB | ~250 ms |
| repository absent (error) | 17 MB | ~250 ms |
| 200 checks, 20 in parallel | 25 MB | ~3.8 s (~53/s) |

CPU stayed under ~5% of one core, so the shipped limits (`500m` / `256Mi`) have
roughly 10x headroom. An OOM is fail-closed but hard to diagnose, so leave the
memory limit generous.

Cost does not grow with the size of the repository: a pinned tag is resolved
with a manifest `HEAD` rather than by listing tags, so a repository with ten
thousand releases costs the same as one with ten. Both `requestTimeoutSeconds`
(server) and `requestTimeout` (Argo CD plugin ConfigMap) still need to exceed
your slowest query.

Raise `requeueAfterSeconds` rather than adding replicas: the registry is the
bottleneck, so replicas help availability, not throughput.

#### Worked example: many ApplicationSets, few clusters

At **1200 existence checks per cycle** the plugin needs roughly **25 s of work**
and stays flat around 25 MB, comfortably inside the shipped resources. What to
watch is load on the registry:

| `requeueAfterSeconds` | sustained registry load |
|---|---|
| 120 | ~20 req/s |
| 300 | ~8 req/s |
| 600 | ~4 req/s |

Start at 300-600 s for this shape: the cost is dominated by how often the checks
repeat, not by how long each one takes.

## Config

Two ConfigMaps:

- `oci-generator` (`configmap-plugin.yaml`): what Argo CD reads (`baseUrl`,
  `token`, `requestTimeout`). Must be labeled `app.kubernetes.io/part-of: argocd`.
- `oci-generator-config`: registries, auth, allowlists. Secrets are `${VAR}`
  from the pod env, never inline.

Metrics: `GET /metrics` exposes Prometheus series for request volume, latency
and upstream registry calls. See [docs/metrics.md](docs/metrics.md).

Auth types: `basic`, `ecr`, `anonymous`. `tls.insecureSkipVerify` and
`plainHTTP` apply to every registry; leave them off unless you are talking to
a local registry.

## Development

```bash
make test
make race
make lint
make image
```

## License

Apache-2.0. See [LICENSE](LICENSE).
