#!/usr/bin/env bash
# Self-contained end-to-end demo of the OCI ApplicationSet generator plugin.
#
# One command spins up everything on a throwaway kind cluster and proves the
# whole chain works:
#   1. a TLS (self-signed) in-cluster OCI registry
#   2. a minimal Helm chart pushed to it as an OCI artifact (apps-oci/demo:1.0.0)
#   3. Argo CD (incl. the ApplicationSet controller)
#   4. this plugin, built from the repo Dockerfile and loaded into kind
#   5. three ApplicationSets that exercise different plugin modes:
#        - tag match (apps-oci/demo:1.0.0)  -> one Application, SYNCED
#        - repository match (match: repository) -> one Application, SYNCED,
#          created because the repository exists, with no specific tag
#        - a missing tag (apps-oci/demo:9.9.9) -> NO Application (empty result)
#   6. a "break": the registry is taken down so the generator errors, and we
#      show the ApplicationSets do NOT delete the already-created Applications.
#   7. a "recover": the registry is brought back and the chart re-pushed, so the
#      generator succeeds again and the Applications return to Synced/Healthy.
#      The demo ends in a working state.
#
# Requirements: docker, kind, kubectl, helm, openssl. Nothing else.
#
# Usage:
#   hack/e2e.sh            # run the full demo (creates cluster, leaves it up)
#   hack/e2e.sh --clean    # delete the kind cluster and exit
set -euo pipefail

CLUSTER="${E2E_CLUSTER:-oci-gen-e2e}"
IMAGE="argocd-oci-generator-plugin:e2e"
TOKEN="dev-token"
# Local demo only. bcrypt("e2e"), generated with htpasswd -nbBC 10.
ARGOCD_ADMIN_PASSWORD="e2e"
ARGOCD_ADMIN_BCRYPT='$2a$10$vEgjBY7OsKnFwsRY8DS96uweFJs.0r7XqsqHHvTKsFUq5xvBCQd6a'
HOST_REG_PORT="${E2E_REG_PORT:-5001}"   # host port -> registry NodePort
NODE_REG_PORT=30001                      # NodePort inside the cluster
REG_IN_CLUSTER="registry.registry.svc.cluster.local:5000"
ARGOCD_MANIFEST="https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

c_blue=$'\033[36m'; c_green=$'\033[32m'; c_red=$'\033[31m'; c_dim=$'\033[2m'; c_off=$'\033[0m'
step() { echo; echo "${c_blue}==> $*${c_off}"; }
ok()   { echo "${c_green}  ✓ $*${c_off}"; }
warn() { echo "${c_red}  ! $*${c_off}"; }

# Block until the registry answers on the host port, or fail after ~60s.
wait_registry_reachable() {
  local i
  for i in $(seq 1 30); do
    curl -ks "https://localhost:${HOST_REG_PORT}/v2/" >/dev/null 2>&1 && return 0
    sleep 2
  done
  warn "registry not reachable on localhost:${HOST_REG_PORT}"; return 1
}

# Package the minimal demo chart and push it as apps-oci/demo:1.0.0. Called once
# for the initial deploy and again after the registry is brought back (its
# storage is ephemeral, so a restart starts it empty).
push_demo_chart() {
  local dir chart
  dir="$(mktemp -d)"; chart="${dir}/demo"
  mkdir -p "${chart}/templates"
  cat > "${chart}/Chart.yaml" <<'YAML'
apiVersion: v2
name: demo
version: 1.0.0
type: application
YAML
  cat > "${chart}/templates/configmap.yaml" <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: demo-from-oci
data:
  message: "deployed by Argo CD from an OCI Helm chart discovered by the plugin"
YAML
  helm package "${chart}" -d "${dir}" >/dev/null
  helm push "${dir}/demo-1.0.0.tgz" \
    "oci://localhost:${HOST_REG_PORT}/apps-oci" --insecure-skip-tls-verify >/dev/null 2>&1
  rm -rf "${dir}"
}

# Use a kubeconfig of our own, so kind never touches ~/.kube/config and the
# demo cannot repoint (or unset) the context you use for real clusters.
export KUBECONFIG="${ROOT}/.e2e/kubeconfig"
mkdir -p "$(dirname "${KUBECONFIG}")"
touch "${KUBECONFIG}"
chmod 600 "${KUBECONFIG}"

if [[ "${1:-}" == "--clean" ]]; then
  step "Deleting kind cluster ${CLUSTER}"
  kind delete cluster --name "${CLUSTER}" || true
  ok "done"
  exit 0
fi

step "Preflight"
for bin in docker kind kubectl helm openssl; do
  command -v "$bin" >/dev/null 2>&1 || { warn "missing required tool: $bin"; exit 1; }
done
docker info >/dev/null 2>&1 || { warn "docker daemon is not running"; exit 1; }
ok "docker, kind, kubectl, helm, openssl present"

KUBECTL=(kubectl --context "kind-${CLUSTER}")

# ---------------------------------------------------------------------------
step "Creating kind cluster ${CLUSTER} (host :${HOST_REG_PORT} -> registry)"
if kind get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
  ok "cluster already exists, reusing"
else
  cat <<YAML | kind create cluster --name "${CLUSTER}" --config=-
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: ${NODE_REG_PORT}
        hostPort: ${HOST_REG_PORT}
        protocol: TCP
YAML
  ok "cluster created"
fi

# ---------------------------------------------------------------------------
step "Deploying a TLS OCI registry (self-signed)"
CERTDIR="$(mktemp -d)"
openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
  -keyout "${CERTDIR}/tls.key" -out "${CERTDIR}/tls.crt" \
  -subj "/CN=registry" \
  -addext "subjectAltName=DNS:registry.registry.svc.cluster.local,DNS:registry,DNS:localhost,IP:127.0.0.1" \
  >/dev/null 2>&1
"${KUBECTL[@]}" create namespace registry --dry-run=client -o yaml | "${KUBECTL[@]}" apply -f - >/dev/null
"${KUBECTL[@]}" -n registry create secret tls registry-tls \
  --cert="${CERTDIR}/tls.crt" --key="${CERTDIR}/tls.key" \
  --dry-run=client -o yaml | "${KUBECTL[@]}" apply -f - >/dev/null
rm -rf "${CERTDIR}"
cat <<YAML | "${KUBECTL[@]}" apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata: { name: registry, namespace: registry }
spec:
  replicas: 1
  selector: { matchLabels: { app: registry } }
  template:
    metadata: { labels: { app: registry } }
    spec:
      containers:
        - name: registry
          image: registry:2
          ports: [ { containerPort: 5000 } ]
          env:
            - { name: REGISTRY_HTTP_TLS_CERTIFICATE, value: /certs/tls.crt }
            - { name: REGISTRY_HTTP_TLS_KEY, value: /certs/tls.key }
          volumeMounts: [ { name: certs, mountPath: /certs, readOnly: true } ]
      volumes:
        - name: certs
          secret: { secretName: registry-tls }
---
apiVersion: v1
kind: Service
metadata: { name: registry, namespace: registry }
spec:
  type: NodePort
  selector: { app: registry }
  ports:
    - { port: 5000, targetPort: 5000, nodePort: ${NODE_REG_PORT} }
YAML
"${KUBECTL[@]}" -n registry rollout status deploy/registry --timeout=120s
ok "registry up over TLS (in-cluster: ${REG_IN_CLUSTER}, host: localhost:${HOST_REG_PORT})"

# ---------------------------------------------------------------------------
step "Packaging and pushing a minimal Helm chart as an OCI artifact"
wait_registry_reachable || exit 1
push_demo_chart
ok "pushed apps-oci/demo:1.0.0"

# ---------------------------------------------------------------------------
step "Building plugin image and loading it into kind"
docker build -t "${IMAGE}" "${ROOT}" >/dev/null
kind load docker-image "${IMAGE}" --name "${CLUSTER}" >/dev/null
ok "loaded ${IMAGE}"

# ---------------------------------------------------------------------------
step "Installing Argo CD (this can take a minute)"
"${KUBECTL[@]}" create namespace argocd --dry-run=client -o yaml | "${KUBECTL[@]}" apply -f - >/dev/null
# Server-side apply: Argo CD's CRDs exceed the 256KB client-side annotation limit.
"${KUBECTL[@]}" -n argocd apply --server-side --force-conflicts -f "${ARGOCD_MANIFEST}" >/dev/null
"${KUBECTL[@]}" wait --for=condition=established --timeout=120s \
  crd/applicationsets.argoproj.io crd/applications.argoproj.io >/dev/null
# Wait for ALL core components: the repo-server must be reachable before an
# Application can be compared, or the first sync races into a connection error.
for d in argocd-repo-server argocd-server argocd-applicationset-controller; do
  "${KUBECTL[@]}" -n argocd rollout status deploy/"$d" --timeout=300s
done
"${KUBECTL[@]}" -n argocd rollout status statefulset/argocd-application-controller --timeout=300s
# HTTP on :80 (no Chrome self-signed warning) and a known admin password.
"${KUBECTL[@]}" -n argocd patch configmap argocd-cmd-params-cm --type merge \
  -p '{"data":{"server.insecure":"true"}}' >/dev/null
"${KUBECTL[@]}" -n argocd patch secret argocd-secret --type merge \
  -p "{\"stringData\":{\"admin.password\":\"${ARGOCD_ADMIN_BCRYPT}\",\"admin.passwordMtime\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}}" >/dev/null
"${KUBECTL[@]}" -n argocd rollout restart deploy/argocd-server >/dev/null
"${KUBECTL[@]}" -n argocd rollout status deploy/argocd-server --timeout=180s
ok "Argo CD ready (UI: admin / ${ARGOCD_ADMIN_PASSWORD} over HTTP)"

# ---------------------------------------------------------------------------
step "Deploying the plugin and wiring Argo CD"
cat <<YAML | "${KUBECTL[@]}" apply -f -
apiVersion: v1
kind: Secret
metadata: { name: oci-generator-secret, namespace: argocd }
type: Opaque
stringData:
  token: "${TOKEN}"
---
apiVersion: v1
kind: ConfigMap
metadata: { name: oci-generator-config, namespace: argocd }
data:
  config.yaml: |
    listen: ":8080"
    token: "\${PLUGIN_TOKEN}"
    defaultRegistry: ${REG_IN_CLUSTER}
    requestTimeoutSeconds: 15
    registries:
      - host: ${REG_IN_CLUSTER}
        auth: { type: anonymous }
        allowedRepositories: ["apps-oci/**"]
    tls:
      insecureSkipVerify: true   # self-signed demo registry
---
# ConfigMap the ApplicationSet controller reads to discover the plugin.
apiVersion: v1
kind: ConfigMap
metadata:
  name: oci-generator
  namespace: argocd
  labels: { app.kubernetes.io/part-of: argocd }
data:
  baseUrl: "http://oci-generator.argocd.svc.cluster.local"
  token: "\$oci-generator-secret:token"
  requestTimeout: "30"
---
# Argo CD repository credentials so it can pull the OCI Helm chart over TLS.
apiVersion: v1
kind: Secret
metadata:
  name: oci-demo-repo
  namespace: argocd
  labels: { argocd.argoproj.io/secret-type: repository }
stringData:
  type: helm
  name: oci-demo
  url: ${REG_IN_CLUSTER}
  enableOCI: "true"
  insecure: "true"            # skip verify for the self-signed demo registry
---
apiVersion: v1
kind: Service
metadata: { name: oci-generator, namespace: argocd }
spec:
  selector: { app.kubernetes.io/name: oci-generator }
  ports: [ { port: 80, targetPort: 8080 } ]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: oci-generator
  namespace: argocd
  labels: { app.kubernetes.io/name: oci-generator }
spec:
  replicas: 1
  selector: { matchLabels: { app.kubernetes.io/name: oci-generator } }
  template:
    metadata: { labels: { app.kubernetes.io/name: oci-generator } }
    spec:
      securityContext: { runAsNonRoot: true, seccompProfile: { type: RuntimeDefault } }
      containers:
        - name: plugin
          image: ${IMAGE}
          imagePullPolicy: IfNotPresent
          args: ["--config=/etc/oci-generator/config.yaml"]
          ports: [ { name: http, containerPort: 8080 } ]
          env:
            - name: PLUGIN_TOKEN
              valueFrom: { secretKeyRef: { name: oci-generator-secret, key: token } }
          readinessProbe: { httpGet: { path: /readyz, port: http }, initialDelaySeconds: 2, periodSeconds: 5 }
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: { drop: ["ALL"] }
          volumeMounts:
            - { name: config, mountPath: /etc/oci-generator, readOnly: true }
            - { name: tmp, mountPath: /tmp }
      volumes:
        - name: config
          configMap: { name: oci-generator-config }
        - name: tmp
          emptyDir: {}
YAML
"${KUBECTL[@]}" -n argocd rollout status deploy/oci-generator --timeout=120s
# Wait until the Service actually has endpoints, so the controller's first call
# doesn't race into a connection-refused (which triggers a ~3min error backoff).
for i in $(seq 1 30); do
  ep="$("${KUBECTL[@]}" -n argocd get endpoints oci-generator -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null || true)"
  [[ -n "$ep" ]] && break; sleep 2
done
ok "plugin ready"

# ---------------------------------------------------------------------------
# await_generated APP APPSET: wait until the ApplicationSet has produced APP,
# nudging the controller out of its error backoff if it raced into one.
await_generated() {
  local app="$1" appset="$2" i reason
  for ((i=0; i<30; i++)); do
    "${KUBECTL[@]}" -n argocd get application "$app" >/dev/null 2>&1 && return 0
    reason="$("${KUBECTL[@]}" -n argocd get applicationset "$appset" \
      -o jsonpath='{.status.conditions[?(@.type=="ErrorOccurred")].reason}' 2>/dev/null || true)"
    if [[ "$reason" == "ApplicationGenerationFromParamsError" ]]; then
      "${KUBECTL[@]}" -n argocd rollout restart deploy/argocd-applicationset-controller >/dev/null 2>&1 || true
      "${KUBECTL[@]}" -n argocd rollout status deploy/argocd-applicationset-controller --timeout=90s >/dev/null 2>&1 || true
    fi
    sleep 5
  done
  return 1
}

# wait_synced APP [timeout]: wait until APP is Synced/Healthy, hard-refreshing
# between polls to clear any cached early repo-server comparison error.
wait_synced() {
  local app="$1" timeout="${2:-180}" i got
  for ((i=0; i<timeout; i+=5)); do
    got="$("${KUBECTL[@]}" -n argocd get application "$app" \
      -o jsonpath='{.status.sync.status}/{.status.health.status}' 2>/dev/null || true)"
    [[ "$got" == "Synced/Healthy" ]] && return 0
    "${KUBECTL[@]}" -n argocd annotate application "$app" argocd.argoproj.io/refresh=hard --overwrite >/dev/null 2>&1 || true
    sleep 5
  done
  return 1
}

step "Applying ApplicationSets (tag match, repository match, and a missing tag)"
cat <<YAML | "${KUBECTL[@]}" apply -f -
# 1) Tag match (the default): create an Application only if apps-oci/demo:1.0.0
#    resolves. The resolved tag and digest are available to the template.
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata: { name: demo, namespace: argocd }
spec:
  goTemplate: true
  goTemplateOptions: ["missingkey=error"]
  generators:
    - plugin:
        configMapRef: { name: oci-generator }
        requeueAfterSeconds: 20
        input:
          parameters:
            registry: ${REG_IN_CLUSTER}
            repository: apps-oci/demo
            tag: "1.0.0"
  template:
    metadata: { name: demo-dev }
    spec:
      project: default
      source:
        repoURL: "{{ .oci.registry }}"
        chart: "{{ .oci.repository }}"
        targetRevision: "{{ .oci.tag }}"
      destination: { server: https://kubernetes.default.svc, namespace: demo }
      syncPolicy:
        automated: { prune: true, selfHeal: true }
        syncOptions: [CreateNamespace=true]
---
# 2) Repository match: create an Application because the repository EXISTS,
#    regardless of any specific tag. match: repository returns no tag or digest,
#    so the chart version is pinned in the template instead of templated.
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata: { name: demo-repo, namespace: argocd }
spec:
  goTemplate: true
  goTemplateOptions: ["missingkey=error"]
  generators:
    - plugin:
        configMapRef: { name: oci-generator }
        requeueAfterSeconds: 20
        input:
          parameters:
            registry: ${REG_IN_CLUSTER}
            repository: apps-oci/demo
            match: repository
  template:
    metadata: { name: demo-repo-dev }
    spec:
      project: default
      source:
        repoURL: "{{ .oci.registry }}"
        chart: "{{ .oci.repository }}"
        targetRevision: "1.0.0"
      destination: { server: https://kubernetes.default.svc, namespace: demo-repo }
      syncPolicy:
        automated: { prune: true, selfHeal: true }
        syncOptions: [CreateNamespace=true]
---
# 3) A tag that was never published: the generator returns an empty result (a
#    SUCCESSFUL empty list, not an error), so NO Application is created.
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata: { name: demo-missing, namespace: argocd }
spec:
  goTemplate: true
  goTemplateOptions: ["missingkey=error"]
  generators:
    - plugin:
        configMapRef: { name: oci-generator }
        requeueAfterSeconds: 20
        input:
          parameters:
            registry: ${REG_IN_CLUSTER}
            repository: apps-oci/demo
            tag: "9.9.9"
  template:
    metadata: { name: demo-missing-dev }
    spec:
      project: default
      source:
        repoURL: "{{ .oci.registry }}"
        chart: "{{ .oci.repository }}"
        targetRevision: "{{ .oci.tag }}"
      destination: { server: https://kubernetes.default.svc, namespace: demo-missing }
      syncPolicy:
        automated: { prune: true, selfHeal: true }
        syncOptions: [CreateNamespace=true]
YAML

step "Verifying the deployments (tag match and repository match)"
fail=0
for app in demo-dev demo-repo-dev; do
  await_generated "$app" "${app%-dev}" || { warn "$app was never generated"; fail=1; continue; }
done
if wait_synced demo-dev; then
  ok "tag match: 'demo-dev' is Synced + Healthy"
  echo -n "${c_dim}    deployed: ${c_off}"; "${KUBECTL[@]}" -n demo get configmap demo-from-oci -o jsonpath='{.data.message}'; echo
else
  warn "demo-dev did not reach Synced/Healthy"; fail=1
fi
if wait_synced demo-repo-dev; then
  ok "repository match: 'demo-repo-dev' is Synced + Healthy (created because the repo exists)"
else
  warn "demo-repo-dev did not reach Synced/Healthy"; fail=1
fi
if [[ $fail -ne 0 ]]; then
  "${KUBECTL[@]}" -n argocd get applications || true
  "${KUBECTL[@]}" -n argocd logs deploy/oci-generator --tail=5 || true
  exit 1
fi

step "Verifying the missing tag yields NO Application (successful empty result)"
sleep 20   # let the controller run a couple of generation cycles
if "${KUBECTL[@]}" -n argocd get application demo-missing-dev >/dev/null 2>&1; then
  warn "demo-missing-dev exists, but tag 9.9.9 was never published!"
  exit 1
fi
err="$("${KUBECTL[@]}" -n argocd get applicationset demo-missing \
  -o jsonpath='{.status.conditions[?(@.type=="ErrorOccurred")].status}' 2>/dev/null || true)"
ok "tag 9.9.9 absent -> no Application (empty result, ErrorOccurred=${err:-False})"

# ---------------------------------------------------------------------------
step "BREAK: taking the registry down (generator will now error)"
"${KUBECTL[@]}" -n registry scale deploy/registry --replicas=0
ok "registry scaled to 0; the plugin can no longer resolve tags"
echo "${c_dim}  Waiting ~45s for generator requeues to fail...${c_off}"
sleep 45

step "Verifying the ApplicationSets did NOT delete the existing Applications"
missing=0
for app in demo-dev demo-repo-dev; do
  if "${KUBECTL[@]}" -n argocd get application "$app" >/dev/null 2>&1; then
    ok "'$app' is STILL PRESENT despite the generator erroring"
  else
    warn "'$app' was deleted; this is NOT the expected safe behaviour!"; missing=1
  fi
done
[[ $missing -eq 0 ]] || exit 1
echo
echo "${c_dim}  ApplicationSet error condition (fail-closed behaviour):${c_off}"
"${KUBECTL[@]}" -n argocd get applicationset demo \
  -o jsonpath='{range .status.conditions[*]}{"    "}{.type}{": "}{.reason}{"\n"}{end}' 2>/dev/null || true
echo "${c_dim}  Plugin logs (recent generation failures):${c_off}"
"${KUBECTL[@]}" -n argocd logs deploy/oci-generator --tail=3 2>/dev/null | sed 's/^/    /' || true

# ---------------------------------------------------------------------------
step "RECOVER: bringing the registry back and re-pushing the chart"
"${KUBECTL[@]}" -n registry scale deploy/registry --replicas=1
"${KUBECTL[@]}" -n registry rollout status deploy/registry --timeout=120s
wait_registry_reachable || exit 1
push_demo_chart
ok "registry back up; apps-oci/demo:1.0.0 re-pushed"

step "Verifying the Applications recover (generator succeeds again)"
# The controller may still be in the error backoff from the outage; nudge it so
# it re-generates promptly instead of waiting out the backoff.
"${KUBECTL[@]}" -n argocd rollout restart deploy/argocd-applicationset-controller >/dev/null 2>&1 || true
"${KUBECTL[@]}" -n argocd rollout status deploy/argocd-applicationset-controller --timeout=90s >/dev/null 2>&1 || true
rec=0
for app in demo-dev demo-repo-dev; do
  if wait_synced "$app"; then
    ok "'$app' is Synced + Healthy again"
  else
    warn "'$app' did not return to Synced/Healthy after recovery"; rec=1
  fi
done
if [[ $rec -ne 0 ]]; then
  "${KUBECTL[@]}" -n argocd get applications -o wide || true
  "${KUBECTL[@]}" -n argocd logs deploy/oci-generator --tail=5 || true
  exit 1
fi
"${KUBECTL[@]}" -n argocd get applications

# ---------------------------------------------------------------------------
cat <<EOF

${c_green}==> Demo complete.${c_off}

What you just saw (success -> failure -> success):
  • Tag match: the generator resolved apps-oci/demo:1.0.0 and Argo CD DEPLOYED
    Application 'demo-dev' (a real ConfigMap in namespace 'demo').
  • Repository match: 'demo-repo-dev' was created because the repository EXISTS,
    with no specific tag named (match: repository), and deployed to 'demo-repo'.
  • Missing tag: apps-oci/demo:9.9.9 was never published, so the generator
    returned an empty result and NO Application was created (a successful empty
    list is a prune instruction, not an error).
  • Taking the registry down makes the plugin fail closed (non-2xx), so Argo CD
    PRESERVES the existing Applications instead of deleting them.
  • Bringing the registry back (and re-pushing the chart) lets the generator
    succeed again, and the Applications return to Synced/Healthy. The cluster is
    left in a WORKING state.

This demo uses its own kubeconfig, so your usual context is untouched. To talk
to the throwaway cluster:

  export KUBECONFIG=${KUBECONFIG}

Open the Argo CD UI (HTTP, no certificate warning):
  kubectl -n argocd port-forward svc/argocd-server 8080:80
  # http://localhost:8080
  # user: admin
  # password: ${ARGOCD_ADMIN_PASSWORD}

If that password is rejected, the generated one is still in the cluster:
  kubectl -n argocd get secret argocd-initial-admin-secret \\
    -o jsonpath='{.data.password}' | base64 -d; echo

Tear everything down:
  hack/e2e.sh --clean          # or: make e2e-clean
EOF
