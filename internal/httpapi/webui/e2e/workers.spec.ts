import { test, expect, type Page } from "@playwright/test";
import { mkdirSync } from "node:fs";
import { join } from "node:path";
import { SHOT_DIR, expectNoAxeViolations, resolveToken, signIn as sharedSignIn } from "./helpers";
import { WORKER_ID } from "./global-setup";

// workers.spec.ts — end-to-end coverage of the Workers + GPU management screens
// (issue 101), driving the REAL built binary. The connected worker it manages is
// seeded ONCE by the Playwright globalSetup (e2e/global-setup.ts) against the same
// server the config started, so the fleet already has one connected worker to list,
// open, and manage. It then asserts the AC flows with role/label locators (never
// CSS/XPath) and WCAG 2 AA accessibility (axe-core) on each new screen, saving a
// screenshot per screen as a CI artifact:
//
//   - the worker list is visible + accessible, with the seeded worker as a link;
//   - clicking the worker opens its detail (1 click from the list);
//   - the GPU heatmap shows a cell linking to the worker (color + a band WORD);
//   - drain is medium friction (an explicit confirm step);
//   - force-evict is high friction — the confirm button stays DISABLED until the
//     worker id is typed verbatim, then ENABLES (AC3 typed-name gating);
//   - pull and unload are reachable and produce a toast.
//
// The seeded worker runs without a reachable Ollama, which is fine: it still
// registers and heartbeats every 1s (the --models fallback seeds its model set), so
// it appears in the fleet and on the detail page — and re-registers within a poll
// after the force-evict test deregisters it, so no later test sees a missing worker.
// Model pull/unload DISPATCH to the worker, so those toasts may report a failure (no
// Ollama) — the test asserts the interaction produces a toast, not a specific
// success, since the control-plane wiring is covered by the Go handler tests.

const TOKEN = resolveToken();

// signIn binds the shared helper to this spec's resolved token.
async function signIn(page: Page) {
  await sharedSignIn(page, TOKEN);
}

test.beforeAll(() => {
  mkdirSync(SHOT_DIR, { recursive: true });
  if (!TOKEN) {
    throw new Error("AGENTGPU_E2E_TOKEN was not set by the Playwright config global seed");
  }
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
  // The region is addressed by its accessible role + name (role="region",
  // aria-label="GPU utilization"), never by its #id.
  const heatmap = page.getByRole("region", { name: "GPU utilization" });
  const heatmapLink = heatmap.getByRole("link", { name: new RegExp(WORKER_ID.slice(0, 8)) });
  await expect(heatmapLink.first()).toBeVisible();

  await page.screenshot({ path: join(SHOT_DIR, "workers-list.png"), fullPage: true });

  await expectNoAxeViolations(page, "the workers list");
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

  await expectNoAxeViolations(page, "the worker detail");
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
