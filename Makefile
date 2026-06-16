# agent-gpu developer Makefile.
#
# Pinned tool versions (also documented in docs/architecture.md):
#   buf                v1.50.0
#   protoc-gen-go      v1.36.6
#   protoc-gen-go-grpc v1.5.1
#   goreleaser         v2.16.0
BUF_VERSION             := v1.50.0
PROTOC_GEN_GO_VERSION   := v1.36.6
PROTOC_GEN_GRPC_VERSION := v1.5.1
GORELEASER_VERSION      := v2.16.0

GORELEASER ?= go run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)

# Redocly CLI (OpenAPI lint + Redoc docs render), pinned by version + digest to
# match the `openapi` job in .github/workflows/ci.yml.
REDOCLY_VERSION := 2.33.2
REDOCLY_IMAGE   := redocly/cli:$(REDOCLY_VERSION)@sha256:6ba52a89c87a37749cee3e31def1f10ad322a6c5418b008334c4694ae665086e

GO     ?= go
BUF    ?= buf
DOCKER ?= docker

.PHONY: all
all: build test

.PHONY: tools
tools: ## Install the pinned proto toolchain into $(go env GOPATH)/bin
	$(GO) install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
	$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	$(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GRPC_VERSION)

.PHONY: proto
proto: ## Regenerate Go stubs from proto/ (commit the result)
	$(BUF) lint
	$(BUF) generate

.PHONY: proto-lint
proto-lint: ## Lint the protobuf definitions
	$(BUF) lint

.PHONY: openapi-lint
openapi-lint: ## Validate openapi.yaml (OpenAPI 3.1 + recommended ruleset) via the pinned Redocly image
	$(DOCKER) run --rm -e REDOCLY_TELEMETRY=off -v "$(CURDIR):/spec" -w /spec $(REDOCLY_IMAGE) lint openapi.yaml

.PHONY: openapi-docs
openapi-docs: ## Render openapi.yaml to openapi.html (Redoc) via the pinned Redocly image
	$(DOCKER) run --rm -e REDOCLY_TELEMETRY=off -v "$(CURDIR):/spec" -w /spec $(REDOCLY_IMAGE) build-docs openapi.yaml -o openapi.html

.PHONY: build
build: ## Build all packages and the agentgpu binary
	$(GO) build ./...

.PHONY: test
test: ## Run the test suite
	$(GO) test ./...

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: tidy
tidy: ## Tidy go.mod/go.sum
	$(GO) mod tidy

.PHONY: release-check
release-check: ## Validate the GoReleaser config (.goreleaser.yaml)
	$(GORELEASER) check

.PHONY: snapshot
snapshot: ## Cross-compile all release artifacts locally into dist/ (no publish)
	$(GORELEASER) build --snapshot --clean

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  %-14s %s\n", $$1, $$2}'
