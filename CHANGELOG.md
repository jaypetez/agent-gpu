# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.5] - 2026-08-13

A maintenance release: pinned-action and Docker base-image updates only, with no functional
or API changes.

### Changed

- Update pinned CI actions to their current releases: `step-security/harden-runner`
  (2.20.0 to 2.20.1), `github/codeql-action/upload-sarif` (4.37.4 to 4.37.6), and
  `DavidAnson/markdownlint-cli2-action` (24.1.0 to 24.2.0).
- Update the pinned `golang:1.26` builder image digest in the Dockerfile.

## [0.1.4] - 2026-08-07

A maintenance release: dependency and pinned-action updates only, with no functional or
API changes.

### Changed

- Bump `google.golang.org/grpc` from 1.82.1 to 1.83.0 — the transport underlying the
  server↔worker bidirectional stream. Pulls through updated
  `google.golang.org/genproto/googleapis/rpc` and OpenTelemetry `otel`, `otel/metric`,
  `otel/sdk`, `otel/sdk/metric`, and `otel/trace` (1.43.0 to 1.44.0).
- Update pinned CI actions to their current releases: `actions/stale` (10.4.0 to 11.0.0 —
  the action moved to ES modules and picked up dependency security fixes; its inputs are
  unchanged), `docker/login-action`, and `github/codeql-action/upload-sarif`.

## [0.1.3] - 2026-07-26

A maintenance release: dependency and pinned-toolchain updates only, with no functional or
API changes.

### Changed

- Bump `github.com/prometheus/client_golang` from 1.23.2 to 1.24.1 — the client library
  behind the `/metrics` endpoint. Pulls through updated `prometheus/common`,
  `prometheus/procfs`, `klauspost/compress`, and `golang.org/x/net` / `x/sys` / `x/text`.
- Update pinned CI actions to their current releases: `actions/checkout` (across all five
  workflows), `docker/login-action`, `github/codeql-action/upload-sarif`, and
  `ossf/scorecard-action`.
- Refresh the pinned Docker image digests: the `golang:1.26` builder, and the
  `distroless/static-debian12` (server) and `distroless/base-debian12` (worker) runtime
  images.

## [0.1.2] - 2026-07-19

### Changed

- Bump `google.golang.org/grpc` from 1.82.0 to 1.82.1 (patch release) — the transport
  underlying the server↔worker bidirectional stream.
- Update pinned CI actions and Docker images to their current releases: `actions/setup-go`,
  `actions/setup-node`, `docker/login-action`, `github/codeql-action/upload-sarif`, and
  `DavidAnson/markdownlint-cli2-action`, plus the `golang` builder image and the
  `distroless/base-debian12` / `distroless/static-debian12` runtime image digests.

## [0.1.1] - 2026-07-12

### Security

- Bump the Go toolchain to **1.25.12** to patch **GO-2026-5856 / CVE-2026-42505** — an
  Encrypted Client Hello (ECH) de-anonymization issue in the `crypto/tls` standard library
  where pre-shared-key identities leaked in the unencrypted ClientHello. Reached through the
  server and client HTTP/TLS paths; `govulncheck` now reports no vulnerabilities.

### Changed

- Update pinned CI actions and Docker base images to their current releases:
  `step-security/harden-runner`, `lycheeverse/lychee-action`,
  `DavidAnson/markdownlint-cli2-action`, `docker/build-push-action`, `actions/stale`, the
  `golang:1.26` builder image, and the `distroless/base-debian12` / `distroless/static-debian12`
  runtime image digests.

## [0.1.0] - 2026-07-05

First public release of agent-gpu: a distributed inference layer for Ollama. A central
server owns the public OpenAI-compatible API, auth, quotas, permissions, and scheduling;
workers run Ollama and execute dispatched jobs over a gRPC bidirectional stream.

### Added

- **OpenAI-compatible API** — `chat/completions` and `completions` with SSE streaming and
  tool calls, model discovery (`/v1/models`, `/models`), and a global rate-limit middleware
  with `Retry-After`. Fails fast on unserved models.
- **Sessions** — stateful and affinity modes with a history store, idle-expiry sweeper,
  session-affinity scheduling (rebind, hit/miss metrics), per-session quotas (concurrency,
  turns, context tokens), and Ollama `keep_alive` coordinated with session warmth.
- **Scheduling** — global job queue with priority lanes, FIFO ordering, and backpressure;
  a capacity-aware scheduler with queue placement; and queue-depth / wait-time monitoring.
- **Workers** — Ollama integration with token streaming and gated model pulls, heartbeats
  with capacity reporting, eviction and drain, and cgo-free GPU detection feeding capacity.
- **Auth, quotas & permissions** — API key system (generation, hashing, lifecycle, CLI),
  a model-access permission layer (roles, allow/deny, audit), and a per-key quota engine
  (RPM/TPM, token budgets, windows).
- **Admin API** — scoped RBAC with audit log, idempotency, and pagination; endpoints for
  keys, quotas, permissions, workers (detail, timed/forced drain, model pull/unload),
  config with live hot-reload, GPU fleet inventory, roles, per-key usage (with CSV export),
  telemetry dashboard summary, and log query + SSE live-tail.
- **Admin GUI** — embedded admin console (templ/htmx/alpine/tailwind) covering workers and
  GPUs, keys/users/permissions, and usage/telemetry/logs/settings.
- **CLI** — `agentgpu` with server-targeting key/quota and models management, admin
  subcommands, a `--local` bootstrap mode, a load-testing harness, and typed exit codes.
- **Observability** — Prometheus `/metrics` endpoint, `/v1/admin/stats`, configurable JSON
  logging with correlation IDs and secret redaction, and session-id log correlation.
- **Packaging & release** — multi-stage Dockerfiles for server and worker with CI
  build/publish, a Docker Compose dev stack (server, workers, ollama, redis, postgres), and
  a cross-platform GoReleaser pipeline producing signed-checksum binaries for
  Windows/macOS/Linux on x64 and ARM64, plus an `agentgpu version` / `--version` command.
- **Documentation** — full OpenAPI 3.1 spec with CI validation, a rendered API reference on
  GitHub Pages, a contributor developer guide, a multi-turn sessions guide, an end-to-end
  example client, and a rewritten README with a working quickstart.
- **Project foundation** — Go + gRPC server/worker scaffold, repository hardening (OpenSSF
  Scorecard, Conventional Commits PR-title check, stale bot, community-health files), and a
  deterministic end-to-end agentic test harness with a coverage gate.

[Unreleased]: https://github.com/jaypetez/agent-gpu/compare/v0.1.5...HEAD
[0.1.5]: https://github.com/jaypetez/agent-gpu/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/jaypetez/agent-gpu/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/jaypetez/agent-gpu/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/jaypetez/agent-gpu/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/jaypetez/agent-gpu/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/jaypetez/agent-gpu/releases/tag/v0.1.0
