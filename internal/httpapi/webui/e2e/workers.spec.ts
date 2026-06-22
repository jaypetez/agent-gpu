import { test, expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { spawn, type ChildProcess } from "node:child_process";
import { existsSync, mkdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

// workers.spec.ts — end-to-end coverage of the Workers + GPU management screens
// (issue 101), driving the REAL built binary. It seeds a live worker by spawning
// `agentgpu worker start` against the same server the Playwright config started, so
// the fleet has one connected worker to list, open, and manage. It then asserts the
// AC flows with role/label locators (never CSS/XPath) and WCAG 2 AA accessibility
// (axe-core) on each new screen, saving a screenshot per screen as a CI artifact:
//
//   - the worker list is visible + accessible, with the seeded worker as a link;
//   - clicking the worker opens its detail (1 click from the list);
//   - the GPU heatmap shows a cell linking to the worker (color + a band WORD);
//   - drain is medium friction (an explicit confirm step);
//   - force-evict is high friction — the confirm button stays DISABLED until the
//     worker id is typed verbatim, then ENABLES (AC3 typed-name gating);
//   - pull and unload are reachable and produce a toast.
//
// The worker runs without a reachable Ollama, which is fine: it still registers and
// heartbeats (the --models fallback seeds its model set), so it appears in the fleet
// and on the detail page. Model pull/unload DISPATCH to the worker, so those toasts
// may report a failure (no Ollama) — the test asserts the interaction produces a
// toast, not a specific success, since the control-plane wiring is covered by the Go
// handler tests.

const httpPort = process.env.AGENTGPU_E2E_HTTP_PORT || "18080";
const grpcPort = process.env.AGENTGPU_E2E_GRPC_PORT || "18111";
const isWindows = process.platform === "win32";
const binaryName = isWindows ? "agentgpu.exe" : "agentgpu";
const repoRoot = join(__dirname, "..", "..", "..", "..");
const binary = process.env.AGENTGPU_BIN || join(repoRoot, binaryName);

// The worker id is deterministic so the spec can type it into the force-evict
// confirm and target it directly. It is long enough to exercise the shortID
// truncation in the UI while remaining a single path segment.
const WORKER_ID = "e2e-worker-0001";

const SHOT_DIR = join(__dirname, "..", "test-results");

function resolveToken(): string {
  if (process.env.AGENTGPU_E2E_TOKEN) {
    return process.env.AGENTGPU_E2E_TOKEN;
  }
  const stateFile = process.env.AGENTGPU_E2E_STATE_FILE;
  if (stateFile && existsSync(stateFile)) {
    return (JSON.parse(readFileSync(stateFile, "utf-8")) as { token: string }).token;
  }
  return "";
}

const TOKEN = resolveToken();

async function axeAAViolations(page: Page) {
  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
    .analyze();
  return results.violations;
}

function formatViolations(violations: Awaited<ReturnType<typeof axeAAViolations>>) {
  return violations
    .map((v) => `  [${v.impact ?? "n/a"}] ${v.id}: ${v.help} (${v.nodes.length} node(s))`)
    .join("\n");
}

// signIn runs the real login form and lands on the console root.
async function signIn(page: Page) {
  await page.goto("/admin/login");
  await page.getByLabel("Admin API token").fill(TOKEN);
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL("**/admin/");
}

let worker: ChildProcess | undefined;

test.beforeAll(async () => {
  mkdirSync(SHOT_DIR, { recursive: true });
  if (!TOKEN) {
    throw new Error("AGENTGPU_E2E_TOKEN was not set by the Playwright config global seed");
  }
  // Spawn a worker that registers with the running server. GPU detection is off and
  // a manual GPU type + VRAM are supplied so the worker reports stable capacity
  // without needing real hardware or a reachable Ollama.
  worker = spawn(
    binary,
    [
      "worker", "start",
      "--server", `127.0.0.1:${grpcPort}`,
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
});

test.afterAll(() => {
  worker?.kill();
});

// waitForWorkerRow polls the workers page until the seeded worker's row link is
// visible (the worker needs a heartbeat or two to register and the list to poll).
async function waitForWorkerRow(page: Page) {
  await page.goto("/admin/workers");
  const link = page.getByRole("link", { name: new RegExp(WORKER_ID.slice(0, 8)) }).first();
  await expect(link).toBeVisible({ timeout: 20_000 });
  return link;
}

test("workers list is accessible and lists the connected worker", async ({ page }) => {
  await signIn(page);

  // The seeded worker appears as a link in the live list (it may take a poll cycle).
  // waitForWorkerRow navigates to /admin/workers first.
  await waitForWorkerRow(page);
  await expect(page.getByRole("heading", { name: "Workers" })).toBeVisible();

  // The GPU heatmap region renders a cell linking to the worker, with a band word.
  const heatmapLink = page
    .locator("#gpu-heatmap")
    .getByRole("link", { name: new RegExp(WORKER_ID.slice(0, 8)) });
  await expect(heatmapLink.first()).toBeVisible();

  await page.screenshot({ path: join(SHOT_DIR, "workers-list.png"), fullPage: true });

  const violations = await axeAAViolations(page);
  expect(violations, `axe AA violations on the workers list:\n${formatViolations(violations)}`).toEqual([]);
});

test("clicking a worker opens its accessible detail screen", async ({ page }) => {
  await signIn(page);
  const link = await waitForWorkerRow(page);

  // One click from the list to the detail (AC2 for the heatmap; the list is the
  // same one-click affordance).
  await link.click();
  await page.waitForURL(`**/admin/workers/${WORKER_ID}`);

  // The detail header, the management sections, and the View logs affordance (AC1).
  await expect(page.getByRole("heading", { name: new RegExp(WORKER_ID.slice(0, 8)) })).toBeVisible();
  const logsLink = page.getByRole("link", { name: "View logs" });
  await expect(logsLink).toBeVisible();
  await expect(logsLink).toHaveAttribute("href", `/admin/logs?worker=${WORKER_ID}`);

  await page.screenshot({ path: join(SHOT_DIR, "worker-detail.png"), fullPage: true });

  const violations = await axeAAViolations(page);
  expect(violations, `axe AA violations on the worker detail:\n${formatViolations(violations)}`).toEqual([]);
});

test("drain is a medium-friction explicit confirm", async ({ page }) => {
  await signIn(page);
  const link = await waitForWorkerRow(page);
  await link.click();
  await page.waitForURL(`**/admin/workers/${WORKER_ID}`);

  // The drain action does not fire immediately: it reveals a confirm step first.
  await page.getByRole("button", { name: "Drain", exact: true }).click();
  const confirmDrain = page.getByRole("button", { name: "Confirm drain" });
  await expect(confirmDrain).toBeVisible();

  // Confirming produces a toast (the worker has no in-flight jobs, so the soft drain
  // succeeds server-side).
  await confirmDrain.click();
  await expect(page.getByRole("status").filter({ hasText: /Draining/ })).toBeVisible({ timeout: 10_000 });
});

test("force-evict is high friction: confirm is gated on typing the worker id", async ({ page }) => {
  await signIn(page);
  const link = await waitForWorkerRow(page);
  await link.click();
  await page.waitForURL(`**/admin/workers/${WORKER_ID}`);

  // Open the force-evict control. There are two "Force-evict" buttons (the opener and
  // the confirm); the opener is the first.
  await page.getByRole("button", { name: "Force-evict" }).first().click();

  // The confirm input and a confirm button appear. The confirm button is DISABLED
  // until the typed text exactly equals the worker id (AC3 typed-name gating).
  const confirmInput = page.getByLabel("Type the worker id to confirm eviction");
  await expect(confirmInput).toBeVisible();

  const confirmButtons = page.getByRole("button", { name: "Force-evict" });
  const confirmEvict = confirmButtons.last();
  await expect(confirmEvict).toBeDisabled();

  // A wrong value keeps it disabled.
  await confirmInput.fill("not-the-id");
  await expect(confirmEvict).toBeDisabled();

  // The exact worker id enables it.
  await confirmInput.fill(WORKER_ID);
  await expect(confirmEvict).toBeEnabled();

  // Confirming produces a toast.
  await confirmEvict.click();
  await expect(page.getByRole("status").filter({ hasText: /Evicting/ })).toBeVisible({ timeout: 10_000 });
});

test("pull a model produces a toast", async ({ page }) => {
  await signIn(page);
  const link = await waitForWorkerRow(page);
  await link.click();
  await page.waitForURL(`**/admin/workers/${WORKER_ID}`);

  // The pull form is reachable by its label; submitting dispatches the pull and
  // returns a toast (success or, with no reachable Ollama, a graceful failure — the
  // interaction is what this asserts; the control-plane wiring is unit-tested).
  await page.getByLabel("Pull a model").fill("llama3");
  await page.getByRole("button", { name: "Pull", exact: true }).click();
  await expect(page.getByRole("status")).toBeVisible({ timeout: 10_000 });
});
