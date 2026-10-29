# syntax=docker/dockerfile:1

# --- build stage ---
# Chainguard Go dev image: Wolfi-based, continuously patched, includes a shell
# (required for RUN) and the Go toolchain. Pinned by digest; Dependabot (docker
# ecosystem) keeps the digest fresh.
FROM cgr.dev/chainguard/go:latest-dev@sha256:84220a627ebd59f9b8b4fb9c82c03606866407fe3180cf54bdb83859e884732b AS build
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
FROM cgr.dev/chainguard/static:latest@sha256:fc42af33e643f20d08baf03cddcc1c02ba57052dfb02edb0d79f3fc47bcea8d9
COPY --from=build /out/plugin /usr/local/bin/plugin
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/plugin"]
