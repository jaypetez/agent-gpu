# Metrics (Prometheus)

agent-gpu exposes operational metrics in the [Prometheus text exposition
format](https://prometheus.io/docs/instrumenting/exposition_formats/) so a
Prometheus server (or any compatible scraper) can collect request throughput and
latency, token usage, queue pressure, per-worker GPU/VRAM utilization, worker
uptime, session-affinity effectiveness, live and historical session activity, and
rate-limit throttling — plus the standard Go runtime and process metrics.

Metrics are served by the **server** process only (the public API + scheduler
own the request, queue, and fleet state). Workers report their capacity to the
server over the gRPC control plane, and the server re-exports it; workers expose
no metrics port of their own.

## The `/metrics` listener

Metrics are served on a **dedicated listener**, separate from the public
OpenAI-compatible API, on `127.0.0.1:9090` by default:

```bash
curl -s http://127.0.0.1:9090/metrics
```

It serves only `/metrics` and is **unauthenticated** — it is an operational port
meant to sit behind your network boundary, exactly like a typical Prometheus
exporter. Keeping it off the public API port means scraping needs no API key and
the API surface (and its OpenAPI spec) is unaffected.

| Setting        | Flag               | Environment variable      | Default          |
| -------------- | ------------------ | ------------------------- | ---------------- |
| Listen address | `--metrics-listen` | `AGENTGPU_METRICS_LISTEN` | `127.0.0.1:9090` |

Precedence is the project standard: **flag > environment > default**.

- To expose it off-box (e.g. inside a container so a remote Prometheus can
  scrape it), bind all interfaces: `--metrics-listen 0.0.0.0:9090` or
  `AGENTGPU_METRICS_LISTEN=0.0.0.0:9090`.
- To **disable** the listener entirely, set the flag or the environment variable
  to an explicit empty value: `--metrics-listen=""` or
  `AGENTGPU_METRICS_LISTEN=` (export it empty). An *unset* flag/env leaves the
  listener on at the default address.

## Metrics reference

All metric names are prefixed `agentgpu_`. Two kinds of metrics are exported:

- **Request-path metrics** are updated inline as HTTP requests flow through the
  server (counters and a latency histogram).
- **Live state metrics** are read from the control plane at scrape time, so the
  values always reflect the server's current queue depth and fleet — there is no
  background poller and no staleness window.

| Metric | Type | Labels | Meaning |
| ------ | ---- | ------ | ------- |
| `agentgpu_requests_total` | counter | `endpoint`, `method`, `code` | HTTP API requests handled, by route, method, and response status. |
| `agentgpu_request_duration_seconds` | histogram | `endpoint`, `method` | Request latency. Buckets span sub-ms discovery to multi-second streamed inference. |
| `agentgpu_tokens_generated_total` | counter | `model`, `kind` | Tokens accounted on completed inference. `kind` is `prompt` or `completion`. |
| `agentgpu_throttled_total` | counter | `scope` | Requests rejected by rate limiting. `scope` is `global` (server-wide limiter) or `key` (per-key quota). |
| `agentgpu_queue_depth` | gauge | `priority` | Jobs currently waiting in the scheduling queue. A series is present for every priority (`low`/`normal`/`high`), 0 when empty. |
| `agentgpu_queue_wait_seconds` | histogram | — | Time jobs spent queued before placement on a worker (the dequeue→dispatch path; the synchronous fast path is excluded). |
| `agentgpu_worker_gpu_utilization` | gauge | `worker`, `gpu_type` | A worker's reported mean GPU utilization, 0–100. |
| `agentgpu_worker_vram_bytes` | gauge | `worker` | A worker's total video memory, in bytes. |
| `agentgpu_worker_vram_free_bytes` | gauge | `worker` | A worker's currently-free video memory, in bytes. |
| `agentgpu_worker_active_jobs` | gauge | `worker` | Jobs currently in flight on a worker. |
| `agentgpu_worker_start_time_seconds` | gauge | `worker` | Unix time the worker registered. Derive uptime as `time() - agentgpu_worker_start_time_seconds`. |
| `agentgpu_fleet_workers` | gauge | `status` | Connected workers by `status` (`online`/`draining`/`stale`); a series is present for every status. |
| `agentgpu_affinity_total` | counter | `result` | Session-affinity routing outcomes: `result` is `hit` (routed back to the warm worker) or `miss` (rebound elsewhere). |
| `agentgpu_session_rebinds_total` | counter | — | Session affinity rebindings: a turn whose session moved to a different worker because the bound one was gone/draining/stale/unfit. Equals `agentgpu_affinity_total{result="miss"}` (every miss is a rebind); exposed separately as a clearly-named rebind signal. |
| `agentgpu_active_sessions` | gauge | — | Conversation sessions currently live (created and not yet ended by delete or idle expiry). Read live from the session manager at scrape time. |
| `agentgpu_session_turns` | histogram | — | Number of conversation turns a session accumulated over its lifetime, observed once when the session ends (delete or idle expiry). |
| `agentgpu_session_duration_seconds` | histogram | — | How long a session lived (end minus creation), in seconds, observed once when the session ends (delete or idle expiry). |

The standard `go_*` (runtime) and `process_*` (CPU/memory/FDs) collectors are
registered too, so they appear in the exposition for free.

> **`agentgpu_session_rebinds_total` and `agentgpu_affinity_total{result="miss"}`
> count the same events** — a session moving to a different worker is exactly an
> affinity miss. Both are exported so dashboards can chart a "sessions are moving
> workers" rate directly (`rate(agentgpu_session_rebinds_total[5m])`) without
> filtering the affinity counter by label. Watch the rebind rate climb when a
> worker drains or goes stale: live sessions on it re-pin to a healthy peer.

### Tracing a conversation in the logs

Metrics tell you the aggregate shape of session activity; the **logs** let you
follow one conversation. Every request carries a `request_id` (see
[architecture.md](architecture.md) on correlation, and the `X-Request-Id`
header), and every session-aware log line additionally carries a `session_id`:

- The stateful/affinity chat path logs a `session chat turn` line per turn,
  carrying both `request_id` (that one turn) and `session_id` (the conversation).
- The session CRUD handlers log `session created` and `session deleted` with the
  `session_id`, and the session manager logs `session expired` when the idle
  sweeper reaps one.

So you can pivot between the two granularities: filter your logs by `session_id`
to see a whole multi-turn conversation end-to-end, or by `request_id` to isolate a
single turn (which is also the worker-side `job_id`, linking the HTTP request to
the worker that executed it). `session_id` and `request_id` are **identifiers,
never secrets** — the owning API key's secret is never logged.

> **Worker uptime resets on reconnect.** `agentgpu_worker_start_time_seconds` is
> stamped when a worker registers. A worker that drops and reconnects gets a
> fresh server-side record, so its start time (and thus computed uptime) resets —
> which is exactly what you want for "how long has this worker been continuously
> attached".

## Cardinality

Every label here is **bounded** by design — none can grow without limit from
client input — so the time-series count stays predictable.

| Label | Bound |
| ----- | ----- |
| `endpoint` | A fixed allowlist of route patterns; `{id}` segments are collapsed (e.g. `/v1/sessions/{id}`) and any unrecognized path maps to `other`. |
| `method` | The HTTP methods the API serves. |
| `code` | The HTTP status codes the handlers return. |
| `model` | The set of models served across the fleet (operator-controlled). |
| `worker` | The set of connected worker IDs (operator-controlled). |
| `gpu_type` | The reported accelerator type per worker. |
| `priority` | `low`, `normal`, `high`. |
| `scope` | `global`, `key`. |
| `kind` | `prompt`, `completion`. |
| `result` | `hit`, `miss`. |

**Metrics are deliberately NOT labeled by API key id or session id.** The
number of API keys and of sessions is unbounded and driven by clients, so a
`key_id` or `session_id` label would let the time-series count grow without limit
(a classic Prometheus cardinality blow-up). The session metrics are therefore
unlabeled aggregates: `agentgpu_active_sessions` is a single gauge, and
`agentgpu_session_turns` / `agentgpu_session_duration_seconds` are single
histograms over all sessions. Per-key usage is available — at full fidelity —
through the **admin/quota API** (`GET /v1/admin/keys/{id}/quota`), and a specific
conversation is traceable through the logs by its `session_id` (see *Tracing a
conversation in the logs* above), which is the right tool for per-entity detail.
The aggregate throttle counter (`agentgpu_throttled_total{scope}`) still shows how
much per-key vs. global limiting is happening fleet-wide.

## Example exposition

A representative scrape (runtime/process metrics elided):

```text
# HELP agentgpu_requests_total Total HTTP API requests handled, labeled by endpoint, method, and response status code.
# TYPE agentgpu_requests_total counter
agentgpu_requests_total{code="200",endpoint="/v1/chat/completions",method="POST"} 2
agentgpu_requests_total{code="200",endpoint="/v1/models",method="GET"} 1
# HELP agentgpu_request_duration_seconds HTTP API request latency in seconds, labeled by endpoint and method.
# TYPE agentgpu_request_duration_seconds histogram
agentgpu_request_duration_seconds_bucket{endpoint="/v1/chat/completions",method="POST",le="0.5"} 1
agentgpu_request_duration_seconds_bucket{endpoint="/v1/chat/completions",method="POST",le="2.5"} 2
agentgpu_request_duration_seconds_bucket{endpoint="/v1/chat/completions",method="POST",le="+Inf"} 2
agentgpu_request_duration_seconds_sum{endpoint="/v1/chat/completions",method="POST"} 1.52
agentgpu_request_duration_seconds_count{endpoint="/v1/chat/completions",method="POST"} 2
# HELP agentgpu_tokens_generated_total Total tokens accounted on completed inference, labeled by model and kind (prompt|completion).
# TYPE agentgpu_tokens_generated_total counter
agentgpu_tokens_generated_total{kind="completion",model="llama3"} 64
agentgpu_tokens_generated_total{kind="prompt",model="llama3"} 128
# HELP agentgpu_throttled_total Total requests rejected by rate limiting, labeled by scope (global|key).
# TYPE agentgpu_throttled_total counter
agentgpu_throttled_total{scope="key"} 1
# HELP agentgpu_queue_depth Number of jobs currently waiting in the scheduling queue, by priority.
# TYPE agentgpu_queue_depth gauge
agentgpu_queue_depth{priority="high"} 0
agentgpu_queue_depth{priority="low"} 0
agentgpu_queue_depth{priority="normal"} 2
# HELP agentgpu_queue_wait_seconds Time jobs spent queued before being placed on a worker (placement path only), in seconds.
# TYPE agentgpu_queue_wait_seconds histogram
agentgpu_queue_wait_seconds_bucket{le="0.1"} 1
agentgpu_queue_wait_seconds_bucket{le="0.5"} 2
agentgpu_queue_wait_seconds_bucket{le="1"} 3
agentgpu_queue_wait_seconds_bucket{le="+Inf"} 3
agentgpu_queue_wait_seconds_sum 1.6
agentgpu_queue_wait_seconds_count 3
# HELP agentgpu_worker_gpu_utilization Reported mean GPU utilization of a worker, 0-100.
# TYPE agentgpu_worker_gpu_utilization gauge
agentgpu_worker_gpu_utilization{gpu_type="NVIDIA RTX 4090",worker="worker-1"} 37
# HELP agentgpu_worker_start_time_seconds Unix timestamp at which a worker registered with the server. Compute uptime as time() - this (resets on reconnect).
# TYPE agentgpu_worker_start_time_seconds gauge
agentgpu_worker_start_time_seconds{worker="worker-1"} 1.78e+09
# HELP agentgpu_fleet_workers Number of workers currently connected to the fleet, by status (online|draining|stale).
# TYPE agentgpu_fleet_workers gauge
agentgpu_fleet_workers{status="draining"} 0
agentgpu_fleet_workers{status="online"} 1
agentgpu_fleet_workers{status="stale"} 0
# HELP agentgpu_affinity_total Session-affinity routing outcomes since startup, by result (hit|miss).
# TYPE agentgpu_affinity_total counter
agentgpu_affinity_total{result="hit"} 8
agentgpu_affinity_total{result="miss"} 2
# HELP agentgpu_session_rebinds_total Total session affinity rebindings since startup: a turn whose session moved to a different worker because the bound one was gone/draining/stale/unfit. Equals affinity_total{result="miss"}.
# TYPE agentgpu_session_rebinds_total counter
agentgpu_session_rebinds_total 2
# HELP agentgpu_active_sessions Number of conversation sessions currently live (created and not yet ended by delete or idle expiry).
# TYPE agentgpu_active_sessions gauge
agentgpu_active_sessions 5
# HELP agentgpu_session_turns Number of conversation turns a session accumulated over its lifetime, observed when the session ends (delete or idle expiry).
# TYPE agentgpu_session_turns histogram
agentgpu_session_turns_bucket{le="5"} 3
agentgpu_session_turns_bucket{le="10"} 6
agentgpu_session_turns_bucket{le="+Inf"} 7
agentgpu_session_turns_sum 58
agentgpu_session_turns_count 7
# HELP agentgpu_session_duration_seconds How long a session lived (end minus creation) in seconds, observed when the session ends (delete or idle expiry).
# TYPE agentgpu_session_duration_seconds histogram
agentgpu_session_duration_seconds_bucket{le="300"} 4
agentgpu_session_duration_seconds_bucket{le="900"} 6
agentgpu_session_duration_seconds_bucket{le="+Inf"} 7
agentgpu_session_duration_seconds_sum 4123
agentgpu_session_duration_seconds_count 7
```

## Useful queries (PromQL)

Copy-paste starting points; tune the `rate()` windows to your scrape interval.

**Request rate (req/s) by endpoint:**

```promql
sum by (endpoint) (rate(agentgpu_requests_total[5m]))
```

**Error rate (5xx fraction):**

```promql
sum(rate(agentgpu_requests_total{code=~"5.."}[5m]))
  / sum(rate(agentgpu_requests_total[5m]))
```

**p95 request latency by endpoint:**

```promql
histogram_quantile(
  0.95,
  sum by (le, endpoint) (rate(agentgpu_request_duration_seconds_bucket[5m]))
)
```

**Tokens per second by model:**

```promql
sum by (model) (rate(agentgpu_tokens_generated_total[5m]))
```

**Current queue depth (total across priorities):**

```promql
sum(agentgpu_queue_depth)
```

**p95 time a job waits in the queue:**

```promql
histogram_quantile(0.95, sum by (le) (rate(agentgpu_queue_wait_seconds_bucket[5m])))
```

**GPU utilization per worker:**

```promql
agentgpu_worker_gpu_utilization
```

**Free VRAM fraction per worker:**

```promql
agentgpu_worker_vram_free_bytes / agentgpu_worker_vram_bytes
```

**Worker uptime (seconds):**

```promql
time() - agentgpu_worker_start_time_seconds
```

**Throttle rate by scope (global vs. per-key):**

```promql
sum by (scope) (rate(agentgpu_throttled_total[5m]))
```

**Session-affinity hit ratio:**

```promql
sum(rate(agentgpu_affinity_total{result="hit"}[5m]))
  / sum(rate(agentgpu_affinity_total[5m]))
```

**Live session count:**

```promql
agentgpu_active_sessions
```

**Session rebind rate (sessions moving workers per second):**

```promql
rate(agentgpu_session_rebinds_total[5m])
```

**p50 / p95 conversation length (turns per session):**

```promql
histogram_quantile(0.50, sum by (le) (rate(agentgpu_session_turns_bucket[1h])))
histogram_quantile(0.95, sum by (le) (rate(agentgpu_session_turns_bucket[1h])))
```

**p50 / p95 session lifetime (seconds):**

```promql
histogram_quantile(0.50, sum by (le) (rate(agentgpu_session_duration_seconds_bucket[1h])))
histogram_quantile(0.95, sum by (le) (rate(agentgpu_session_duration_seconds_bucket[1h])))
```

These cover the metrics the dashboards you build will most often chart; the
exposition is plain Prometheus text, so any Grafana panel, recording rule, or
alert that consumes Prometheus works against it unchanged.

## Scraping a Dockerized server

The server image (see [docs/docker.md](docker.md)) sets
`AGENTGPU_METRICS_LISTEN=0.0.0.0:9090` and `EXPOSE`s port `9090`, so a Prometheus
running alongside it can scrape the container directly. A minimal scrape config:

```yaml
scrape_configs:
  - job_name: agent-gpu
    static_configs:
      - targets: ["agentgpu-server:9090"]
```

Map or publish port `9090` as your deployment requires (and keep it on a trusted
network — the endpoint is unauthenticated by design).
