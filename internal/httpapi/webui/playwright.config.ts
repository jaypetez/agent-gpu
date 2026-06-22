import { defineConfig, devices } from "@playwright/test";
import { spawnSync } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

// Playwright config for the agent-gpu admin console E2E (issue #100). The spec
// drives a REAL built binary headless: it seeds a deterministic admin key into a
// throwaway store, starts `agentgpu server start` against it, then exercises the
// login → dashboard flow and asserts WCAG AA accessibility with @axe-core/playwright
// on the login, shell, and dashboard screens. CI is the arbiter — browsers may not
// be available in the implementer's local environment — so the config is written to
// run unattended (seed → serve → test → screenshot artifact).
//
// IMPORTANT: Playwright re-imports this config module in EACH worker process, so any
// seeding done at import time would run multiple times with different throwaway
// stores — the webServer would hold one store while a worker authenticated against
// another. To make the seed run exactly once and be shared across processes, the
// seeded {storePath, token} is persisted to a fixed state file under the OS temp
// dir; the first eval seeds and writes it, every later eval reads it.

// This config lives at internal/httpapi/webui/ (co-located with the console source
// so the Tailwind build resolves tailwindcss from node_modules — see package.json),
// so the repo root is three levels up.
const repoRoot = join(__dirname, "..", "..", "..");
const isWindows = process.platform === "win32";
const binaryName = isWindows ? "agentgpu.exe" : "agentgpu";
// `make ui-e2e` / CI builds the binary at the repo root and passes AGENTGPU_BIN;
// the repoRoot fallback covers a bare `npx playwright test` from this dir.
const binary = process.env.AGENTGPU_BIN || join(repoRoot, binaryName);

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
// exactly one store + token.
function seedOnce(): SeedState {
  if (existsSync(stateFile)) {
    return JSON.parse(readFileSync(stateFile, "utf-8")) as SeedState;
  }
  const storeDir = mkdtempSync(join(tmpdir(), "agpu-e2e-"));
  const storePath = join(storeDir, "keys.json");
  const res = spawnSync(
    binary,
    ["key", "create", "--local", "--store", storePath, "--name", "e2e", "--role", "admin"],
    { encoding: "utf-8" },
  );
  if (res.status !== 0) {
    throw new Error(
      `failed to seed admin key (is the binary built at ${binary}?):\n${res.stdout || ""}${res.stderr || ""}`,
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
// Expose to the spec (read in-process; the spec also has a file fallback).
process.env.AGENTGPU_E2E_TOKEN = token;
process.env.AGENTGPU_E2E_STATE_FILE = stateFile;
process.env.AGENTGPU_E2E_BASE_URL = baseURL;

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  reporter: process.env.CI ? [["github"], ["list"]] : "list",
  outputDir: "./test-results",
  use: {
    baseURL,
    // Locators must be role/label based (never CSS/XPath); a screenshot on failure
    // plus an explicit success screenshot are saved as CI artifacts.
    screenshot: "only-on-failure",
    trace: "retain-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: {
    command: `"${binary}" server start --listen 127.0.0.1:${grpcPort} --http-listen 127.0.0.1:${httpPort} --metrics-listen "" --store "${storePath}"`,
    url: `${baseURL}/admin/login`,
    reuseExistingServer: !process.env.CI,
    timeout: 60_000,
    stdout: "pipe",
    stderr: "pipe",
  },
});
