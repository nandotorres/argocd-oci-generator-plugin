#!/usr/bin/env bash
# Local end-to-end smoke test:
#   1. start a throwaway OCI registry (docker)
#   2. push a couple of artifacts
#   3. run the plugin server against it
#   4. call getparams.execute exactly like the ApplicationSet controller does
#
# Requirements: docker, go, curl, jq (optional, for pretty output).
set -euo pipefail

REG_PORT="${REG_PORT:-5001}"
REG_HOST="localhost:${REG_PORT}"
PLUGIN_PORT="${PLUGIN_PORT:-8080}"
TOKEN="${PLUGIN_TOKEN:-dev-token}"
REG_NAME="oci-gen-smoke-registry"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

cleanup() {
  [[ -n "${PLUGIN_PID:-}" ]] && kill "${PLUGIN_PID}" 2>/dev/null || true
  docker rm -f "${REG_NAME}" >/dev/null 2>&1 || true
  [[ -n "${CFG:-}" ]] && rm -f "${CFG}" || true
}
trap cleanup EXIT

echo "==> Starting throwaway registry on ${REG_HOST}"
docker rm -f "${REG_NAME}" >/dev/null 2>&1 || true
docker run -d --rm -p "${REG_PORT}:5000" --name "${REG_NAME}" registry:2 >/dev/null
sleep 2

echo "==> Publishing test artifacts"
docker pull busybox:latest >/dev/null
for ref in \
  "apps-oci/orders-api/orders-api/dev:dev-current" \
  "apps-oci/orders-api/orders-api/dev:v1.0.0" \
  "apps-oci/checkout/checkout/dev:dev-current"; do
  docker tag busybox:latest "${REG_HOST}/${ref}"
  docker push "${REG_HOST}/${ref}" >/dev/null
  echo "    pushed ${REG_HOST}/${ref}"
done

echo "==> Building plugin"
(cd "${ROOT}" && go build -o bin/plugin ./cmd/plugin)

CFG="$(mktemp)"
cat >"${CFG}" <<YAML
listen: ":${PLUGIN_PORT}"
token: "\${PLUGIN_TOKEN}"
defaultRegistry: ${REG_HOST}
registries:
  - host: ${REG_HOST}
    auth: { type: anonymous }
    allowedRepositories: ["apps-oci/**"]
tls:
  plainHTTP: true
YAML

echo "==> Starting plugin server"
PLUGIN_TOKEN="${TOKEN}" CONFIG_PATH="${CFG}" LOG_FORMAT=text "${ROOT}/bin/plugin" &
PLUGIN_PID=$!
sleep 1

call() {
  local desc="$1" body="$2"
  echo
  echo "==> ${desc}"
  echo "    request: ${body}"
  curl -sS -X POST "http://localhost:${PLUGIN_PORT}/api/v1/getparams.execute" \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    -d "${body}" | (command -v jq >/dev/null && jq . || cat)
}

call "Existence check (tag present -> 1 result)" \
  '{"applicationSetName":"smoke","input":{"parameters":{"repository":"apps-oci/orders-api/orders-api/dev","tags":["dev-current"]}}}'

call "Existence check (tag absent -> empty result)" \
  '{"applicationSetName":"smoke","input":{"parameters":{"repository":"apps-oci/orders-api/orders-api/dev","tags":["does-not-exist"]}}}'

call "Wildcard + captures (repository: apps-oci/**/{env}, tag: {app}-current)" \
  '{"applicationSetName":"smoke","input":{"parameters":{"repository":"apps-oci/**/{env}","tagPattern":"{app}-current"}}}'

echo
echo "==> Smoke test complete."
