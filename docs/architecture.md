# Architecture

agent-gpu uses a **server + worker** model.

- **Server** — the single entry point. Owns the public API, authenticates API keys, enforces
  permissions and quotas, maintains the job queue, and schedules jobs onto workers.
- **Worker** — runs on any machine with [Ollama](https://ollama.com). Registers with the server,
  reports its capacity via heartbeats, and executes inference jobs against local Ollama.

## System overview

```mermaid
flowchart LR
  subgraph Clients
    A[Agents / OpenAI SDKs]
  end

  subgraph Server["agent-gpu server"]
    AUTH[Auth / API keys]
    PERM[Permissions]
    QUOTA[Quotas + rate limiting]
    SCHED[Capacity-aware scheduler]
    QUEUE[(Job queue)]
    STORE[(State: keys · quotas · perms)]
  end

  A -->|HTTPS + API key| AUTH
  AUTH --> PERM --> QUOTA --> QUEUE --> SCHED
  AUTH -.-> STORE
  QUOTA -.-> STORE
  PERM -.-> STORE

  SCHED <-->|register · heartbeat · dispatch| W1[Worker 1]
  SCHED <-->|register · heartbeat · dispatch| W2[Worker 2]
  SCHED <-->|register · heartbeat · dispatch| Wn[Worker N]

  W1 --> O1[Ollama] --> G1[(GPU / CPU)]
  W2 --> O2[Ollama] --> G2[(GPU / CPU)]
  Wn --> On[Ollama] --> Gn[(GPU / CPU)]
```

Workers continuously report GPU type, total/free VRAM, current load, active job count, and the
set of models they have available. The scheduler uses these signals — plus the requesting key's
priority — to route each job to a best-fit worker.

## Request flow

```mermaid
sequenceDiagram
  participant C as Client
  participant S as Server
  participant Q as Queue
  participant Sch as Scheduler
  participant W as Worker
  participant O as Ollama

  C->>S: POST /v1/chat/completions (Bearer key)
  S->>S: Authenticate key
  S->>S: Check permissions (model allowed?)
  S->>S: Check quota / rate limit
  alt over quota
    S-->>C: 429 Too Many Requests (Retry-After)
  else not permitted
    S-->>C: 403 Forbidden
  else accepted
    S->>Q: Enqueue job (model, priority)
    Sch->>Q: Dequeue (priority then FIFO)
    Sch->>W: Dispatch to best-fit worker
    W->>O: Run inference
    O-->>W: Stream tokens
    W-->>S: Stream tokens
    S-->>C: Stream response (SSE)
  end
```

## Capacity-aware scheduling

For each job the scheduler scores candidate workers by:

1. **Model availability** — prefer workers that already have the model loaded.
2. **Free VRAM** — the model must fit; larger models go only where they fit.
3. **Current load / active jobs** — spread work and avoid hotspots.
4. **API-key priority** — higher-priority keys win under contention.

If no worker currently fits, the job is **queued** (never silently dropped) and re-evaluated as
capacity frees up. Queue depth and per-worker load are exported as metrics.

## State

Authentication, permission rules, and quota counters are persisted so they survive restarts.
The Docker Compose environment backs this with Redis/Postgres; standalone deployments may use an
embedded store. See the relevant milestones on the roadmap for specifics.

<!-- ci: ruleset + admin-merge verification -->
