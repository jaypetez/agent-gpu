# Developer Guide

This guide is the contributor's entry point to agent-gpu: how to set up a local
development environment, build and run the project, test it, and get a change
merged. It **ties together** the deeper docs rather than repeating them — each
section links out to the authoritative reference for detail.

New to the project? Read the [README](../README.md) first for the what and why,
then this guide for the how. End-user install and quickstart live in the
[README](../README.md); this page is for people changing the code.

## Overview & architecture

agent-gpu is a single Go module that ships as **one binary** with `server` and
`worker` subcommands.

- **Server** — the single entry point. Owns the public, OpenAI-compatible HTTP
  API and authenticates API keys, enforces permissions and quotas, applies a
  server-wide rate limit, holds the job queue, and runs the capacity-aware
  scheduler that dispatches jobs onto workers.
- **Worker** — runs on any machine with [Ollama](https://ollama.com). It opens
  one long-lived gRPC stream to the server, reports its capacity (GPU type,
  total/free VRAM, load, available models) via heartbeats, and executes
  inference jobs against its local Ollama.

The two transports are deliberately split:

- **Public API: HTTP.** Clients (and the operator CLI's admin commands) talk to
  the server over HTTP. An unmodified OpenAI client library works with only a
  base-URL + key change.
- **Control plane: gRPC.** The server↔worker hop is one persistent
  **bidirectional** stream in the versioned protobuf package `agentgpu.v1`
  (`ControlPlane.Connect`): registration, heartbeats, job dispatch, and streamed
  results all flow over it, so a worker needs no inbound port and can sit behind
  NAT.

The component map (server-owned subsystems → workers → Ollama → GPU):

```text
clients ──HTTP──▶ server { auth · authz · quota · rate-limit · sessions ·
                           queue · capacity-aware scheduler · /metrics }
                    │
                    └─gRPC bidi (agentgpu.v1)─▶ worker(s) ──▶ Ollama ──▶ GPU/CPU
```

For the deep dive — the request-flow and lifecycle diagrams, the scheduler
scoring weights and session affinity, the queue, auth/authz precedence, quotas,
global rate limiting, sessions, GPU detection, and structured logging — read
[docs/architecture.md](architecture.md). This guide stays a summary; that
document is the source of truth for the implemented design, and the public HTTP
surface is specified formally in the [API reference](api-reference.md).

## Local dev setup

### Prerequisites

- **Go 1.23+** — the module targets Go 1.23 (see [`go.mod`](../go.mod)). The
  toolchain alone is enough to build, run, and test the project.
- **Docker** *(optional)* — needed only for the container images, the Compose
  dev stack, and the OpenAPI lint/docs targets (which use a pinned Redocly
  image). Pure Go work needs no Docker.
- **Ollama** *(optional for local end-to-end)* — a worker drives a local
  [Ollama](https://ollama.com); the test suite never needs a real one (it stubs
  Ollama over `httptest`).

### Clone and install the toolchain

```bash
git clone https://github.com/jaypetez/agent-gpu.git
cd agent-gpu

# Install the pinned protobuf toolchain (buf, protoc-gen-go, protoc-gen-go-grpc)
# into $(go env GOPATH)/bin. Only needed if you regenerate proto stubs.
make tools
```

`make tools` installs the version-pinned `buf` toolchain used by `make proto`;
the exact versions live in the [Makefile](../Makefile) and
[docs/architecture.md](architecture.md#proto-code-generation). The generated
`*.pb.go` stubs are **committed**, so you only need this when you change a
`.proto` file.

### Default addresses

The server binds two ports (loopback by default; resolution is
**flag > env > default**):

- **Public HTTP API** — `127.0.0.1:8080` (`--http-listen` / `AGENTGPU_HTTP_LISTEN`).
- **gRPC control plane** — `127.0.0.1:50051` (`--listen` / `AGENTGPU_SERVER_LISTEN`)
  that workers connect to.
- **Prometheus `/metrics`** — `127.0.0.1:9090` on a dedicated listener
  (`--metrics-listen` / `AGENTGPU_METRICS_LISTEN`); see
  [docs/metrics.md](metrics.md).

(The Docker images override the listen addresses to `0.0.0.0` so they are
reachable from outside the container — see [docs/docker.md](docker.md).)

## Build / run / test

All routine tasks are Makefile targets; run `make help` for the full list. The
ones you will use most:

| Task | Command | Notes |
| --- | --- | --- |
| Build everything | `make build` | `go build ./...` |
| Run the tests | `make test` | `go test ./...` |
| Tests with the race detector | `go test -race ./...` | what CI runs; needs cgo |
| Coverage + gate metric | `make cover` | prints the `total:` line; 65% floor |
| Coverage HTML report | `make cover-html` | renders `coverage.html` |
| Vet | `make vet` | `go vet ./...` |
| Tidy modules | `make tidy` | `go mod tidy` |
| Regenerate proto stubs | `make proto` | runs `buf lint` + `buf generate` |
| Lint proto only | `make proto-lint` | `buf lint` |
| Validate the OpenAPI spec | `make openapi-lint` | pinned Redocly image (Docker) |
| Render the API reference | `make openapi-docs` | writes `openapi.html` (Docker) |
| Compose dev stack up | `make compose-up` | `docker compose up -d --build` |
| Compose stack down | `make compose-down` | keeps volumes/state |
| Compose end-to-end smoke | `make compose-e2e` | full bootstrap + inference |
| Load-test baseline | `make loadtest` | in-process; no Ollama/GPU needed |
| Validate release config | `make release-check` | `goreleaser check` |
| Cross-compile artifacts | `make snapshot` | local-only build into `dist/` |

A few of these warrant detail:

- **`make cover`** runs the suite with coverage and **no** `-race`, so it works
  on a box without a C toolchain (Windows included), then prints the gate metric.
  CI runs `go test -race` separately. See [docs/testing.md](testing.md#coverage)
  for the coverage gate (a **65%** ratchet that excludes generated protobuf
  code).
- **`make openapi-lint` / `make openapi-docs`** use the pinned Redocly Docker
  image, identical to the `openapi` job in CI; the spec lives at
  [`openapi.yaml`](../openapi.yaml). See [docs/api-reference.md](api-reference.md).
- **`make compose-up` / `make compose-e2e`** bring up the full stack (server,
  worker(s), Ollama, and backing services). See
  [docs/docker.md](docker.md#docker-compose-local-dev-stack).
- **`make loadtest`** runs the reproducible in-process load-test baseline; see
  [docs/load-testing.md](load-testing.md).
- **`make snapshot` / `make release-check`** validate the release pipeline
  locally; see [docs/releasing.md](releasing.md).

### Running the stack locally (from source)

The single binary exposes everything as subcommands
(`agentgpu <server|worker|key|quota|models|loadtest|version>`). To run an
end-to-end stack by hand:

```bash
# 1. Bootstrap the first admin key into the on-disk store BEFORE the server runs.
#    --local writes the key file directly; --name is required.
#    The file store is loaded once at boot, so this must happen before step 2
#    (or restart the server afterward to pick the key up).
go run ./cmd/agentgpu key create --name bootstrap --role admin --local
#    -> prints a one-time admin token; save it.

# 2. Start the server (HTTP on :8080, gRPC control plane on :50051).
go run ./cmd/agentgpu server start

# 3. In another shell, start a worker pointed at the gRPC control plane and a
#    local Ollama. --server here is the gRPC host:port (AGENTGPU_SERVER_ADDR).
go run ./cmd/agentgpu worker start --server 127.0.0.1:50051 \
  --ollama-url http://localhost:11434

# 4. Point the operator CLI at the RUNNING server's HTTP admin API and mint a
#    user key. Here --server/--url is the HTTP base URL (AGENTGPU_HTTP_ADDR),
#    distinct from the worker's gRPC --server above.
export AGENTGPU_HTTP_ADDR=http://127.0.0.1:8080
export AGENTGPU_TOKEN=<the admin token from step 1>
go run ./cmd/agentgpu key create --name my-agent --role user --allow-model llama3
go run ./cmd/agentgpu models list      # the permitted, Online-only catalog

# 5. Make an OpenAI-compatible request with the user token.
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer <the user token from step 4>" \
  -H "Content-Type: application/json" \
  -d '{"model":"llama3","messages":[{"role":"user","content":"Hello!"}]}'
```

> **Why the `--local` bootstrap + restart.** There is no auto-seeded admin key,
> and before a server runs there is no token to authenticate with. `--local`
> mints the first admin key straight into the on-disk store; because the server
> reads that store **only at boot**, a `--local` change to an already-running
> server is picked up only after a restart. Once you have the admin token, the
> `key`/`quota` commands manage the **running** server over its HTTP admin API
> and take effect immediately (no restart). `models list` is HTTP-only. This is
> the same bootstrap flow documented for containers in
> [docs/docker.md](docker.md#bootstrapping-keys).

The easiest full stack is **Docker Compose**, which wires the server, a worker,
a local Ollama (with a tiny model pulled automatically), and the backing
services in one command — start there if you just want something running:

```bash
make compose-up        # docker compose up -d --build
make compose-e2e       # bootstrap + a real chat completion, end to end
make compose-clean     # docker compose down -v (clean teardown)
```

See [docs/docker.md](docker.md) for the complete Compose walkthrough (key
bootstrap, sample inference request, scaling workers, persistence, GPU access).

## Testing & contribution workflow

### Testing

Every change ships with tests, and CI must be green before merge.

- **Unit tests** live next to the code (`*_test.go`, same package) and exercise
  one function or type.
- **Integration tests** use the external `_test` package over an in-process
  transport to assert cross-component flows end to end (registration/heartbeat,
  scheduler routing, permission `403`, quota `429`, streaming).
- **No external services.** The worker's Ollama client is pointed at an
  `httptest` stub, so the suite runs unchanged in CI with no real Ollama or GPU.
- **Shared fixtures** live in `internal/testutil` (job/key/worker/heartbeat
  builders and a configurable fake `worker.Executor`). Use them in new black-box
  `_test` tests; production code must never import `testutil`.
- **Determinism rules** (mandatory — a flaky test fails the build): poll with
  `waitFor` instead of `time.Sleep`, inject a clock and fast-forward instead of
  sleeping through real windows, keep real timeouts short, guard cross-goroutine
  state with a mutex, and run `-race`.

Run the suite the way CI does, then check the coverage gate:

```bash
go test -race ./...    # the race detector, as CI runs it (needs cgo)
make cover             # coverage + the gate metric (no -race; works without cgo)
```

Coverage is gated at a **65%** ratchet (generated protobuf code excluded). The
full conventions, the fixture catalog, the integration-test layout, the reusable
harnesses, and the anti-flake rules are in [docs/testing.md](testing.md).
Throughput/latency are covered separately by the
[load-testing harness](load-testing.md), not the functional suite.

### Contribution workflow

1. **Find or open an issue** describing the change and comment to claim it. Work
   is tracked as GitHub Issues grouped into **milestones** (each milestone is an
   epic from the roadmap) on the agent-gpu roadmap board.
2. **Branch** off `main`: `feature/<short-desc>`, `fix/<short-desc>`, or
   `chore/<short-desc>`. Never commit to `main`.
3. **Make the change with tests** (see above). Run `make test` / `go test -race
   ./...` and `make cover` locally before pushing.
4. **Open a PR** that references the issue (e.g. `Closes #26`). The PR **title**
   follows Conventional Commits (`feat:`, `fix:`, `chore:`, `ci:`, `docs:`,
   `test:`, …) — this is enforced by the **pr-title** check.
5. **Get CI green** and address review feedback before merge.

**Labels** classify issues by **type** (`epic`, `enhancement`, `bug`, `chore`,
`documentation`), **area** (`backend`, `infra`, `api`, `security`, `testing`,
`docs`, …), and **priority** (`priority:high` / `priority:medium` /
`priority:low`). Milestones group issues into the roadmap epics.

The full policy — workflow, label/milestone meanings, commit and PR conventions,
the code of conduct, and security reporting — is in
[CONTRIBUTING.md](../CONTRIBUTING.md). Releases (tagging, GoReleaser) are
documented in [docs/releasing.md](releasing.md).

## See also

- [README](../README.md) — project overview, install, and end-user quickstart.
- [Architecture](architecture.md) — the deep dive on the implemented design.
- [API Reference](api-reference.md) — the OpenAPI 3.1 contract for the HTTP API.
- [Testing](testing.md) · [Load testing](load-testing.md) — the test suite and
  the performance harness.
- [Running with Docker](docker.md) — images and the Compose dev stack.
- [Metrics (Prometheus)](metrics.md) — the `/metrics` endpoint and reference.
- [Releasing](releasing.md) — cutting a release with GoReleaser.
- [Contributing](../CONTRIBUTING.md) — the contribution policy in full.
