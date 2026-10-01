# syntax=docker/dockerfile:1

# --- build stage ---
# Chainguard Go dev image: Wolfi-based, continuously patched, includes a shell
# (required for RUN) and the Go toolchain. Pinned by digest; Dependabot (docker
# ecosystem) keeps the digest fresh.
FROM cgr.dev/chainguard/go:latest-dev@sha256:e6c2e263b59bae84e9cad12bb2571ee61626b83165be0f1a867758bf1a6b704b AS build
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
FROM cgr.dev/chainguard/static:latest@sha256:324c96273762d9500fd72d973f7d05f0dd15be0668935b3ba02221658041dc9a
COPY --from=build /out/plugin /usr/local/bin/plugin
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/plugin"]
