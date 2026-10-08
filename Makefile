# EffectTrace developer and CI entry points. CI runs these same targets.
SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help

GO        ?= go
BIN       := $(CURDIR)/.bin
VERSION   ?= dev
COMMIT    := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS   := -s -w -X github.com/effecttrace/effecttrace/internal/version.Version=$(VERSION) -X github.com/effecttrace/effecttrace/internal/version.Commit=$(COMMIT)
GOLANGCI_LINT_VERSION := v2.14.0
GOVULNCHECK_VERSION   := v1.8.0
SITE_DIR  ?= ../effecttrace.github.io
FUZZTIME  ?= 15s

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "  \033[1m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build effecttrace and effecttrace-collector into bin/
	@mkdir -p bin
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/effecttrace ./cmd/effecttrace
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/effecttrace-collector ./cmd/effecttrace-collector

.PHONY: fmt
fmt: ## Check formatting (gofmt)
	@out="$$(gofmt -l $$(git ls-files '*.go' 2>/dev/null || find . -name '*.go' -not -path './.lab/*'))"; \
	if [[ -n "$$out" ]]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

$(BIN)/golangci-lint:
	GOBIN=$(BIN) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: lint
lint: $(BIN)/golangci-lint ## Run golangci-lint
	$(BIN)/golangci-lint run ./...

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: vuln
vuln: ## Check dependencies for known vulnerabilities (govulncheck)
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

.PHONY: test
test: test-unit test-integration ## Unit and integration tests (no cluster needed)

.PHONY: test-unit
test-unit: ## Unit tests with the race detector
	$(GO) test -race -count=1 ./pkg/... ./internal/...

.PHONY: test-integration
test-integration: ## In-process integration tests (real components, fake Kubernetes API)
	$(GO) test -race -count=1 ./test/integration/...

.PHONY: fuzz
fuzz: ## Run every fuzz target for FUZZTIME each
	@for target in \
	  "./pkg/model FuzzUnmarshalGraph" \
	  "./internal/store FuzzApplyRecordJSON" \
	  "./internal/source/otlp FuzzDecodeJSON" \
	  "./internal/source/otlp FuzzDecodeProto" \
	  "./internal/source/audit FuzzParse" \
	  "./internal/privacy FuzzText" \
	  "./pkg/k8sinstrument FuzzParseProtobufMetadata" \
	  "./pkg/k8sinstrument FuzzParseRequest"; do \
	  set -- $$target; echo "fuzz $$2 ($$1)"; \
	  $(GO) test $$1 -run '^$$' -fuzz "^$$2$$" -fuzztime $(FUZZTIME); \
	done

.PHONY: test-e2e
test-e2e: ## Run the experiment suite against the kind lab (make demo-up first)
	$(GO) run ./test/experiments/cmd/run --out test-results

.PHONY: test-results
test-results: ## Regenerate summary.json from results.json and export site data
	$(GO) run ./test/experiments/cmd/summarize --dir test-results
	$(GO) run ./test/experiments/cmd/report --dir test-results
	@if [[ -d "$(SITE_DIR)" ]]; then $(GO) run ./test/experiments/cmd/sitedata --results test-results --out $(SITE_DIR)/data; fi

.PHONY: benchmark
benchmark: ## Run synthetic in-process benchmarks into test-results/benchmarks.json
	$(GO) run ./test/benchmarks --out test-results/benchmarks.json

.PHONY: scan
scan: ## Scan tracked files for personal data, secrets and attribution strings
	./scripts/scan.sh

.PHONY: verify
verify: fmt vet lint test scan ## Everything CI checks without a cluster

.PHONY: site-check
site-check: ## Validate the project website (links, assets, no third-party requests)
	python3 scripts/site_check.py $(SITE_DIR)

.PHONY: images
images: ## Build container images and load them into the lab
	./scripts/lab.sh images

.PHONY: demo-up
demo-up: ## Create the effecttrace-lab kind cluster and deploy everything
	./scripts/lab.sh up

.PHONY: demo-run
demo-run: ## Run the scripted demo: one action, an unrelated change, two concurrent actions
	./scripts/demo.sh

.PHONY: demo-down
demo-down: ## Delete the effecttrace-lab kind cluster (only that cluster)
	./scripts/lab.sh down
