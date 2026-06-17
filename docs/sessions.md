# Multi-Turn Sessions

agent-gpu supports **multi-turn conversations** on top of the OpenAI-compatible
chat API. A **session** is an owner-scoped handle to a model conversation that
lets the gateway keep the conversation's model **warm** on a single worker (a
hot KV cache) across turns, so follow-up turns avoid a cold model reload.

This guide is a practical, copy-pasteable walkthrough of both session modes, the
warmth behavior, and the limits/errors you may hit. For the formal contract see
the [API Reference](api-reference.md) and [`openapi.yaml`](../openapi.yaml); for
the implemented design see the
[Sessions section of the architecture doc](architecture.md#sessions).

> The examples use `qwen2:0.5b` as a small, fast model and a placeholder Bearer
> key (`agpu_<keyid>_<secret>`) — substitute a model your key is permitted to
> use and your own token. The default server address is `http://127.0.0.1:8080`.

## Overview

Without a session, every `POST /v1/chat/completions` request is independent: the
client sends the full `messages[]` array each time and the server routes the job
to whatever worker is the best fit at that moment. That works, but a follow-up
turn may land on a **different** worker, forcing a cold model load and a re-read
of the whole prompt.

A session fixes the conversation to one worker (its warm cache) and is offered in
**two modes**, distinguished only by **where you put the session id**:

| | Affinity (stateless) | Stateful |
| --- | --- | --- |
| Session id rides in | the **`X-Session-Id` header** | the **`session_id` body field** |
| Who owns the history | the **client** (you resend full `messages[]` each turn) | the **server** (it reconstructs context) |
| What you send per turn | the **entire** conversation | **only the new turn(s)** |
| What the server stores | **nothing** | the full conversation history |
| What the session buys you | warm-worker routing only | warm-worker routing **and** server-side history |

In both modes the gateway pins the conversation to its bound worker and **rebinds**
transparently if that worker drains, is evicted, goes stale, or no longer fits —
the next turn just succeeds on a new worker, with no client-visible failure.

### When to use which

- **Affinity (stateless)** — you already keep the conversation client-side (most
  OpenAI SDK usage does) and just want warm-cache routing. The server holds no
  conversation state, which keeps it lighter and means there is no per-session
  history cap to manage. You pay the bandwidth of resending the full history each
  turn.
- **Stateful** — you want the **simplest client**: create a session once, then
  send only each new user message and let the server remember the rest. The
  server reconstructs the full context from stored history and persists each
  completed turn. This is the easiest path for a thin client, at the cost of the
  server storing (and capping) the conversation.

Both modes support **streaming** (`stream: true`) and function/tool calling
across turns. If you somehow set **both** the header and the body field, the
**body wins** (stateful) — but the header's id still tags the job, so a stateful
conversation also routes to its warm worker.

## Affinity (stateless) mode

The client keeps the full conversation and resends it in `messages[]` every turn,
passing the session id in the **`X-Session-Id` header**. The server stores
nothing; the header only pins the job to the session's warm-cache worker.

### 1. Create a session

```bash
curl http://127.0.0.1:8080/v1/sessions \
  -H "Authorization: Bearer agpu_<keyid>_<secret>" \
  -H "Content-Type: application/json" \
  -d '{"model":"qwen2:0.5b"}'
```

Response (`201 Created`):

```json
{
  "id": "sess_a1b2c3d4e5f6",
  "object": "session",
  "model": "qwen2:0.5b",
  "created": 1718553600
}
```

Save the `id` — it is the value you send as `X-Session-Id` on every turn.

### 2. First turn

Send the conversation so far (here, one user message) plus the session id in the
header:

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer agpu_<keyid>_<secret>" \
  -H "X-Session-Id: sess_a1b2c3d4e5f6" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen2:0.5b",
    "messages": [
      {"role": "user", "content": "My name is Ada. Remember it."}
    ]
  }'
```

### 3. Follow-up turn (full history each time)

Because the server stores nothing in this mode, **you** carry the context: append
the assistant's previous reply and your new message, and send the whole array
again with the same `X-Session-Id`:

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer agpu_<keyid>_<secret>" \
  -H "X-Session-Id: sess_a1b2c3d4e5f6" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen2:0.5b",
    "messages": [
      {"role": "user", "content": "My name is Ada. Remember it."},
      {"role": "assistant", "content": "Got it, Ada."},
      {"role": "user", "content": "What is my name?"}
    ]
  }'
```

The turn routes back to the same warm worker (its KV cache already holds the
model), so there is no cold reload.

### 4. Streaming a turn

Set `stream: true` to receive Server-Sent Events. Each event is a
`data: <json>\n\n` frame and the stream ends with the literal `data: [DONE]\n\n`
sentinel:

```bash
curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer agpu_<keyid>_<secret>" \
  -H "X-Session-Id: sess_a1b2c3d4e5f6" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen2:0.5b",
    "stream": true,
    "messages": [
      {"role": "user", "content": "My name is Ada. Remember it."},
      {"role": "assistant", "content": "Got it, Ada."},
      {"role": "user", "content": "Spell my name letter by letter."}
    ]
  }'
```

Sample frames:

```text
data: {"id":"chatcmpl-…","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"}}]}

data: {"id":"chatcmpl-…","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"A"}}]}

data: {"id":"chatcmpl-…","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"-d"}}]}

data: {"id":"chatcmpl-…","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]
```

`curl -N` disables buffering so you see frames as they arrive.

### 5. Delete the session

When you are done, end the session. This frees the conversation's model on its
bound worker promptly (see [Warmth](#warmth--kv-cache-keep_alive)):

```bash
curl -X DELETE http://127.0.0.1:8080/v1/sessions/sess_a1b2c3d4e5f6 \
  -H "Authorization: Bearer agpu_<keyid>_<secret>"
```

Returns `204 No Content`. A session you do not delete idles out on its own once
its TTL elapses.

## Stateful mode

The server owns the history. You create a session once, then send **only the new
turn(s)** plus the session id in the **`session_id` body field**; the server
prepends the stored history before dispatch and persists each completed turn.

### 1. Create a session

Identical to affinity mode — the session object is the same; only how you
reference it on chat turns differs:

```bash
curl http://127.0.0.1:8080/v1/sessions \
  -H "Authorization: Bearer agpu_<keyid>_<secret>" \
  -H "Content-Type: application/json" \
  -d '{"model":"qwen2:0.5b"}'
```

Response (`201 Created`):

```json
{
  "id": "sess_a1b2c3d4e5f6",
  "object": "session",
  "model": "qwen2:0.5b",
  "created": 1718553600
}
```

### 2. First turn

Send only the new user message and reference the session by `session_id` in the
body (no `X-Session-Id` header needed):

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer agpu_<keyid>_<secret>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen2:0.5b",
    "session_id": "sess_a1b2c3d4e5f6",
    "messages": [
      {"role": "user", "content": "My name is Ada. Remember it."}
    ]
  }'
```

The server stores this user turn and the assistant reply after a successful
response.

### 3. Follow-up turn (only the new message)

Send just the new message — the server reconstructs the rest from its stored
history:

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer agpu_<keyid>_<secret>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen2:0.5b",
    "session_id": "sess_a1b2c3d4e5f6",
    "messages": [
      {"role": "user", "content": "What is my name?"}
    ]
  }'
```

A turn is persisted **atomically** (your new message(s) **and** the assistant
reply together) and **only on success** — a failed inference stores nothing, so
the conversation never ends up with a half turn.

### 4. Streaming a turn

Streaming works the same way; the server accumulates the assistant reply as it
emits frames and persists the completed turn after `[DONE]`:

```bash
curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer agpu_<keyid>_<secret>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen2:0.5b",
    "session_id": "sess_a1b2c3d4e5f6",
    "stream": true,
    "messages": [
      {"role": "user", "content": "Spell my name letter by letter."}
    ]
  }'
```

If the client **disconnects mid-stream**, the upstream job is aborted and
**nothing is persisted** (no orphaned user turn, no truncated reply), so the
session stays consistent and you can simply retry the turn.

### 5. Inspect the stored conversation (optional)

Because the server owns the history, you can fetch it back — rendered in the same
OpenAI message wire shape you would send — to inspect or replay it:

```bash
curl http://127.0.0.1:8080/v1/sessions/sess_a1b2c3d4e5f6 \
  -H "Authorization: Bearer agpu_<keyid>_<secret>"
```

Response (`200 OK`):

```json
{
  "id": "sess_a1b2c3d4e5f6",
  "object": "session",
  "model": "qwen2:0.5b",
  "created": 1718553600,
  "last_active": 1718553720,
  "messages": [
    {"role": "user", "content": "My name is Ada. Remember it."},
    {"role": "assistant", "content": "Got it, Ada."},
    {"role": "user", "content": "What is my name?"},
    {"role": "assistant", "content": "Your name is Ada."}
  ]
}
```

### 6. Delete the session

```bash
curl -X DELETE http://127.0.0.1:8080/v1/sessions/sess_a1b2c3d4e5f6 \
  -H "Authorization: Bearer agpu_<keyid>_<secret>"
```

Returns `204 No Content` and **purges the stored history**.

> **Ownership & privacy.** Every session is scoped to the key that created it. A
> request for a session that is missing, owned by another key, or expired returns
> the same `404` — the API never reveals whether another owner's session exists.

## Warmth / KV cache (`keep_alive`)

Ollama keeps a model resident in VRAM for a while after each request and unloads
it once an idle timer elapses (its default is **5 minutes**); each request may
override that window with a
[`keep_alive`](https://github.com/ollama/ollama/blob/main/docs/api.md#parameters)
value. agent-gpu coordinates that timer with the session lifecycle so an active
conversation does not pay a **cold model reload** between turns, while an idle or
abandoned conversation still frees its VRAM.

### How it works

- **Warm window tied to the idle TTL.** For a **session-bound** turn the server
  sets the job's `keep_alive` to `min(session TTL, --model-warm-max)`. A session
  with no idle TTL (never-idle) falls back to the `--model-warm-max` cap. The
  value is always **bounded and positive** — the warm path never sends a
  "forever" `keep_alive`. A **session-less** request sends no `keep_alive` at
  all, so Ollama's own default applies and the non-session path is byte-identical
  to before.
- **Stays warm across active turns.** Affinity routes every turn of a session to
  the same bound worker, and each turn **re-sends** `keep_alive`, which **resets**
  Ollama's unload timer. So while a conversation is active its model never unloads
  mid-flight — no cold reload between turns. The benefit is a warm **KV cache**:
  the model stays resident and the prompt does not have to be reprocessed on a
  different GPU.
- **Released when idle or abandoned.** Once the last turn's `keep_alive` window
  elapses with no new turn, Ollama unloads the model on its own — no control
  message needed. Because the window is bounded (≤ the cap), an idle or abandoned
  session's model is always freed within that window and can never pin VRAM
  indefinitely.
- **Released promptly on explicit end.** On `DELETE /v1/sessions/{id}` the server
  additionally asks the session's **bound worker** to unload the model **now**
  (best-effort), rather than waiting for the warm window to elapse. This is
  fire-and-forget and never blocks or fails the delete; if the worker is gone or
  the message is dropped, the idle `keep_alive` timer is the backstop. An
  **unbound** session (no turn taken) has nothing resident to release.

### VRAM vs. latency trade-off

A longer warm window keeps more models resident at once, which interacts with
Ollama's concurrent-model capacity on the worker host — `OLLAMA_NUM_PARALLEL`
and `OLLAMA_MAX_LOADED_MODELS`. **Warmth trades VRAM for latency**, and the cap
bounds that trade:

- **Lower `--model-warm-max`** on VRAM-constrained workers so idle sessions
  release their models sooner.
- **Raise `--model-warm-max`** (or the session TTL) where long pauses between
  turns are common and VRAM is plentiful.

To observe warmth and affinity effectiveness in aggregate — affinity hit/miss
rate (`agentgpu_affinity_total{result}`), rebinds
(`agentgpu_session_rebinds_total`), live sessions (`agentgpu_active_sessions`),
and per-session turn/duration histograms (`agentgpu_session_turns`,
`agentgpu_session_duration_seconds`) — see [docs/metrics.md](metrics.md).

## Limits & errors

Sessions are bounded by a few operator-configured caps. Each is a `server start`
flag with an `AGENTGPU_*` environment equivalent (resolved **flag > env >
default**). The errors use the standard `{"error":{"message","code"}}` envelope.

| Limit | Flag (env) | Default | When exceeded |
| --- | --- | --- | --- |
| Concurrent sessions per key | `--max-sessions-per-key` (`AGENTGPU_MAX_SESSIONS_PER_KEY`) | `0` (unlimited) | `POST /v1/sessions` → **429** `session_limit_exceeded` |
| History turn cap | `--max-session-turns` (`AGENTGPU_MAX_SESSION_TURNS`) | `200` | trim oldest, or **409** `session_limit_exceeded` under `reject` |
| History context-token cap | `--max-session-context-tokens` (`AGENTGPU_MAX_SESSION_CONTEXT_TOKENS`) | `0` (unlimited) | trim oldest, or **409** `session_limit_exceeded` under `reject` |
| Overflow policy | `--session-overflow-policy` (`AGENTGPU_SESSION_OVERFLOW_POLICY`) | `trim` | selects trim vs. reject above |
| Idle TTL | `--session-ttl` (`AGENTGPU_SESSION_TTL`) | `30m` | session is reaped after this long idle |
| Model warm cap | `--model-warm-max` (`AGENTGPU_MODEL_WARM_MAX`) | `1h` | bounds the `keep_alive` window (see [Warmth](#warmth--kv-cache-keep_alive)) |

### Concurrent-session limit → 429

When `--max-sessions-per-key` is set to a positive value and the authenticated
key already holds that many live sessions, `POST /v1/sessions` is refused:

```json
{ "error": { "message": "concurrent session limit reached", "code": "session_limit_exceeded" } }
```

Back off, then end an existing session (`DELETE /v1/sessions/{id}`) or wait for
one to idle out, and retry. The default (`0`) is unlimited.

### History caps → trim (default) or 409

In **stateful** mode the server caps a session's history by **turn count**
(`--max-session-turns`, default 200) and optionally by **cumulative context
tokens** (`--max-session-context-tokens`, default unlimited). The context-token
count is a whitespace-token estimate (there is no model tokenizer), consistent
with the rest of the project's token accounting. There is also an internal
cumulative-byte cap (1 MiB) bounding memory growth.

What happens at the cap is set by `--session-overflow-policy`:

- **`trim`** (default) — the oldest turns are dropped so the most recent context
  is retained and the turn **always succeeds**. You never see an error for this.
  A single turn larger than the byte cap on its own is still stored (so a session
  can never become permanently unwritable).
- **`reject`** — a turn that would exceed a cap is refused before any inference
  runs, and `POST /v1/chat/completions` returns **409 Conflict**:

  ```json
  { "error": { "message": "session history limit reached", "code": "session_limit_exceeded" } }
  ```

  Start a new session (or shorten the conversation) rather than retrying the same
  turn. (These caps apply to server-stored history, i.e. **stateful** mode;
  affinity mode stores nothing server-side, so no per-session history cap
  applies there.)

### Idle expiry (TTL)

A session untouched for longer than `--session-ttl` (default `30m`) is reaped by
a background sweeper, which deletes the session **and** its history. Any activity
on a session — creating it, reading it, or appending a turn — resets its idle
clock. A session whose TTL is non-positive never idles out. After expiry, a
request for that session returns the same uniform `404` as a missing or
not-owned session.

### Other status codes

- **401** — missing/malformed/unknown/revoked key (every endpoint).
- **404** — session missing, owned by another key, or expired (uniform; no
  existence leak).
- **400** — malformed request body.
- **500** — an unexpected server error.

## See also

- [API Reference](api-reference.md) — the cross-cutting conventions and the link
  to the formal contract.
- [`openapi.yaml`](../openapi.yaml) — the machine-readable spec for the session
  endpoints (`POST /v1/sessions`, `GET`/`DELETE /v1/sessions/{id}`) and the chat
  endpoint's session inputs.
- [Architecture: Sessions](architecture.md#sessions) — the implemented design:
  the two modes, the manager/lifecycle, affinity scheduling, history caps, and
  [model warmth](architecture.md#model-warmth-keep_alive).
- [Metrics (Prometheus)](metrics.md) — session-affinity and session-activity
  metrics.
- [Developer Guide](developer-guide.md) — running the stack locally and the
  contribution workflow.
