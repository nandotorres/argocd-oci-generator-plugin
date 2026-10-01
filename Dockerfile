# syntax=docker/dockerfile:1

# --- build stage ---
FROM golang:1.25 AS build
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
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/plugin /usr/local/bin/plugin
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/plugin"]
