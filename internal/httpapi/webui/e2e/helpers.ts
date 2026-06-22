import { expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

// helpers.ts — the single shared module for the admin console E2E (issue #105).
// Before #105 every spec re-declared resolveToken / axeAAViolations /
// formatViolations / signIn / uniqueName; this centralises them so there is one
// source of truth (and one place to fix). The four specs and the global setup all
// import from here.
//
// Locators in the specs are role/label based on purpose (getByRole / getByLabel),
// never CSS or XPath — they assert the accessible structure, not the markup, so a
// refactor that keeps the UX keeps the tests green.

// The HTTP/gRPC ports the Playwright config starts the server on. Kept in sync with
// playwright.config.ts via the same env vars (defaults match the config defaults).
export const HTTP_PORT = process.env.AGENTGPU_E2E_HTTP_PORT || "18080";
export const GRPC_PORT = process.env.AGENTGPU_E2E_GRPC_PORT || "18111";
export const BASE_URL = process.env.AGENTGPU_E2E_BASE_URL || `http://127.0.0.1:${HTTP_PORT}`;

const isWindows = process.platform === "win32";
const binaryName = isWindows ? "agentgpu.exe" : "agentgpu";

// repoRoot from e2e/ is four levels up (internal/httpapi/webui/e2e -> repo root).
const repoRoot = join(__dirname, "..", "..", "..", "..");

// binaryPath resolves the built agentgpu binary: `make ui-e2e` / CI passes
// AGENTGPU_BIN, with the repo-root fallback covering a bare `npx playwright test`.
export const binaryPath = process.env.AGENTGPU_BIN || join(repoRoot, binaryName);

// SHOT_DIR is where each spec drops its CI screenshot artifact (and where the JSON
// reporter writes results.json). It mirrors the Playwright outputDir.
export const SHOT_DIR = join(__dirname, "..", "test-results");

// resolveToken reads the seeded admin token. The config seeds it once and exposes
// it via AGENTGPU_E2E_TOKEN, but since Playwright runs specs in a separate worker
// process it falls back to the shared state file the config wrote (the same one the
// webServer's store came from) so the token always matches the running server.
export function resolveToken(): string {
  if (process.env.AGENTGPU_E2E_TOKEN) {
    return process.env.AGENTGPU_E2E_TOKEN;
  }
  const stateFile = process.env.AGENTGPU_E2E_STATE_FILE;
  if (stateFile && existsSync(stateFile)) {
    return (JSON.parse(readFileSync(stateFile, "utf-8")) as { token: string }).token;
  }
  return "";
}

// axeAAViolations runs an axe scan constrained to the WCAG 2.0/2.1 A and AA rule
// tags and returns the violations. Scoping to the standard AA tags keeps the gate to
// the criteria the AC names (rather than best-practice noise).
export async function axeAAViolations(page: Page) {
  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
    .analyze();
  return results.violations;
}

// formatViolations renders axe violations compactly for a useful failure message,
// so a failing assertion is legible enough to fix in one loop iteration (AC5).
export function formatViolations(violations: Awaited<ReturnType<typeof axeAAViolations>>) {
  return violations
    .map((v) => `  [${v.impact ?? "n/a"}] ${v.id}: ${v.help} (${v.nodes.length} node(s))`)
    .join("\n");
}

// expectNoAxeViolations scans the current page and fails with a legible, per-rule
// message when any WCAG AA violation is present. It is the shared form of the
// per-spec accessibility gate.
export async function expectNoAxeViolations(page: Page, where: string) {
  const violations = await axeAAViolations(page);
  expect(violations, `axe AA violations on ${where}:\n${formatViolations(violations)}`).toEqual([]);
}

// signIn runs the real login form with the seeded admin token and lands on the
// console root. Every authenticated spec starts here.
export async function signIn(page: Page, token: string) {
  await page.goto("/admin/login");
  await page.getByLabel("Admin API token").fill(token);
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL("**/admin/");
}

// uniqueName returns a per-test unique name so each mutating test's row/record is
// unambiguous and tests never collide on the shared store. This is the per-spec half
// of the isolation model documented in playwright.config.ts: a single deterministic
// global seed is established once, mutating specs use unique names and clean up after
// themselves, and read-only specs assert against the seeded baseline.
export function uniqueName(prefix: string): string {
  return `${prefix}-${Date.now()}-${Math.floor(Math.random() * 1e6)}`;
}
