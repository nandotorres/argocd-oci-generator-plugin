# argocd-oci-generator-plugin

Argo CD [ApplicationSet plugin](https://argo-cd.readthedocs.io/en/latest/operator-manual/applicationset/Generators-Plugin/)
that lists an OCI registry and returns one parameter map per matching artifact.

Common cases:

- create an Application only if a tag exists (`my-app:stable`)
- create one Application per repo/tag that matches a glob (`apps/**/{env}`)

Same input also covers semver/regex filters, annotation selectors, artifact
type, sort, and limit. Full list under [Parameters](#parameters).

Credentials live on the plugin server. The ApplicationSet only names a
`registry` and `repository`. Both are checked against server config.

Internals: [DESIGN.md](DESIGN.md).

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
| 200 with matches | create / update Applications |
| 200 with `[]` | delete the Applications this set owns |
| anything else (4xx/5xx) | error, **do not touch existing apps** |

A registry outage is an error, not an empty list. Set `failOnEmpty: true` if
you also want “no matching tags” to be an error instead of a prune.

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
            tags: ["dev-current"]
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
repository: "apps-oci/{app}"
tagPattern: "v{version}"
sort: semver
order: desc
limit: 1
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
| `repository` | string | required. Path or glob. |
| `registry` | string | host. Defaults to server `defaultRegistry`. |
| `tags` | []string | exact tags (existence check). |
| `tagPattern` | string | tag glob, with captures. |
| `tagFilters` | []filter | `{regex}` or `{semver}`; all must match. |
| `excludeTagFilters` | []filter | any match drops the tag. |
| `artifactType` | string | OCI artifact type / config media type. |
| `annotationSelectors` | []selector | `In` / `NotIn` / `Exists` / `DoesNotExist`. |
| `sort` | string | `semver` \| `alpha` \| `created`. |
| `order` | string | `asc` \| `desc`. |
| `limit` | int | first N after sort. |
| `failOnEmpty` | bool | empty match is an error (no prune). |

Output, one map per artifact:

```yaml
oci:
  registry, repository, tag, digest
  ref            # registry/repository:tag
  pinnedRef      # registry/repository@sha256:...
  mediaType, artifactType, createdAt
  annotations: { ... }
  semver: { major, minor, patch, prerelease, metadata }   # if the tag is semver
  captures:  { <name>: <value> }
  wildcards: [ ... ]                 # positional * / ** , in order
  repositorySegments: [ ... ]
```

With `goTemplate: false`, Argo CD flattens these (`oci.tag`, `oci.captures.env`).
Prefer `pinnedRef` / `digest` in templates if you want immutability.

## Globs

Not regex. `.` is literal. Use `tagFilters.regex` when you need a real regexp.

| token | matches | captured |
|---|---|---|
| `*` | one path segment, or a run in a tag | yes (positional) |
| `**` | zero or more path segments (repos only) | yes (positional) |
| `?` | one character | no |
| `{name}` | one segment (repo) or a run (tag) | yes (`oci.captures.name`) |

`repository: "apps/**/{env}"` + `tagPattern: "{app}-current"` matches
`apps/team/web/prod` + `web-current`, and gives `{{ .oci.captures.env }}`,
`{{ .oci.captures.app }}`, `{{ index .oci.wildcards 0 }}`.

A literal `repository` is queried directly. A wildcard lists
`GET /v2/_catalog` first. The registry must support that (ECR does).

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
