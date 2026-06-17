# API Reference

The agent-gpu public HTTP API — the OpenAI-compatible inference surface plus the
admin and session endpoints — is described formally by an **OpenAPI 3.1**
document. That document is the single source of truth: this page is the human
entry point and a summary of the cross-cutting conventions, while the full
per-endpoint and per-schema reference is generated from the spec and never
hand-maintained.

- **Machine-readable spec:** [`openapi.yaml`](../openapi.yaml) — the canonical
  contract for every endpoint, request/response schema, status code, and example.
- **Hosted reference (Redoc):** <https://jaypetez.github.io/agent-gpu/> — a
  browsable rendering of the spec, republished automatically on every push to
  `main`.

Only the server's public HTTP surface is described. The server-to-worker control
plane is gRPC and is intentionally out of scope (see
[architecture.md](architecture.md)).

## Conventions

These apply uniformly across the API; the spec is authoritative for the details.

### Authentication

Every endpoint requires an agent-gpu API key, presented as an HTTP Bearer token:

```http
Authorization: Bearer agpu_<keyid>_<secret>
```

Admin endpoints (`/v1/admin/...`) additionally require the key to hold the
`admin` role; a valid non-admin key receives `403`. A missing, malformed,
unknown, or revoked key receives `401`. See the `bearerAuth` security scheme in
the spec.

### Rate limiting

Requests may be throttled by a per-key quota or the server-wide rate limiter.
A throttled request receives `429` with a `Retry-After` header giving the
integer number of seconds to wait before retrying:

```http
HTTP/1.1 429 Too Many Requests
Retry-After: 30
```

### Errors

Every error response uses one consistent envelope carrying a generic,
human-readable `message` and a stable, machine-readable `code`. Messages never
leak secrets or internal detail; clients should branch on `code`:

```json
{ "error": { "message": "rate limit exceeded", "code": "rate_limit_exceeded" } }
```

The `code` values are a fixed set (see the `Error` schema in the spec):
`unauthorized`, `forbidden`, `rate_limit_exceeded`, `session_limit_exceeded`,
`unavailable`, `invalid_request_error`, `not_found`, `method_not_allowed`,
`not_implemented`, and `internal_error`.

### Session limits

A server may cap conversation **sessions** in addition to per-request quota
(configured globally via the `server start` flags listed at the end of this
section). Two limits surface to clients, both with code `session_limit_exceeded`:

- **Concurrent sessions per key** — `POST /v1/sessions` returns `429` when the
  authenticated key already holds the maximum number of live sessions. End an
  existing session (`DELETE /v1/sessions/{id}`) or wait for one to idle out.
- **Per-session history** (turns / context tokens) — in stateful chat, when the
  server is configured to *reject* (rather than *trim*) on overflow, a turn that
  would exceed a session's history cap returns `409` from
  `POST /v1/chat/completions`. The default policy is *trim* (oldest turns are
  dropped), in which case turns never fail for this reason.

The relevant `server start` flags (each with an `AGENTGPU_*` env equivalent):
`--max-sessions-per-key` (0 = unlimited), `--max-session-turns` (default 200),
`--max-session-context-tokens` (0 = unlimited), and
`--session-overflow-policy` (`trim` | `reject`, default `trim`). The context-token
count is a whitespace-token estimate (there is no model tokenizer), consistent
with the rest of the project's token accounting.

For a task-oriented walkthrough of both conversation modes — with working,
copy-pasteable `curl` examples for creating a session, multi-turn chat,
streaming, and deletion, plus the model-warmth (`keep_alive`) behavior and the
limits above — see the [Multi-Turn Sessions guide](sessions.md).

### Streaming

The two inference endpoints (`/v1/chat/completions`, `/v1/completions`) return
either a single JSON body or a Server-Sent Events (SSE) stream, selected by the
request's `stream` field. When streaming, each event is a `data: <json>\n\n`
frame and the stream terminates with the literal `data: [DONE]\n\n` sentinel.

### Timestamps

All timestamp fields are Unix epoch **seconds** as integers, not RFC 3339
date-time strings.

## Examples

The spec embeds request and response examples for the high-traffic endpoints —
chat and text completions (including a sample SSE frame), and the admin key,
permission, quota, and stats endpoints. They render inline in the
[hosted reference](https://jaypetez.github.io/agent-gpu/), and the
[Quickstart](../README.md#quickstart) shows an end-to-end `curl` chat request.

## Rendering the reference locally

The hosted reference is produced by [Redocly](https://redocly.com/) from the
spec. To render the same static HTML locally:

```bash
make openapi-docs   # writes openapi.html (Redoc) from openapi.yaml
```

To validate the spec against OpenAPI 3.1 and the project ruleset:

```bash
make openapi-lint
```

Both targets use the pinned Redocly image, identical to the `openapi` job in CI,
so a locally rendered reference matches the hosted one exactly.
