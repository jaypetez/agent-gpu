# Load testing

agent-gpu ships a built-in load-testing harness as the `agentgpu loadtest`
subcommand. It drives the OpenAI-compatible inference endpoints under a
configurable concurrency and request mix, times every request, and reports
**throughput**, **latency percentiles (p50/p95/p99/p99.9)**, an **error rate**,
and a **status-code breakdown** so throttling and queue pressure are observable
from the client side. The harness is dependency-free (raw `net/http` + the
standard library) and runs in two modes:

- **`remote`** (the default) loads a running deployment over its HTTP API. This
  is the "against a deployment" path: point it at your server and a user token.
- **`inproc`** spins up a full in-process stack (control plane + echo workers
  over an in-memory transport + the HTTP API) and loads that. It needs no Ollama
  and no GPU, so it is the **reproducible, model-free baseline** path — the
  baseline numbers recorded below come from it, and CI exercises it.

## Quick start

Run a reproducible baseline with no external setup:

```bash
agentgpu loadtest --mode inproc --workers 2 --concurrency 16 --requests 2000 --endpoint chat
```

Load a running deployment (user token must be permitted for `--model`):

```bash
export AGENTGPU_HTTP_ADDR=http://localhost:8080
export AGENTGPU_TOKEN=<a user key>
agentgpu loadtest --concurrency 32 --duration 30s --endpoint chat --model llama3
```

Emit the report as JSON for archiving / comparison (`jq`-clean on stdout):

```bash
agentgpu loadtest --mode inproc --concurrency 16 --requests 2000 --json > baseline.json
```

`make loadtest` runs the in-process baseline for you.

## Flags

| Flag | Default | Purpose |
| --- | --- | --- |
| `--mode` | `remote` | `remote` (a deployment) or `inproc` (in-process stack) |
| `--url` | `$AGENTGPU_HTTP_ADDR` or `http://127.0.0.1:8080` | deployment base URL (remote) |
| `--token` | `$AGENTGPU_TOKEN` | user Bearer token permitted for `--model` (remote) |
| `--admin-token` | _(none)_ | admin token to poll `/v1/admin/stats` during the run (remote) |
| `--concurrency` | `16` | in-flight requests (closed-loop workers, or open-loop cap) |
| `--duration` | _(none)_ | run for a wall-clock duration (e.g. `30s`) |
| `--requests` | `1000` | run a fixed number of requests (mutually exclusive with `--duration`) |
| `--rate` | `0` | open-loop arrival rate in req/s (`0` = closed-loop) |
| `--endpoint` | `chat` | `chat`, `completions`, or `models` |
| `--mix` | _(none)_ | weighted mix, e.g. `chat=80,models=20` (overrides `--endpoint`) |
| `--model` | _(inproc: `echo-model`)_ | model name for chat/completions requests |
| `--prompt` | `ping` | prompt text for inference requests |
| `--json` | `false` | emit the run report as JSON |
| `--stats-interval` | `1s` | how often to poll queue/wait-time stats |
| `--workers` | `2` | **inproc**: echo workers to spin up (≥2 exercises routing) |
| `--queue-max-depth` | `0` | **inproc**: bound the server job queue (see saturation, below) |
| `--global-rpm` | `0` | **inproc**: server-wide requests-per-minute cap (`0` = unlimited) |
| `--global-tpm` | `0` | **inproc**: server-wide tokens-per-minute cap (`0` = unlimited) |
| `--think` | `0` | **inproc**: artificial per-request backend delay (e.g. `5ms`) |

Exactly one of `--duration` or `--requests` applies; a bare invocation defaults
to `--requests 1000`.

## Reading the report

A run prints four blocks (the same data is in the `--json` output):

```text
throughput
----------
elapsed:            0.025s
requests:           2000
throughput:         79977.9 req/s (all)
success throughput: 79977.9 req/s (2xx)
tokens/sec:         79977.9
error rate:         0.00%

status breakdown
----------------
2xx (ok):          2000
429 (throttled):   0
503 (unavailable): 0
other/errors:      0

latency — all requests
----------------------
p50:      0.00 ms
p95:      0.74 ms
p99:      1.26 ms
p99.9:    1.78 ms
max:      2.74 ms
mean:     0.20 ms
```

- **throughput** is completed requests per second over the run. `success
  throughput` counts only 2xx responses — under saturation it diverges from the
  total as rejections climb, so it is the rate of _useful_ work. `tokens/sec` is
  derived from the `usage.total_tokens` the inference endpoints report.
- **error rate** is the fraction of completed requests that were not 2xx.
- **status breakdown** is how throttling and queueing are observed from outside:
  - `429 (throttled)` — the request was rate-limited (global or per-key).
  - `503 (unavailable)` — the server had no capacity (a full bounded queue, or
    shutting down).
  - `other/errors` — any other status, plus transport errors (no response).
- **latency** is reported twice: over **all** requests (including fast
  rejections) and over **successful (2xx)** requests only, so a flood of quick
  429s under saturation does not flatter the latency of real work. Percentiles
  use the nearest-rank method over the sorted samples (exact, no interpolation).

## Closed-loop vs open-loop (coordinated omission)

`--rate` selects the load model, and the difference matters under saturation:

- **Closed-loop** (`--rate 0`, the default): `--concurrency` workers each send a
  request, wait for the response, then send the next. Throughput is _emergent_ —
  a slower server simply slows the send rate. This measures the system's
  sustainable throughput well, but it **hides tail latency**: a request is never
  "late" because the next one is not sent until the previous finishes.
- **Open-loop** (`--rate R`): requests are scheduled at a fixed arrival rate
  independent of how fast the server responds, and **latency is measured from the
  intended send time**, not the actual one. If every concurrency slot is busy
  when a request is due, that request waits for a slot, and the wait is folded
  into its measured latency. This is **coordinated-omission-aware**: under
  saturation the open-loop tail reflects the latency a client actually
  experiences, which closed-loop would mask.

Use closed-loop to find max sustainable throughput; use open-loop (at a rate at
or above your expected production arrival rate) to measure latency under that
load. For example, an open-loop run whose target rate exceeds what two workers
can serve shows the backlog directly in the tail:

```bash
agentgpu loadtest --mode inproc --workers 2 --concurrency 16 --rate 2000 --duration 3s
# ... achieved ~1810 req/s, p50 ~144 ms — the wait from the intended send time.
```

## Making saturation observable

The harness exposes saturation in three complementary ways. The in-process mode
provides knobs to elicit each one deterministically without a real model.

### Throttling (HTTP 429)

A low global rate limit rejects the overflow. With `--global-rpm 500` against a
2000-request burst, exactly the per-minute allowance succeeds and the rest are
throttled:

```bash
agentgpu loadtest --mode inproc --concurrency 16 --requests 2000 --global-rpm 500
```

```text
status breakdown
----------------
2xx (ok):          500
429 (throttled):   1500
503 (unavailable): 0
other/errors:      0
error rate:         75.00%
```

The same shows up against a real deployment whenever a request trips the global
or a per-key limit (`--token` keys can carry per-key quotas).

### Queue backlog (rising latency)

When workers are busy, requests wait, and the **client-observed latency** climbs.
The `--think` knob gives each in-process echo job an artificial processing delay
so a backlog forms under enough concurrency:

```bash
agentgpu loadtest --mode inproc --workers 2 --concurrency 32 --requests 1500 --think 5ms
```

```text
throughput:         333.0 req/s (all)   # bounded by 2 workers × 5 ms
latency — all requests
p50:    152.24 ms                       # vs ~0 ms unsaturated: requests are waiting
p99:    168.99 ms
```

Throughput plateaus at the service rate (two workers × the think time) and the
latency distribution reveals the backpressure — the saturation signal even when
no request is outright rejected.

### Queue depth and wait time (admin stats)

`GET /v1/admin/stats` (admin-gated) exposes the server's live **queue depth** and
**time-in-queue** histogram. Pass `--admin-token` (remote) or run `inproc` (which
reads the server's accessors directly) and the harness polls it over the run and
reports the peak:

```bash
agentgpu loadtest --concurrency 64 --duration 30s --model llama3 \
  --token "$AGENTGPU_TOKEN" --admin-token "$ADMIN_TOKEN" --stats-interval 1s
```

```text
saturation (admin stats)
------------------------
peak queue depth:  12
queued jobs:       340
queue wait max:    1840 ms
queue wait mean:   210 ms
```

A note on `--queue-max-depth` and HTTP 503: the server only enqueues a job (and
can therefore reject with 503 when the queue is full) when **no worker is
runnable** for the model — not merely when every worker is busy. A busy but
healthy worker stays runnable, so under the in-process echo backend the server
queue stays empty and `--queue-max-depth` does not fire; the in-process
saturation signals are the 429 (throttling) path and the latency backlog above.
The 503 path engages on a real deployment when the fleet cannot serve the
requested model (all workers saturated past capacity, draining, stale, or no
worker advertises the model) — and the harness surfaces those 503s in the status
breakdown.

## Baseline numbers

These are recorded so future changes can be compared against a known starting
point. They come from the in-process mode (echo backend — no Ollama/GPU), so they
measure the **gateway** (HTTP, auth, scheduling, control-plane round-trip), not
model inference. Absolute throughput is machine-specific; the percentile shape
and the saturation behavior are the durable signal.

### Environment

- Hardware: AMD Ryzen 5 9600X (6 cores / 12 threads), 32 GB RAM
- OS: Windows 11 Pro (10.0.26200)
- Go: go1.26.2 (`GOMAXPROCS` = 12)
- agent-gpu: `feature/load-testing`

### Throughput baseline

Command: `agentgpu loadtest --mode inproc --workers 2 --concurrency 16 --requests 2000 --endpoint chat`

| Metric | Value (representative of 3 runs) |
| --- | --- |
| throughput | ~78,000–86,000 req/s |
| success / errors | 2000 / 0 (0.00% error rate) |
| latency p50 | ~0.00 ms |
| latency p95 | ~0.7 ms |
| latency p99 | ~1.1–2.1 ms |
| latency p99.9 | ~1.3–2.4 ms |
| latency max | ~1.6–2.7 ms |
| latency mean | ~0.20 ms |

At this speed each request is dominated by per-call overhead, so throughput
varies run to run; the sub-millisecond p50 and low-single-digit-ms tail are the
stable signal.

### Throttling baseline (saturation)

Command: the throughput baseline plus `--global-rpm 500`.

| Metric | Value |
| --- | --- |
| 2xx (ok) | 500 |
| 429 (throttled) | 1500 |
| error rate | 75.00% |
| success throughput | ~31,000 req/s |

The 500/1500 split is deterministic: the global limiter admits exactly its
per-minute allowance and rejects the rest with 429.

### Latency-backlog baseline (saturation)

Command: `agentgpu loadtest --mode inproc --workers 2 --concurrency 32 --requests 1500 --think 5ms`

| Metric | Value |
| --- | --- |
| throughput | ~333 req/s (= 2 workers × 5 ms) |
| success / errors | 1500 / 0 |
| latency p50 | ~152 ms (vs ~0 ms unsaturated) |
| latency p99 | ~169 ms |

To refresh these numbers, re-run the three commands above on your hardware and
record them alongside the environment block.

## In CI

CI runs the in-process harness as a functional test (in `internal/loadtest` and
`cmd/agentgpu`), not a performance gate: it asserts a run **completes** and
produces **sensible numbers** (the request count matches, throughput is
positive, percentiles are ordered, the happy path has zero errors, and the 429
throttle path is observable). It deliberately asserts **no latency thresholds** —
CI timing is noisy and latency targets are out of scope. The tests ride the
existing race-detector test job.

## See also

- [Testing](testing.md) — the unit/integration test suite and how it stays
  deterministic.
- [Architecture](architecture.md) — scheduling, the queue, and quotas that this
  harness exercises.
