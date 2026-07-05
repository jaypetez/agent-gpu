import { defineConfig, devices, type ReporterDescription } from "@playwright/test";
import { spawnSync } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { binaryPath } from "./e2e/helpers";

// Playwright config for the agent-gpu admin console E2E (issues #100–#105). The
// specs drive a REAL built binary headless: the config seeds a deterministic admin
// key into a throwaway store and starts `agentgpu server start` against it; a
// globalSetup (e2e/global-setup.ts) then seeds the runtime-only state — a connected
// worker + sample usage/telemetry/logs — so every screen renders populated,
// deterministic data. The specs exercise the login → dashboard → workers → keys →
// observability flows and assert WCAG AA accessibility with @axe-core/playwright.
// CI is the arbiter — browsers may not be available in the implementer's local
// environment — so the config is written to run unattended (seed → serve → setup →
// test → JSON + screenshot artifacts).
//
// IMPORTANT: Playwright re-imports this config module in EACH worker process, so any
// seeding done at import time would run multiple times with different throwaway
// stores — the webServer would hold one store while a worker authenticated against
// another. To make the seed run exactly once and be shared across processes, the
// seeded {storePath, token} is persisted to a fixed state file under the OS temp
// dir; the first eval seeds and writes it, every later eval reads it.
//
// ISOLATION MODEL (#105, AC3): one shared server, run serially (workers:1,
// fullyParallel:false), seeded ONCE by the config (admin key) + globalSetup (worker
// + sample traffic). "Every test starts from a known state" is met not by a fresh
// server per test (that would blow the ~30s budget) but by: (a) the deterministic
// global seed established before any spec; (b) mutating specs creating records with
// unique names (helpers.uniqueName) and cleaning up after themselves; and (c)
// read-only specs asserting against the seeded baseline. There is therefore no
// inter-test state leakage despite the shared server.

const httpPort = process.env.AGENTGPU_E2E_HTTP_PORT || "18080";
const grpcPort = process.env.AGENTGPU_E2E_GRPC_PORT || "18111";
const baseURL = `http://127.0.0.1:${httpPort}`;

// stateFile is the run-shared marker holding the seeded store path + admin token.
// Keyed by the HTTP port so concurrent suites on different ports don't collide.
const stateFile = join(tmpdir(), `agpu-e2e-state-${httpPort}.json`);

interface SeedState {
  storePath: string;
  token: string;
}

// seedOnce returns the shared seed state, creating it (once) if absent: it mints an
// admin key into a fresh throwaway store and records its one-time token. Subsequent
// evals (worker processes) read the same file, so the server and the spec agree on
// exactly one store + token. This is the file-store half of the seed; the runtime
// half (worker + sample traffic) is e2e/global-setup.ts, which runs after the server
// is up.
function seedOnce(): SeedState {
  if (existsSync(stateFile)) {
    return JSON.parse(readFileSync(stateFile, "utf-8")) as SeedState;
  }
  const storeDir = mkdtempSync(join(tmpdir(), "agpu-e2e-"));
  const storePath = join(storeDir, "keys.json");
  const res = spawnSync(
    binaryPath,
    ["key", "create", "--local", "--store", storePath, "--name", "e2e", "--role", "admin"],
    { encoding: "utf-8" },
  );
  if (res.status !== 0) {
    throw new Error(
      `failed to seed admin key (is the binary built at ${binaryPath}?):\n${res.stdout || ""}${res.stderr || ""}`,
    );
  }
  const match = (res.stdout || "").match(/agpu_[a-f0-9]+_[a-f0-9]+/);
  if (!match) {
    throw new Error(`could not parse the admin token from key-create output:\n${res.stdout}`);
  }
  const state: SeedState = { storePath, token: match[0] };
  writeFileSync(stateFile, JSON.stringify(state), "utf-8");
  return state;
}

const { storePath, token } = seedOnce();
// Expose to the spec + globalSetup (read in-process; they also have a file fallback).
process.env.AGENTGPU_E2E_TOKEN = token;
process.env.AGENTGPU_E2E_STATE_FILE = stateFile;
process.env.AGENTGPU_E2E_BASE_URL = baseURL;

// Reporters: keep the human-readable list output (and the GitHub annotations in CI)
// AND add the native JSON reporter so the run is machine-parseable (#105, AC5). The
// JSON goes to a gitignored file under test-results/ so an agent can read a failure
// and fix it in one loop iteration without scraping stdout.
const jsonReporter: ReporterDescription = ["json", { outputFile: "test-results/results.json" }];
const reporter: ReporterDescription[] = process.env.CI
  ? [["github"], ["list"], jsonReporter]
  : [["list"], jsonReporter];

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  reporter,
  outputDir: "./test-results",
  // globalSetup seeds the runtime-only state (a connected worker + sample
  // usage/telemetry/logs) once the webServer is up, before any spec runs.
  globalSetup: "./e2e/global-setup.ts",
  use: {
    baseURL,
    // Locators must be role/label based (never CSS/XPath); a screenshot on failure
    // plus an explicit success screenshot are saved as CI artifacts.
    screenshot: "only-on-failure",
    trace: "retain-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: {
    command: `"${binaryPath}" server start --listen 127.0.0.1:${grpcPort} --http-listen 127.0.0.1:${httpPort} --metrics-listen "" --store "${storePath}"`,
    url: `${baseURL}/admin/login`,
    reuseExistingServer: !process.env.CI,
    timeout: 60_000,
    stdout: "pipe",
    stderr: "pipe",
  },
});
