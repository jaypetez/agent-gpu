# Go example client

A self-contained, dependency-free Go program that calls the agent-gpu
OpenAI-compatible API end to end: it lists the models your key may use, then runs
a chat completion (buffered, or streamed token-by-token with `-stream`),
surfacing the server's error envelope and `Retry-After` hints the way a real
client should.

It is **stdlib-only** — its [`go.mod`](go.mod) has zero dependencies — so you can
copy this directory anywhere and `go run` it without pulling agent-gpu's server
dependencies. The HTTP/SSE logic lives in [`client.go`](client.go) (a small
`Client` with `ListModels`, `Chat`, and `ChatStream`); flag parsing and the run
flow live in [`main.go`](main.go).

## Prerequisites

A running agent-gpu server + worker with a pulled model, and a **user API key**
(a real `agpu_<keyid>_<secret>` token — the server authenticates it). The fastest
way to get all of that is the Docker Compose stack, which pulls `qwen2:0.5b`
automatically — see the [Quickstart](../../README.md#quickstart) and
[docs/docker.md](../../docs/docker.md).

## Run it

From this directory:

```bash
# Non-streaming: prints the assistant reply, then a usage summary on stderr.
cd examples/go-client
AGENTGPU_API_KEY=agpu_… go run . -prompt "Explain goroutines in one sentence."

# Streaming: prints content deltas as they arrive (Ctrl-C stops cleanly).
AGENTGPU_API_KEY=agpu_… go run . -stream -prompt "Write a haiku about GPUs."
```

Build a standalone binary instead:

```bash
cd examples/go-client
go build -o go-client .
AGENTGPU_API_KEY=agpu_… ./go-client -model qwen2:0.5b -prompt "Say hi."
```

## Configuration

Each option is an environment variable with a flag override (flag > env >
default):

| Flag | Environment | Default | Purpose |
| --- | --- | --- | --- |
| `-base-url` | `AGENTGPU_BASE_URL` | `http://localhost:8080/v1` | API base URL (the `.../v1` prefix) |
| `-api-key` | `AGENTGPU_API_KEY` | _(none — **required**)_ | Bearer token (`agpu_<keyid>_<secret>`) |
| `-model` | `AGENTGPU_MODEL` | `qwen2:0.5b` | model to call |
| `-prompt` | — | `Say hello in one short sentence.` | the user prompt |
| `-stream` | — | `false` | stream the reply over SSE |

The program exits `0` on success, `1` on a configuration error (e.g. no API
key), `2` when the requested model is not available to your key (it prints the
models that are), and `3` when the API call itself fails (auth, quota, server
error). On a `429`/`503` with a `Retry-After` header it prints how long to wait.

## Tests

The client is covered by table-free unit tests that drive it against an
`httptest.Server` — no real server or model required:

```bash
cd examples/go-client
go test ./...
```

They verify non-streaming parsing, streaming delta accumulation and `[DONE]`
termination, a mid-stream `data: {"error":…}` frame being surfaced as an error,
model-list parsing, the error envelope on a non-2xx, and `Retry-After` parsing.

## Production Go apps

For real applications you can point the official OpenAI Go SDK at agent-gpu
instead of writing your own client:

```go
import (
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

client := openai.NewClient(
	option.WithBaseURL("http://localhost:8080/v1"),
	option.WithAPIKey("agpu_…"),
)
```

See [`../README.md`](../README.md) for Python, Node/TypeScript, and `curl`
examples against the same endpoint.
