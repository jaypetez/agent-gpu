import { test, expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { existsSync, mkdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

// keys.spec.ts — end-to-end coverage of the API keys, users, and permissions
// screens (issue 102), driving the REAL built binary. Keys live in the in-process
// store the seeded admin key was minted into, so — unlike the workers spec — no live
// worker is needed: the spec creates throwaway keys through the UI and manages those,
// leaving the seeded admin key (which the rest of the run authenticates with)
// untouched. It asserts the AC flows with role/label locators (never CSS/XPath) and
// WCAG 2 AA accessibility (axe-core) on the screen and the open create form, saving a
// screenshot per flow as a CI artifact:
//
//   - the keys table is visible + accessible, showing a MASK and never a token;
//   - create reveals the new token EXACTLY ONCE with a copy affordance;
//   - rotate reveals a NEW token once (and warns the old one stops working);
//   - revoke is HIGH friction — the confirm button stays DISABLED until the key id is
//     typed verbatim, then ENABLES (AC1 typed-name gating);
//   - the permissions editor picks roles/scopes from the catalog and saves (AC2).

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

// uniqueName returns a per-test unique key name so each test's row is unambiguous and
// tests never collide on the shared store.
function uniqueName(prefix: string): string {
  return `${prefix}-${Date.now()}-${Math.floor(Math.random() * 1e6)}`;
}

// createKey drives the create modal end-to-end and returns the revealed one-time
// token (agpu_<id>_<secret>). It asserts the token is shown with a copy control, then
// closes the reveal. The created key persists in the store as a new table row.
async function createKey(page: Page, name: string): Promise<string> {
  await page.goto("/admin/keys");
  await page.getByRole("button", { name: "New key" }).click();

  // The create form is a labelled dialog; fill the required name and pick a role.
  await page.getByLabel(/^Name/).fill(name);
  await page.getByRole("checkbox", { name: /^user/ }).check();

  await page.getByRole("button", { name: "Create key" }).click();

  // The one-time reveal shows the token and a copy affordance (AC1).
  const reveal = page.getByRole("dialog", { name: "Key created" });
  await expect(reveal).toBeVisible({ timeout: 10_000 });
  await expect(page.getByRole("button", { name: "Copy token to clipboard" })).toBeVisible();
  const token = (await reveal.getByText(/^agpu_/).innerText()).trim();
  expect(token).toMatch(/^agpu_[a-f0-9]+_[a-f0-9]+$/);

  // Dismiss the reveal so the table is interactable again.
  await reveal.getByRole("button", { name: "Done" }).click();
  await expect(reveal).toBeHidden();
  return token;
}

// rowFor returns the table row for a key by its (unique) name.
function rowFor(page: Page, name: string) {
  return page.getByRole("row").filter({ hasText: name });
}

test.beforeAll(() => {
  mkdirSync(SHOT_DIR, { recursive: true });
  if (!TOKEN) {
    throw new Error("AGENTGPU_E2E_TOKEN was not set by the Playwright config global seed");
  }
});

test("keys screen is accessible and shows a mask, never a token", async ({ page }) => {
  await signIn(page);
  await page.goto("/admin/keys");

  await expect(page.getByRole("heading", { name: "API keys" })).toBeVisible();
  // The seeded admin key's row is present; the secret column shows a mask.
  await expect(page.getByRole("cell", { name: /agpu_•+/ }).first()).toBeVisible({ timeout: 10_000 });
  // No real token (a full agpu_<id>_<secret>) is rendered in the table.
  await expect(page.getByText(/agpu_[a-f0-9]+_[a-f0-9]+/)).toHaveCount(0);

  await page.screenshot({ path: join(SHOT_DIR, "keys-list.png"), fullPage: true });

  const violations = await axeAAViolations(page);
  expect(violations, `axe AA violations on the keys screen:\n${formatViolations(violations)}`).toEqual([]);
});

test("create reveals the new token exactly once with a copy affordance", async ({ page }) => {
  await signIn(page);

  // The open create form must itself be accessible (AA on the form, AC3).
  await page.goto("/admin/keys");
  await page.getByRole("button", { name: "New key" }).click();
  await expect(page.getByRole("dialog", { name: "New API key" })).toBeVisible();
  const formViolations = await axeAAViolations(page);
  expect(formViolations, `axe AA violations on the create form:\n${formatViolations(formViolations)}`).toEqual([]);
  // Close it and run the full create via the helper.
  await page.getByRole("button", { name: "Cancel" }).click();

  const name = uniqueName("e2e-create");
  const token = await createKey(page, name);
  expect(token).toMatch(/^agpu_/);

  // The new key now appears as a masked row; the token is NOT in the table.
  await expect(rowFor(page, name)).toBeVisible({ timeout: 10_000 });
  await expect(page.getByText(token)).toHaveCount(0);

  await page.screenshot({ path: join(SHOT_DIR, "keys-reveal.png"), fullPage: true });
});

test("rotate reveals a new token once", async ({ page }) => {
  await signIn(page);
  const name = uniqueName("e2e-rotate");
  const original = await createKey(page, name);

  // Open the row's rotate confirm and rotate (scope to THIS row's dialog).
  const row = rowFor(page, name);
  await row.getByRole("button", { name: new RegExp(`Rotate ${name}`) }).click();
  const rotateDialog = row.getByRole("dialog", { name: "Rotate key" });
  await expect(rotateDialog).toBeVisible();
  await rotateDialog.getByRole("button", { name: "Rotate and reveal new token" }).click();

  // A NEW one-time token is revealed (different from the original).
  const reveal = page.getByRole("dialog", { name: "Key rotated" });
  await expect(reveal).toBeVisible({ timeout: 10_000 });
  const rotated = (await reveal.getByText(/^agpu_/).innerText()).trim();
  expect(rotated).toMatch(/^agpu_[a-f0-9]+_[a-f0-9]+$/);
  expect(rotated).not.toEqual(original);
  await reveal.getByRole("button", { name: "Done" }).click();
});

test("revoke is high friction: confirm is gated on typing the key id", async ({ page }) => {
  await signIn(page);
  const name = uniqueName("e2e-revoke");
  const token = await createKey(page, name);
  // The key id is the middle segment of agpu_<id>_<secret>.
  const keyId = token.split("_")[1];

  const row = rowFor(page, name);
  await row.getByRole("button", { name: new RegExp(`Revoke ${name}`) }).click();
  // Scope to THIS row's revoke dialog (every row carries one; only the opened one is
  // visible), so the typed-name gate is asserted on the right key.
  const revokeDialog = row.getByRole("dialog", { name: "Revoke key" });
  await expect(revokeDialog).toBeVisible();

  // The confirm button is DISABLED until the typed text exactly equals the key id.
  const confirmInput = revokeDialog.getByLabel("Type the key id to confirm revocation");
  const confirmBtn = revokeDialog.getByRole("button", { name: "Revoke key" });
  await expect(confirmBtn).toBeDisabled();

  await confirmInput.fill("not-the-id");
  await expect(confirmBtn).toBeDisabled();

  await confirmInput.fill(keyId);
  await expect(confirmBtn).toBeEnabled();

  await confirmBtn.click();
  await expect(page.getByRole("status").filter({ hasText: /revoked/i })).toBeVisible({ timeout: 10_000 });
});

test("permissions editor picks roles and scopes from the catalog and saves", async ({ page }) => {
  await signIn(page);
  const name = uniqueName("e2e-perms");
  await createKey(page, name);

  const row = rowFor(page, name);
  await row.getByRole("button", { name: new RegExp(`Edit permissions for ${name}`) }).click();
  // Scope to THIS row's permissions dialog (every row carries one).
  const permsDialog = row.getByRole("dialog", { name: "Edit permissions" });
  await expect(permsDialog).toBeVisible();

  // The pickers are populated from the catalog: a known role and a known scope.
  await permsDialog.getByRole("checkbox", { name: /^read-only/ }).check();
  await permsDialog.getByRole("checkbox", { name: "workers:read" }).check();

  await permsDialog.getByRole("button", { name: "Save permissions" }).click();
  await expect(page.getByRole("status").filter({ hasText: /Permissions updated/ })).toBeVisible({ timeout: 10_000 });
});
