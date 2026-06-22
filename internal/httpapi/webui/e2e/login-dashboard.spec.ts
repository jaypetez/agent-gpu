import { test, expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { existsSync, mkdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

// login-dashboard.spec.ts — the admin console's first end-to-end test (issue #100).
// It drives the REAL built binary: signs in with a seeded admin token, lands on the
// dashboard, and asserts WCAG 2 AA accessibility (via @axe-core/playwright) on the
// login, shell, and dashboard screens. A build/CI failure on any axe violation is
// the AC6 gate. A dashboard screenshot is saved as a CI artifact.
//
// Locators are role/label based on purpose (getByRole / getByLabel), never CSS or
// XPath — they assert the accessible structure, not the markup, so a refactor that
// keeps the UX keeps the test green.

// resolveToken reads the seeded admin token. The config seeds it once and exposes
// it via AGENTGPU_E2E_TOKEN, but since Playwright runs the spec in a separate worker
// process, it falls back to the shared state file the config wrote (the same one the
// webServer's store came from) so the token always matches the running server.
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
const SHOT_DIR = join(__dirname, "..", "test-results");

// runAxeAA runs an axe scan constrained to the WCAG 2.0/2.1 A and AA rule tags and
// returns the violations. Scoping to the standard AA tags keeps the gate to the
// criteria AC6 names (rather than best-practice noise).
async function axeAAViolations(page: Page) {
  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
    .analyze();
  return results.violations;
}

// formatViolations renders axe violations compactly for a useful failure message.
function formatViolations(violations: Awaited<ReturnType<typeof axeAAViolations>>) {
  return violations
    .map((v) => `  [${v.impact ?? "n/a"}] ${v.id}: ${v.help} (${v.nodes.length} node(s))`)
    .join("\n");
}

test.beforeAll(() => {
  mkdirSync(SHOT_DIR, { recursive: true });
  if (!TOKEN) {
    throw new Error("AGENTGPU_E2E_TOKEN was not set by the Playwright config global seed");
  }
});

test("login page is accessible (WCAG AA)", async ({ page }) => {
  await page.goto("/admin/login");
  await expect(page.getByRole("heading", { name: "Sign in to the console" })).toBeVisible();
  await expect(page.getByLabel("Admin API token")).toBeVisible();

  const violations = await axeAAViolations(page);
  expect(violations, `axe AA violations on the login page:\n${formatViolations(violations)}`).toEqual([]);
});

test("operator signs in and reaches an accessible dashboard", async ({ page }) => {
  // Sign in through the real form.
  await page.goto("/admin/login");
  await page.getByLabel("Admin API token").fill(TOKEN);
  await page.getByRole("button", { name: "Sign in" }).click();

  // Lands on the console root with the dashboard rendered.
  await page.waitForURL("**/admin/");
  await expect(page.getByRole("heading", { name: "Fleet overview" })).toBeVisible();

  // The role-gated sidebar shows the admin's sections (an admin key sees all).
  const nav = page.getByRole("navigation", { name: "Console sections" });
  await expect(nav.getByRole("link", { name: "Overview" })).toBeVisible();
  await expect(nav.getByRole("link", { name: "Workers" })).toBeVisible();
  await expect(nav.getByRole("link", { name: "API keys" })).toBeVisible();

  // The three named dashboard panels load (HTMX swaps them in after first paint).
  await expect(page.getByText("Queue depth", { exact: false }).first()).toBeVisible();
  await expect(page.getByText("Worker health")).toBeVisible();
  await expect(page.getByText("Event stream")).toBeVisible();

  // Wait for the polled overview region to load its real content (KPI word tags),
  // so axe scans the loaded dashboard, not the skeleton.
  await expect(page.getByText("Workers online")).toBeVisible();

  // Save the dashboard screenshot as a CI artifact.
  await page.screenshot({ path: join(SHOT_DIR, "dashboard.png"), fullPage: true });

  // Accessibility gate on the full authenticated shell + dashboard.
  const violations = await axeAAViolations(page);
  expect(violations, `axe AA violations on the dashboard:\n${formatViolations(violations)}`).toEqual([]);
});

test("sign out returns to the login page", async ({ page }) => {
  await page.goto("/admin/login");
  await page.getByLabel("Admin API token").fill(TOKEN);
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL("**/admin/");

  await page.getByRole("button", { name: "Sign out" }).click();
  await page.waitForURL("**/admin/login**");
  await expect(page.getByRole("heading", { name: "Sign in to the console" })).toBeVisible();
});
