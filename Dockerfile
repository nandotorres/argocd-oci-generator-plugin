# syntax=docker/dockerfile:1

# --- build stage ---
# Chainguard Go dev image: Wolfi-based, continuously patched, includes a shell
# (required for RUN) and the Go toolchain. Pinned by digest; Dependabot (docker
# ecosystem) keeps the digest fresh.
FROM cgr.dev/chainguard/go:latest-dev@sha256:1093d76b9e64919e53e1be8b5285aadf6afc51baa67e433a7e16a406a6794f6a AS build
WORKDIR /src

# Cache modules.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/plugin ./cmd/plugin

# --- runtime stage ---
# Chainguard static: minimal, nonroot (uid 65532), no shell/package manager.
FROM cgr.dev/chainguard/static:latest@sha256:fe55470f22d3259488d9d3739168d8f04da67755f0b69382bc26eda4a7d3d327
COPY --from=build /out/plugin /usr/local/bin/plugin
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/plugin"]
