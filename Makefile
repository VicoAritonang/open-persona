# Development tasks. `make check` runs what CI runs on the library.

BUF_VERSION := v1.75.0
BUF         := go run github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)

.PHONY: help
help: ## List the targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-12s %s\n", $$1, $$2}'

.PHONY: generate
generate: ## Regenerate api/gen from api/proto
	$(BUF) generate

.PHONY: fmt
fmt: ## Format Go code
	gofmt -w $$(git ls-files -co --exclude-standard '*.go')

.PHONY: lint
lint: ## gofmt, vet, buf lint
	@test -z "$$(gofmt -l . )" || (gofmt -l . && echo "run: make fmt" && exit 1)
	go vet ./...
	$(BUF) lint

.PHONY: breaking
breaking: ## Fail on breaking changes to the Protobuf contract against main
	$(BUF) breaking --against '.git#branch=main'

.PHONY: generated
generated: generate ## Fail if api/gen is stale
	git diff --exit-code -- api/gen

.PHONY: tidy
tidy: ## go mod tidy, both modules
	go mod tidy
	cd platform && go mod tidy

.PHONY: test
test: ## Tests with the race detector
	go test -race ./...

.PHONY: test-platform
test-platform: ## The platform module's tests
	cd platform && go vet ./... && go test ./...

.PHONY: build
build: ## Build the CLI into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/openpersona ./cmd/openpersona

.PHONY: check
check: lint breaking generated test test-platform ## Everything CI checks
