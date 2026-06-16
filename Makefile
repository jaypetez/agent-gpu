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

GO  ?= go
BUF ?= buf

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
