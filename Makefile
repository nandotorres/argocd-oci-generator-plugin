SHELL := /bin/bash
MODULE := github.com/nandotorres/argocd-oci-generator-plugin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE ?= ghcr.io/nandotorres/argocd-oci-generator-plugin
LDFLAGS := -s -w -X main.version=$(VERSION)

GOLANGCI_LINT_VERSION ?= v2.14.0

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
		go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	golangci-lint run

.PHONY: tidy
tidy: ## Tidy go modules
	go mod tidy

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: verify
verify: ## Verify module checksums against go.sum
	go mod verify

.PHONY: vuln
vuln: ## Run govulncheck (Go vulnerability database)
	@command -v govulncheck >/dev/null 2>&1 || \
		go install golang.org/x/vuln/cmd/govulncheck@latest
	govulncheck ./...

.PHONY: scan
scan: ## Run Trivy filesystem scan (vuln + secret + misconfig), same gate as CI
	@command -v trivy >/dev/null 2>&1 || { echo "install trivy: https://trivy.dev"; exit 1; }
	trivy fs --scanners vuln,secret,misconfig --ignore-unfixed \
		--severity CRITICAL,HIGH --exit-code 1 .

.PHONY: audit
audit: verify vuln scan ## Run the full local supply-chain gate (mirrors release 'guard' job)

.PHONY: go-upgrade
go-upgrade: ## Bump the Go toolchain + stdlib to the latest patch, then re-run the gate
	@latest=$$(curl -fsSL 'https://go.dev/dl/?mode=json' | \
		grep -m1 -oE '"version": "go[0-9.]+"' | grep -oE 'go[0-9.]+'); \
	[ -n "$$latest" ] || { echo "could not resolve latest Go version"; exit 1; }; \
	echo "Pinning toolchain to $$latest"; \
	go mod edit -toolchain=$$latest; \
	go get go@$${latest#go} 2>/dev/null || true; \
	go mod tidy
	$(MAKE) audit

.PHONY: deps-upgrade
deps-upgrade: ## Update all direct+indirect module deps to latest, tidy, then gate
	go get -u ./...
	go mod tidy
	$(MAKE) audit

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

.PHONY: ruleset
ruleset: ## Apply branch-protection-as-code to the default branch (needs gh admin)
	./scripts/apply-ruleset.sh

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin coverage.out
