import { test, expect } from "@playwright/test";
import { mkdirSync } from "node:fs";
import { join } from "node:path";
import { SHOT_DIR, expectNoAxeViolations, resolveToken } from "./helpers";

// login-dashboard.spec.ts — the admin console's first end-to-end test (issue #100).
// It drives the REAL built binary: signs in with a seeded admin token, lands on the
// dashboard, and asserts WCAG 2 AA accessibility (via @axe-core/playwright) on the
// login, shell, and dashboard screens. A build/CI failure on any axe violation is
// the AC6 gate. A dashboard screenshot is saved as a CI artifact.
//
// Locators are role/label based on purpose (getByRole / getByLabel), never CSS or
// XPath — they assert the accessible structure, not the markup, so a refactor that
// keeps the UX keeps the test green. The shared helpers (resolveToken, axe,
// formatViolations, …) live in ./helpers so all four specs share one source.

const TOKEN = resolveToken();

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

  await expectNoAxeViolations(page, "the login page");
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
  // so axe scans the loaded dashboard, not the skeleton. Match the KPI panel title
  // exactly: with the global seed's worker online, the "All workers online" health
  // caption also contains "Workers online", so a substring match is ambiguous.
  await expect(page.getByText("Workers online", { exact: true })).toBeVisible();

  // Save the dashboard screenshot as a CI artifact.
  await page.screenshot({ path: join(SHOT_DIR, "dashboard.png"), fullPage: true });

  // Accessibility gate on the full authenticated shell + dashboard.
  await expectNoAxeViolations(page, "the dashboard");
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
