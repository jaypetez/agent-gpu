# Running agent-gpu with Docker

agent-gpu ships two minimal, non-root container images built from a single
multi-stage [`Dockerfile`](../Dockerfile):

| Image | Purpose | Base | Approx. size |
| --- | --- | --- | --- |
| `server` | Public OpenAI-compatible API, auth, quotas, scheduling | `distroless/static` | ~23 MB |
| `worker` | Runs inference jobs against a local Ollama | `distroless/base` | ~50 MB |

Both images contain only the statically-linked `agentgpu` binary — no shell, no
package manager, and no build toolchain — and run as the non-root UID `65532`.

## Pulling published images

Release tags publish multi-arch (amd64 + arm64) images to the GitHub Container
Registry, listed on the [repository's Packages page](https://github.com/jaypetez/agent-gpu):

```bash
docker pull ghcr.io/jaypetez/agent-gpu/server:latest
docker pull ghcr.io/jaypetez/agent-gpu/worker:latest
```

## Building locally

The two runtime images are selected with `--target`:

```bash
docker build --target server -t agentgpu-server .
docker build --target worker -t agentgpu-worker .
```

The build cross-compiles to the requested platform on a native toolchain, so a
multi-arch build needs no slow emulation of the compiler:

```bash
docker buildx build --target server --platform linux/amd64,linux/arm64 -t agentgpu-server .
```

Three optional `--build-arg`s stamp build metadata into the binary:
`VERSION`, `COMMIT`, and `DATE`.

## Running the server

```bash
docker run -d --name agentgpu-server \
  -p 8080:8080 \
  -p 50051:50051 \
  -v agentgpu-data:/data \
  ghcr.io/jaypetez/agent-gpu/server:latest
```

- `8080` is the public OpenAI-compatible HTTP API; `50051` is the gRPC control
  plane that workers connect to.
- `/data` holds the key, quota, and session state and is declared a volume so it
  survives container restarts. The image already owns `/data` as UID `65532`.

The server is up once an unauthenticated request to `/v1/models` returns `401`
(there is no `/healthz` endpoint yet):

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8080/v1/models   # -> 401
```

Manage API keys with the same binary inside the container:

```bash
docker exec agentgpu-server /agentgpu key create --name my-agent
```

> The `key` commands write to the same `/data` volume, so run them against the
> server container (or any container with that volume mounted).

## Running a worker

A worker needs two pieces of deployment-specific configuration, so neither is
baked into the image:

```bash
docker run -d --name agentgpu-worker \
  -e AGENTGPU_SERVER_ADDR=server-host:50051 \
  -e AGENTGPU_OLLAMA_URL=http://host.docker.internal:11434 \
  ghcr.io/jaypetez/agent-gpu/worker:latest
```

- `AGENTGPU_SERVER_ADDR` is the gRPC server address — a bare `host:port`, **not**
  a URL. (Equivalent to `worker start --server host:port`.)
- `AGENTGPU_OLLAMA_URL` is the base URL of the Ollama the worker drives.

The worker is stateless and opens no inbound port, so it needs no published
ports and no volume.

## GPU access

On `main` the worker reaches the GPU **indirectly**: it sends inference to an
Ollama instance, and Ollama owns the accelerator. Point `AGENTGPU_OLLAMA_URL` at
an Ollama that has the GPU — for example Ollama running on the host, or its own
container started with the NVIDIA Container Toolkit:

```bash
# Ollama with the GPU, in its own container:
docker run -d --name ollama --gpus all -p 11434:11434 ollama/ollama

# Worker pointed at it (same host):
docker run -d --name agentgpu-worker \
  -e AGENTGPU_SERVER_ADDR=server-host:50051 \
  -e AGENTGPU_OLLAMA_URL=http://host.docker.internal:11434 \
  ghcr.io/jaypetez/agent-gpu/worker:latest
```

The worker image uses the glibc-based `distroless/base` (rather than the smaller
static base) so that, when in-container GPU detection lands, the NVIDIA Container
Toolkit can inject `nvidia-smi` and its libraries into a worker started with
`--gpus all`.

## Container gotchas

- **The image already binds `0.0.0.0`.** The binary's built-in defaults are
  loopback-only (`127.0.0.1`), which is unreachable from outside a container, so
  the server image sets `AGENTGPU_HTTP_LISTEN=0.0.0.0:8080` and
  `AGENTGPU_SERVER_LISTEN=0.0.0.0:50051`. You do not need to set these yourself;
  override them only to change the port.
- **`localhost` is the worker container, not the host.** Inside the worker
  container, `http://localhost:11434` points back at the worker itself. Set
  `AGENTGPU_OLLAMA_URL` to the Ollama service name (Compose/Kubernetes) or to
  `http://host.docker.internal:11434` to reach an Ollama on the Docker host.

## Configuration reference

Everything is configurable by environment variable; the server image presets
the listen addresses and state paths shown above.

| Variable | Applies to | Default in image | Meaning |
| --- | --- | --- | --- |
| `AGENTGPU_HTTP_LISTEN` | server | `0.0.0.0:8080` | Public HTTP API bind address |
| `AGENTGPU_SERVER_LISTEN` | server | `0.0.0.0:50051` | gRPC control-plane bind address |
| `AGENTGPU_STORE_PATH` | server | `/data/keys.json` | API-key store file |
| `AGENTGPU_QUOTA_PATH` | server | `/data/quota.json` | Quota counter checkpoint |
| `AGENTGPU_SESSION_PATH` | server | `/data/sessions.json` | Session + history checkpoint |
| `AGENTGPU_SERVER_ADDR` | worker | _(unset — required)_ | gRPC server `host:port` to connect to |
| `AGENTGPU_OLLAMA_URL` | worker | _(unset)_ | Local Ollama base URL (default `http://localhost:11434`) |

A multi-service local stack (server + worker + Ollama via Compose) is tracked
separately in [#18](https://github.com/jaypetez/agent-gpu/issues/18).
