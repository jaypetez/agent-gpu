# Metrics (Prometheus)

agent-gpu exposes operational metrics in the [Prometheus text exposition
format](https://prometheus.io/docs/instrumenting/exposition_formats/) so a
Prometheus server (or any compatible scraper) can collect request throughput and
latency, token usage, queue pressure, per-worker GPU/VRAM utilization, worker
uptime, session-affinity effectiveness, and rate-limit throttling — plus the
standard Go runtime and process metrics.

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

The standard `go_*` (runtime) and `process_*` (CPU/memory/FDs) collectors are
registered too, so they appear in the exposition for free.

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

**Metrics are deliberately NOT labeled by API key id.** The number of API keys is
unbounded and operator-driven, so a `key_id` label would let the time-series
count grow without limit (a classic Prometheus cardinality blow-up). This is a
conscious deviation from the issue's "label by … key" wording: per-key usage is
already available — at full fidelity — through the **admin/quota API**
(`GET /v1/admin/keys/{id}/quota`), which is the right tool for per-key
accounting. The aggregate throttle counter (`agentgpu_throttled_total{scope}`)
still shows how much per-key vs. global limiting is happening fleet-wide.

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
