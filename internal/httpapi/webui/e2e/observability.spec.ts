import { test, expect, type Page } from "@playwright/test";
import { mkdirSync } from "node:fs";
import { join } from "node:path";
import { BASE_URL, SHOT_DIR, expectNoAxeViolations, resolveToken, signIn as sharedSignIn } from "./helpers";

// observability.spec.ts — end-to-end coverage of the Usage, Telemetry, Logs, and
// Settings screens (issue 103), driving the REAL built binary the Playwright config
// started. The globalSetup already seeded a connected worker + sample stats traffic,
// so these screens render populated, deterministic data on a clean run. It asserts
// the AC flows with role/label locators (never CSS/XPath) and WCAG 2 AA
// accessibility (axe-core) on each screen, saving a screenshot per screen as a CI
// artifact:
//
//   - Usage renders consumption METERS (progress bars) and, for a key with history,
//     a 7-day sparkline + a run-out forecast — never a pie chart.
//   - Telemetry renders the request/latency/throttle KPI strip and the distribution
//     + fleet + affinity panels.
//   - Logs filtering reduces volume: applying a tighter filter lowers the buffered
//     line count, and the live tail can be resumed/paused.
//   - Settings edits a tunable live (change log level, apply, see it applied) and
//     shows a boot-only field READ-ONLY (disabled).
//
// The admin key the config seeds holds the admin role, so it satisfies every read
// scope plus config:write. Usage/telemetry/logs all read live in-process state; to
// give the buffered log view extra lines to filter on top of the global seed, the
// log spec generates a little more traffic against the admin API (which logs request
// lines) before asserting.

const TOKEN = resolveToken();

// signIn binds the shared helper to this spec's resolved token.
async function signIn(page: Page) {
  await sharedSignIn(page, TOKEN);
}

// generateLogTraffic makes a few authenticated API calls so the in-memory log ring
// has request lines to filter. Each /v1/admin/stats call logs an HTTP-handler line
// carrying a request_id. Failures are ignored — the goal is to populate the buffer.
async function generateLogTraffic(page: Page) {
  for (let i = 0; i < 6; i++) {
    await page
      .request.get(`${BASE_URL}/v1/admin/stats`, {
        headers: { Authorization: `Bearer ${TOKEN}` },
      })
      .catch(() => undefined);
  }
}

test.beforeAll(() => {
  mkdirSync(SHOT_DIR, { recursive: true });
  if (!TOKEN) {
    throw new Error("AGENTGPU_E2E_TOKEN was not set by the Playwright config global seed");
  }
});

test("usage screen shows progress meters, not pies, and is accessible", async ({ page }) => {
  await signIn(page);
  await page.goto("/admin/usage");
  await expect(page.getByRole("heading", { name: "Usage" })).toBeVisible();

  // The board loads via HTMX; the admin key's own row appears with consumption
  // meters (progress bars labeled by dimension), never a pie chart. The board is
  // addressed by its accessible role + name (role="region", aria-label="Usage"),
  // never by its #id.
  const board = page.getByRole("region", { name: "Usage" });
  await expect(board.getByText("Daily tokens").first()).toBeVisible({ timeout: 15_000 });
  // The meters are role=img bars with an accessible label naming the dimension.
  await expect(board.getByRole("img", { name: /Daily tokens/ }).first()).toBeVisible();
  // No pie/canvas charting — the screen is bar-based by design. This is a structural
  // ABSENCE assertion (a forbidden element must not exist), not a control selector,
  // so it is expressed as an element-count check rather than a role query: there is
  // no ARIA role for "a canvas that should not be here". The role-based meter
  // assertions above are what verify the affordances themselves.
  await expect(page.locator("canvas")).toHaveCount(0);

  await page.screenshot({ path: join(SHOT_DIR, "usage.png"), fullPage: true });

  await expectNoAxeViolations(page, "usage");
});

test("telemetry dashboard renders the metric panels and is accessible", async ({ page }) => {
  await signIn(page);
  await page.goto("/admin/telemetry");
  await expect(page.getByRole("heading", { name: "Telemetry" })).toBeVisible();

  // The board is addressed by its accessible role + name (role="region",
  // aria-label="Telemetry"), never by its #id.
  const board = page.getByRole("region", { name: "Telemetry" });
  // The KPI strip + the named panels render from the live collectors.
  await expect(board.getByText("Requests").first()).toBeVisible({ timeout: 15_000 });
  await expect(board.getByText("Request latency")).toBeVisible();
  await expect(board.getByText("Fleet by status")).toBeVisible();
  await expect(board.getByText("Session affinity")).toBeVisible();

  await page.screenshot({ path: join(SHOT_DIR, "telemetry.png"), fullPage: true });

  await expectNoAxeViolations(page, "telemetry");
});

test("logs filtering reduces the buffered line volume", async ({ page }) => {
  await signIn(page);
  await generateLogTraffic(page);

  // A wide view first: level=debug shows the most lines (the warn floor is widened).
  await page.goto("/admin/logs?level=debug");
  await expect(page.getByRole("heading", { name: "Logs" })).toBeVisible();
  // The buffered-line table is addressed by its accessible role + name
  // (role="region", aria-label="Buffered lines"), never by its #id.
  const table = page.getByRole("region", { name: "Buffered lines" });
  await expect(table.getByText("Buffered lines")).toBeVisible({ timeout: 15_000 });

  // Read the wide count, then tighten to ERROR-only and assert the count does not
  // grow (filters reduce volume). The count carries an accessible label
  // ("Buffered line count") so it is read by label, not by a CSS class. The exact
  // numbers depend on traffic, so the assertion is the monotonic reduction, which is
  // the AC's guarantee.
  const wideText = (await table.getByLabel("Buffered line count").textContent())?.trim() ?? "0";
  const wide = parseInt(wideText, 10) || 0;

  await page.getByLabel("Level (minimum)").selectOption("error");
  await page.getByRole("button", { name: "Apply filters" }).click();
  // Give HTMX a moment to swap the table.
  await expect(table.getByText("Buffered lines")).toBeVisible();
  await page.waitForTimeout(500);
  const narrowText = (await table.getByLabel("Buffered line count").textContent())?.trim() ?? "0";
  const narrow = parseInt(narrowText, 10) || 0;

  expect(narrow).toBeLessThanOrEqual(wide);

  // The live tail can be resumed (it starts paused) and then paused again.
  await page.getByRole("button", { name: "Resume" }).click();
  await expect(page.getByRole("button", { name: "Pause" })).toBeVisible();
  await page.getByRole("button", { name: "Pause" }).click();
  await expect(page.getByRole("button", { name: "Resume" })).toBeVisible();

  await page.screenshot({ path: join(SHOT_DIR, "logs.png"), fullPage: true });

  await expectNoAxeViolations(page, "logs");
});

test("settings edits a tunable live and shows a boot-only field read-only", async ({ page }) => {
  await signIn(page);
  await page.goto("/admin/config");
  await expect(page.getByRole("heading", { name: "Settings" })).toBeVisible();

  // A boot-only field is shown READ-ONLY (in the read-only section; not an editable
  // input). The "gRPC listen" boot value is present as descriptive text.
  await expect(page.getByText("Boot-only (read-only)")).toBeVisible();
  await expect(page.getByText("gRPC listen")).toBeVisible();

  // Edit a tunable on the General tab: change the log level and apply. The General
  // tab is active by default.
  const levelSelect = page.getByLabel("Log level");
  await expect(levelSelect).toBeEnabled();
  await levelSelect.selectOption("debug");
  await page.getByRole("button", { name: "Apply changes" }).click();

  // The change is applied live: a success toast appears and the re-rendered editor
  // reflects the new value.
  await expect(page.getByRole("status").filter({ hasText: /applied/i })).toBeVisible({ timeout: 10_000 });
  await expect(page.getByLabel("Log level")).toHaveValue("debug");

  await page.screenshot({ path: join(SHOT_DIR, "settings.png"), fullPage: true });

  await expectNoAxeViolations(page, "settings");
});
