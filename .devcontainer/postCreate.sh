#!/usr/bin/env bash
# Installs the pinned cluster tooling the e2e demo needs (kind, kubectl) into the
# dev container. Runs once, after the container is created.
set -euo pipefail

KIND_VERSION="v0.33.0"
KUBECTL_VERSION="v1.37.1"
HELM_VERSION="v4.3.0"

arch="$(dpkg --print-architecture)" # amd64 | arm64

install_bin() { # name url
  echo "==> installing $1 ($2)"
  sudo curl -fsSL -o "/usr/local/bin/$1" "$2"
  sudo chmod +x "/usr/local/bin/$1"
}

command -v kind >/dev/null 2>&1 || \
  install_bin kind "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-${arch}"

command -v kubectl >/dev/null 2>&1 || \
  install_bin kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${arch}/kubectl"

if ! command -v helm >/dev/null 2>&1; then
  echo "==> installing helm ${HELM_VERSION}"
  curl -fsSL "https://get.helm.sh/helm-${HELM_VERSION}-linux-${arch}.tar.gz" | tar -xz -C /tmp
  sudo install -m 0755 "/tmp/linux-${arch}/helm" /usr/local/bin/helm
fi
# openssl is preinstalled in the base image.

echo
echo "Dev container ready. Tool versions:"
go version
docker --version
kind version
kubectl version --client --output=yaml 2>/dev/null | grep gitVersion | head -1
helm version --short

cat <<'MSG'

Next:
  make e2e          # spin up kind + Argo CD + registry + plugin, run the demo
  make e2e-clean    # tear the kind cluster down
MSG
