SHELL := /bin/bash
MODULE := github.com/nandotorres/argocd-oci-generator-plugin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE ?= ghcr.io/nandotorres/argocd-oci-generator-plugin
LDFLAGS := -s -w -X main.version=$(VERSION)

GOLANGCI_LINT_VERSION ?= v1.64.8

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the binary into ./bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/plugin ./cmd/plugin

.PHONY: test
test: ## Run unit + integration tests
	go test ./...

.PHONY: race
race: ## Run tests with the race detector
	go test -race ./...

.PHONY: cover
cover: ## Run tests and open a coverage report
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: lint
lint: ## Run golangci-lint (installs it on demand)
	@command -v golangci-lint >/dev/null 2>&1 || \
		go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	golangci-lint run

.PHONY: tidy
tidy: ## Tidy go modules
	go mod tidy

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: image
image: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

.PHONY: dev
dev: build ## Run the server locally with the example config
	CONFIG_PATH=$(CURDIR)/deploy/config.example.yaml \
	PLUGIN_TOKEN=$${PLUGIN_TOKEN:-dev-token} \
	LOG_FORMAT=text LOG_LEVEL=debug \
	./bin/plugin

.PHONY: smoke
smoke: ## Run the local end-to-end smoke test against a throwaway registry
	./hack/smoke.sh

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin coverage.out
