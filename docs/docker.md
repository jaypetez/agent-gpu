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

## Docker Compose (local dev stack)

[`compose.yaml`](../compose.yaml) brings up the whole stack with one command: the
`server`, one or more `worker`s, a local `ollama` (plus a one-shot `ollama-init`
that pulls a tiny model so there is something to serve), and the `redis` /
`postgres` backing services. It targets local development and demos — production
orchestration (Kubernetes, etc.) is out of scope.

### One-command bring-up

```bash
docker compose up -d --build
```

That builds the `server` and `worker` images from the [`Dockerfile`](../Dockerfile),
starts Ollama, pulls the default model (`qwen2:0.5b`, ~350 MB) via the one-shot
`ollama-init`, and starts the server and a worker. Configuration is read from an
optional `.env` (copy [`.env.example`](../.env.example)); every value has a
built-in default, so the stack also runs with no `.env` at all.

The server publishes the public API on `localhost:8080`. The gRPC control plane
(`50051`) stays on the Compose network — workers reach it as `server:50051` and it
needs no host port. The server is up once an unauthenticated request returns
`401` (there is no `/healthz`):

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8080/v1/models   # -> 401
```

> The `server` and `worker` images are distroless (no shell, no `curl`), so they
> carry no in-container healthcheck — readiness is checked from the host as above.
> Start order does not matter: the worker reconnects to the server with
> exponential backoff and tolerates the server not being ready yet.

### Scaling workers

The worker does **not** pin its `--id` (it defaults to the container hostname), so
scaling the service gives each replica a unique id and the fleet shows them all:

```bash
docker compose up -d --scale worker=3
```

### Bootstrapping keys

There is no auto-seeded admin key. Mint keys with the same binary inside the
running `server` container; they are written to the `/data` volume. The plaintext
token is printed **once** — save it.

```bash
# An admin key (sees every model; can list the fleet and run inference).
docker compose exec server /agentgpu key create --name admin --role admin

# A user key scoped to a single model (deny-by-default otherwise).
docker compose exec server /agentgpu key create --name app --role user --allow-model qwen2:0.5b
```

> **Restart the server after creating keys.** The file-backed key store is loaded
> into memory **once at server start** and is not hot-reloaded, so a key created
> while the server is already running is on the `/data` volume but the running
> server does not see it yet — it returns `401 invalid api key` until it reloads.
> Create the keys you need, then:
>
> ```bash
> docker compose restart server
> ```
>
> After the restart the new keys authenticate. (The richer admin/key HTTP
> endpoints under `/v1/admin/keys` _do_ take effect immediately, because they
> mutate the running server's own store — but they require an existing admin key,
> which is exactly the bootstrap chicken-and-egg the restart solves.)

Verify the worker registered (admin key required, after the restart above):

```bash
curl -s -H "Authorization: Bearer $ADMIN_TOKEN" http://localhost:8080/v1/admin/workers
```

### Running an inference request

A model is only routable once a worker advertises it. The worker refreshes its
model list from Ollama's `/api/tags` every heartbeat, so once `ollama-init` has
pulled the model it shows up shortly in the per-key catalog. Wait for it, then
send the request (a request for a model no worker serves **queues and blocks**
rather than erroring, so do not skip the wait):

```bash
# Wait until the model is advertised to this key.
until curl -s -H "Authorization: Bearer $USER_TOKEN" http://localhost:8080/v1/models \
  | grep -q '"id":"qwen2:0.5b"'; do sleep 2; done

# Then run a chat completion.
curl -s -H "Authorization: Bearer $USER_TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"qwen2:0.5b","messages":[{"role":"user","content":"Say hi in one word."}]}' \
  http://localhost:8080/v1/chat/completions
```

To use a host GPU, give Ollama the accelerator: uncomment the
`deploy.resources.reservations.devices` block on the `ollama` service in
`compose.yaml` (requires the NVIDIA Container Toolkit). The default is CPU so the
stack runs anywhere.

### Persistence across restarts

Keys, quotas, and sessions are JSON files on the named `agentgpu-data` volume
(atomic writes), so they survive a container restart. Because the server also
loads that store at start, the restart that activates a CLI-created key is the
same step that proves persistence:

```bash
docker compose exec server /agentgpu key create --name persists --role admin   # save the token
docker compose restart server
# The same token now authenticates — the key came back from the volume on reload:
curl -s -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer $TOKEN" http://localhost:8080/v1/admin/workers      # -> 200
```

Removing the volume (`docker compose down -v`) is what actually discards state; a
plain `restart` or `down`/`up` keeps it.

> **Redis and Postgres are provisioned but not yet wired.** The stack starts
> `redis` and `postgres` (with their own named volumes and healthchecks) because
> the [architecture](architecture.md) backs durable state with them and the store
> interfaces are shaped for that backend swap. Today, however, **nothing in the
> code connects to them** — all state persists to the `agentgpu-data` file-store
> volume shown above. They are scaffolding for the forthcoming backend
> integration, not the live state store.

### Teardown

```bash
docker compose down       # stop the stack, KEEP volumes (state persists)
docker compose down -v    # stop the stack and REMOVE volumes (clean teardown)
```

### Makefile shortcuts

```bash
make compose-up       # docker compose up -d --build
make compose-down     # docker compose down            (keep volumes)
make compose-clean    # docker compose down -v          (clean teardown)
make compose-config   # docker compose config -q        (validate compose.yaml)
make compose-e2e      # full bootstrap + inference smoke test (scripts/compose-e2e.sh)
```

`make compose-e2e` runs [`scripts/compose-e2e.sh`](../scripts/compose-e2e.sh),
which brings the stack up, asserts a worker registers, proves a key survives a
server restart, pulls the model, and runs a real chat completion — then tears the
stack down with `down -v`.
