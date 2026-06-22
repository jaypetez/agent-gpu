import { request, type FullConfig } from "@playwright/test";
import { spawn, type ChildProcess } from "node:child_process";
import { binaryPath, BASE_URL, GRPC_PORT, resolveToken } from "./helpers";

// global-setup.ts — the deterministic seed step for the admin console E2E (#105,
// AC3). It runs ONCE per `playwright test` invocation, AFTER the webServer is up
// (Playwright resolves the webServer readiness probe before invoking globalSetup),
// and BEFORE any spec. The admin key itself is seeded by the config's file-store
// step (key create --local --store <tmp> → server start --store <tmp>); this setup
// adds the runtime-only state that cannot be file-seeded:
//
//   - >=1 connected worker (spawned `agentgpu worker start`, deterministic id), so
//     the Workers + GPU heatmap screens render a populated fleet; and
//   - sample usage/telemetry/logs by driving the admin/stats endpoints, so the
//     observability screens render real, non-empty data.
//
// Establishing this baseline once is the foundation of the isolation model
// documented in playwright.config.ts: a known global seed, mutating specs use
// unique names + clean up, read-only specs assert against the baseline. The worker
// process is long-lived for the whole run and torn down by the returned teardown.

// WORKER_ID is the deterministic id of the seeded worker. It is shared with
// workers.spec.ts (which types it verbatim into the force-evict confirm), so it is
// exported and re-imported there — one source of truth. It is long enough to
// exercise the shortID truncation in the UI while remaining a single path segment.
export const WORKER_ID = "e2e-worker-0001";

// pollUntil polls predicate() every 250ms until it returns true or the deadline
// passes. Used to wait for the seeded worker to register before specs run, so the
// fleet is populated deterministically rather than racing the first poll.
async function pollUntil(label: string, timeoutMs: number, predicate: () => Promise<boolean>) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    if (await predicate().catch(() => false)) {
      return;
    }
    if (Date.now() > deadline) {
      throw new Error(`seed timed out waiting for ${label} after ${timeoutMs}ms`);
    }
    await new Promise((r) => setTimeout(r, 250));
  }
}

export default async function globalSetup(_config: FullConfig): Promise<() => Promise<void>> {
  const token = resolveToken();
  if (!token) {
    throw new Error("globalSetup: AGENTGPU_E2E_TOKEN/STATE_FILE not set by the Playwright config seed");
  }

  // An API client carrying the seeded admin token, pointed at the running server.
  const api = await request.newContext({
    baseURL: BASE_URL,
    extraHTTPHeaders: { Authorization: `Bearer ${token}` },
  });

  // 1. Seed a worker. GPU detection is off and a manual GPU type + VRAM are supplied
  //    so the worker reports stable capacity without real hardware or a reachable
  //    Ollama (it still registers and heartbeats via the --models fallback).
  const worker: ChildProcess = spawn(
    binaryPath,
    [
      "worker", "start",
      "--server", `127.0.0.1:${GRPC_PORT}`,
      "--id", WORKER_ID,
      "--models", "llama3",
      "--gpu-detect=false",
      "--gpu-type", "NVIDIA RTX 4090",
      "--total-vram", `${24 * 1024 * 1024 * 1024}`,
      "--heartbeat-interval", "1s",
    ],
    { stdio: "ignore" },
  );
  worker.unref?.();

  // Wait until the fleet lists the seeded worker, so every spec starts from a known
  // populated state (the worker needs a heartbeat or two to register). The admin
  // list envelope is { data: [...], pagination: {...} } (see pagination.go).
  await pollUntil("the seeded worker to register", 30_000, async () => {
    const res = await api.get("/v1/admin/workers");
    if (!res.ok()) {
      return false;
    }
    const body = (await res.json()) as { data?: Array<{ id?: string }> };
    return (body.data ?? []).some((w) => w.id === WORKER_ID);
  });

  // 2. Generate sample usage/telemetry/logs. Each authenticated admin call records a
  //    request line in the in-memory log ring and increments the request counters
  //    the telemetry/usage screens read, so those screens render non-empty on a
  //    clean run. Failures are non-fatal — the goal is to populate the buffers.
  for (let i = 0; i < 8; i++) {
    await api.get("/v1/admin/stats").catch(() => undefined);
  }

  await api.dispose();

  // Teardown: kill the long-lived worker when the whole run finishes. Playwright
  // calls the function returned from globalSetup as the global teardown.
  return async () => {
    worker.kill();
  };
}
