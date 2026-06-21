# Examples

**agent-gpu is OpenAI-compatible — point the tools you already use at it.** The
public API speaks the OpenAI REST surface under a `/v1` base path, so any OpenAI
SDK (or plain `curl`) works against it: set the base URL to your agent-gpu server
and the API key to an `agpu_…` token. Everything below hits the same endpoint
with the same key and the `qwen2:0.5b` demo model.

| Example | What it shows |
| --- | --- |
| [`go-client/`](go-client/) | A complete, dependency-free Go client (model discovery, chat, streaming, error handling) you can copy and `go run`. |
| [Python](#python) · [Node / TypeScript](#node--typescript) · [curl](#curl) | The same calls with the official SDKs and a shell one-liner. |

## Prerequisites

You need a running agent-gpu **server + worker** with a **pulled model**, and a
**user API key** — a real `agpu_<keyid>_<secret>` token the server
authenticates. The quickest path to all of that is the Docker Compose stack,
which brings up the server, a worker, and Ollama and pulls `qwen2:0.5b` (~350 MB)
automatically:

- [Quickstart](../README.md#quickstart) — bring the stack up and mint a key.
- [docs/docker.md](../docs/docker.md) — the full Compose walkthrough.

In the snippets below, replace `agpu_…` with your user token. They assume the
server is reachable at `http://localhost:8080`; adjust the base URL for a remote
deployment.

> **Server-side only.** These examples run from a server or a CLI, not a browser.
> The API does **not** send CORS headers today, so a browser/JS client calling it
> directly will be blocked by the same-origin policy — front it with your own
> proxy (which can also hold the API key) if you need browser access.

## Go

The [`go-client/`](go-client/) directory is a full, runnable example — see its
[README](go-client/README.md) for details. In short:

```bash
cd examples/go-client
AGENTGPU_API_KEY=agpu_… go run . -prompt "Say hello in one short sentence."

# Stream the reply token-by-token:
AGENTGPU_API_KEY=agpu_… go run . -stream -prompt "Write a haiku about GPUs."
```

For production Go apps you can instead use the official
[`github.com/openai/openai-go`](https://github.com/openai/openai-go) SDK with
`option.WithBaseURL("http://localhost:8080/v1")` and `option.WithAPIKey("agpu_…")`.

## Python

Install the official SDK (`pip install openai`), then point it at agent-gpu:

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="agpu_…",
)

resp = client.chat.completions.create(
    model="qwen2:0.5b",
    messages=[{"role": "user", "content": "Say hello in one short sentence."}],
)
print(resp.choices[0].message.content)
```

Streaming is the same call with `stream=True`:

```python
stream = client.chat.completions.create(
    model="qwen2:0.5b",
    messages=[{"role": "user", "content": "Write a haiku about GPUs."}],
    stream=True,
)
for chunk in stream:
    delta = chunk.choices[0].delta.content
    if delta:
        print(delta, end="", flush=True)
print()
```

## Node / TypeScript

Install the official SDK (`npm i openai`):

```ts
import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "http://localhost:8080/v1",
  apiKey: "agpu_…",
});

const resp = await client.chat.completions.create({
  model: "qwen2:0.5b",
  messages: [{ role: "user", content: "Say hello in one short sentence." }],
});
console.log(resp.choices[0].message.content);
```

Streaming:

```ts
const stream = await client.chat.completions.create({
  model: "qwen2:0.5b",
  messages: [{ role: "user", content: "Write a haiku about GPUs." }],
  stream: true,
});
for await (const chunk of stream) {
  process.stdout.write(chunk.choices[0]?.delta?.content ?? "");
}
process.stdout.write("\n");
```

## curl

List the models your key may use:

```bash
curl http://localhost:8080/v1/models \
  -H "Authorization: Bearer agpu_…"
```

A non-streaming chat completion:

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer agpu_…" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen2:0.5b",
    "messages": [{"role": "user", "content": "Say hello in one short sentence."}]
  }'
```

Streaming — add `"stream": true` and use `-N` so curl prints each SSE frame as it
arrives (the stream ends with a literal `data: [DONE]`):

```bash
curl -N http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer agpu_…" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen2:0.5b",
    "messages": [{"role": "user", "content": "Write a haiku about GPUs."}],
    "stream": true
  }'
```
