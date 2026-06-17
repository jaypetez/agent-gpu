# Testing

This guide covers how agent-gpu is tested and — importantly — how the integration tests stay
deterministic and free of flakes. Read this before adding tests that touch the control plane,
the worker stream, scheduling, quota, or the HTTP API.

## Test layout

- **Unit tests** live next to the code they cover (`*_test.go` in the same package). They exercise
  one function or type in isolation.
- **Integration tests** wire several components together over an in-process transport and assert
  behaviour end-to-end. They use the external `_test` package (e.g. `package server_test`,
  `package httpapi_test`) so they exercise the public surface, not internals.

The integration suite covers the cross-component flows that matter most:

| Flow | Test(s) |
| --- | --- |
| Worker registration + heartbeat / eviction / drain | `internal/server/heartbeat_integration_test.go` |
| Inference routing through the scheduler | `internal/server/scheduler_integration_test.go`, `internal/httpapi/inference_integration_test.go` |
| Permission denial → HTTP 403 | `internal/httpapi/inference_integration_test.go` (`TestChatCompletionForbiddenModel`) |
| Quota exhaustion → HTTP 429 | `internal/httpapi/inference_integration_test.go` (`TestChatCompletionQuotaExceeded429`) |
| Multi-worker model aggregation | `internal/httpapi/integration_test.go` (`TestModelAggregationMultiWorker`) |
| Streaming inference through a (stubbed) Ollama | `internal/httpapi/ollama_stream_integration_test.go`, `internal/server/ollama_integration_test.go` |

No test depends on a real Ollama or any other external service. The worker's `OllamaExecutor` is
pointed at an `httptest` stub (`stubOllama`) that speaks just enough of the Ollama HTTP API
(`/api/version`, `/api/tags`, streaming NDJSON `/api/chat`). Tests run unchanged in CI.

## Running tests

```bash
# Whole suite, with the race detector (this is what CI runs).
go test -race ./...

# A single integration test, run repeatedly to shake out flakes.
go test ./internal/httpapi/ -run TestModelAggregationMultiWorker -count=5
```

CI runs `go test -race -covermode=atomic -coverprofile=coverage.out ./...` (see
`.github/workflows/ci.yml`) — the race detector and coverage in one pass. There are no build tags on
the integration tests, so they always run. A change that makes them flaky will fail the build, so the
guidance below is mandatory, not optional.

> The race detector (`-race`) is not supported on every host. ThreadSanitizer does not run on
> 32-bit or some arm64 hosts, and it needs cgo (a C toolchain); if `-race` refuses to start locally,
> run the suite with `make cover` (no `-race`) for coverage and `-count=5` for flake-stability, and
> rely on CI's amd64 runner for the race check.

## Coverage

Coverage is measured on every CI run and gated by a threshold.

### Measuring coverage locally

```bash
# Run the suite with coverage and print the total. No -race, so it works on a
# dev box without a C toolchain (Windows included).
make cover

# Render the profile to a browsable HTML report (coverage.html).
make cover-html
```

`make cover` writes `coverage.out` and prints the `total:` line from `go tool cover -func`. Both
`coverage.out` and `coverage.html` are gitignored.

### The CI coverage gate

The `go` job runs tests with `-coverprofile=coverage.out`, then:

1. Filters the generated `proto/agentgpu/v1` package out of the profile. That code is generated and
   0%-covered by design, so counting it would unfairly drag the total down.
2. Computes the overall percentage with `go tool cover -func` and **fails the job if the total drops
   below the floor** (currently `65.0%`).
3. Writes a coverage summary to the GitHub Actions job summary — the overall percentage plus the
   per-function table — and uploads `coverage.out` + an HTML report (`coverage.html`) as the
   `coverage-report` artifact (7-day retention).

The `65.0%` floor is a **ratchet**: it sits just under the current number and is meant to be raised
over time as coverage improves. Lower it only with a deliberate decision, never to make a red build
pass.

To reproduce the gate metric locally:

```bash
make cover
grep -v 'proto/agentgpu/v1' coverage.out > coverage.gate.out
go tool cover -func=coverage.gate.out | tail -1
```

## Shared fixtures — `internal/testutil`

`internal/testutil` holds the shared test fixtures so a test states only what it cares about and
inherits sane defaults for the rest. It is imported **only** from `_test.go` files; it depends on
production packages (`types`, `store`, `auth`, `worker`) and takes `*testing.T` in its minting
helpers, so production code must never import it.

White-box internal test files (`package server`, `package httpapi`, …) that assert on **unexported**
types cannot use it and are left alone; the builders serve the black-box `_test` packages.

Each builder applies its defaults first, then the supplied functional options in order (so a later
option wins). The zero-argument form returns a valid value.

- `testutil.Job(opts...) types.Job` — a dispatchable job (`WithModel`, `WithPrompt`, `WithMessages`,
  `WithTools`, `WithSessionID`).
- `testutil.Worker(opts...) types.Worker` and `testutil.Heartbeat(opts...) types.Heartbeat` — fleet
  snapshots / liveness reports (`WithWorkerModels`, `WithFreeVRAM`, `WithLoad`, …).
- `testutil.Key(opts...) store.APIKey` (a bare record) and
  `testutil.MintKey(t, svc, opts...) (store.APIKey, token)` / `testutil.MintToken(t, svc, opts...)`
  (a real, authenticatable key via `auth.Service`): `WithRoles`, `WithAllowModels`, `WithDenyModels`,
  `WithRPM`, `WithLimits`.
- `testutil.FakeExecutor` — one configurable `worker.Executor` that folds the old
  `scriptedExecutor` / `recordingExecutor` / `blockingExecutor`: scripted deltas
  (`WithDeltas`), an echo or reply (`WithReply` / `WithReplyPerRune`), a tool call (`WithToolCall`),
  a token split (`WithTokens`), call recording (`Handled`, `LastJob`, `Pulls`), and an optional
  block/release for in-flight and mid-stream-disconnect tests (`WithBlock`, `WithEmitSignal`).

```go
exec := testutil.NewFakeExecutor(testutil.WithExecModels("llama3"), testutil.WithReply("hi"))
token := testutil.MintToken(t, authSvc,
    testutil.WithRoles(authz.RoleUser), testutil.WithAllowModels("llama3"), testutil.WithRPM(1))
job := testutil.Job(testutil.WithMessages(testutil.UserMessage("hello")), testutil.WithPrompt(""))
```

> The bufconn/gRPC harnesses (`newHarness`, `inferenceHarness`, `buildSessionHarness`) and the
> white-box internal tests are deliberately **not** migrated wholesale — cross-package harness
> refactors are risky. Use the builders in new black-box tests and when touching existing ones.

## Avoiding flaky integration tests

Flakes in this codebase come from one of two places: **waiting on time** and **racing on shared
state across goroutines**. The harnesses are built to make both avoidable.

### 1. Poll for a condition; never `time.Sleep` to "wait long enough"

A fixed sleep is either too short (flaky on a busy CI box) or too long (slow suite). Use the shared
`waitFor` helper, which polls a condition on a short interval until a deadline:

```go
// Wait until the worker's model is visible before dispatching against it.
waitFor(t, 2*time.Second, "model in catalog", func() bool {
	return len(fetchModels(t, h.url, h.token)) == 1
})
```

`waitFor` polls every 5ms and fails with a descriptive message on timeout. Reach for it whenever a
test depends on an asynchronous effect — a worker registering, a model surfacing in the fleet, a
worker being evicted — instead of sleeping a guessed duration.

### 2. Inject a clock and fast-forward instead of sleeping through real time

Anything that depends on wall-clock windows — quota RPM/TPM/daily windows, heartbeat timeouts —
takes an injectable clock so a test can advance time instantly and deterministically. Never sleep a
real minute to cross a quota window.

```go
clk := &testClock{now: time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC)}
eng := quota.NewEngine(quota.NewMemoryCounterStore(), quota.WithClock(clk.nowFn))
// ... exhaust the RPM window ...
clk.advance(time.Minute) // cross the window boundary; no real waiting
```

The clock must be guarded by a mutex: the test goroutine writes `now` while the server reads it
from request goroutines. The `testClock` helpers (`nowFn`/`advance`) do this, keeping the access
race-free under `-race`.

### 3. Keep real timeouts short in tests

Where a test genuinely needs a real timer (a heartbeat that must fire, an eviction scan that must
run), configure aggressively short intervals so the test stays fast without sleeping:

- Heartbeat interval: ~15–20ms.
- Heartbeat timeout: short enough that a silent worker is evicted quickly (tens of ms), but longer
  than the worker's own heartbeat so a live worker stays Online.
- Eviction / scan interval: ~5–10ms.

These are set via server options (`server.WithHeartbeatTimeout`, `server.WithEvictScanInterval`) and
the worker's `HeartbeatInterval`. The harnesses already do this; mirror their values rather than
inventing new ones.

### 4. Guard shared test state with a mutex; run `-race`

Integration tests routinely read state written by a goroutine they do not control — for example a
scripted executor recording the last job it ran while the test goroutine asserts on it. Guard every
such field with a mutex (see `scriptedExecutor.mu` and `recordingExecutor.mu`) so the test is clean
under `-race`. Run `-race` locally when you can, and rely on CI's race runner otherwise.

### 5. Watch for goroutine leaks

Every harness starts a worker goroutine and a gRPC server. Always cancel the worker's context and
stop the server on cleanup (the harnesses register `t.Cleanup` for this), so a test does not leak
goroutines into the next one. When adding a new harness, wire the same teardown: cancel the worker
context, `gs.Stop()` the gRPC server, and close the HTTP `httptest.Server`.

## Reusable harnesses

Prefer extending an existing harness over writing a new one from scratch:

- `internal/server/integration_test.go` — `newHarness` / `newHarnessWith(t, ...server.Option)` over
  bufconn, plus `waitFor` and worker builders.
- `internal/server/ollama_integration_test.go` — `newOllamaWorker` and the stubbed Ollama server.
- `internal/httpapi/inference_integration_test.go` — `inferenceHarness` / `newInferenceHarnessWith`
  (option-taking, e.g. `server.WithQuota`) and the `scriptedExecutor`.
- `internal/httpapi/integration_test.go` — the full HTTP-over-gRPC catalog harness and
  `newCatalogWorker`.

A small shared helper to avoid copy-paste is fine; avoid risky cross-package refactors of the
existing harness files.
