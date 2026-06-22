# Admin console — anti-slop review checklist

This is the committed design-quality checklist for the agent-gpu admin console
(issue #100). It is the bar a reviewer (human or cold AI) checks the console
against, and it is filled in for the three screens the foundation ships: the
**login** page, the **shell** (topbar + role-gated sidebar + breadcrumbs), and the
**dashboard** (Overview). Later GUI issues (#101–#103) extend the table with their
screens.

Status key: `[x]` met · `[~]` partially/deferred (with note) · `[ ]` not met.

## How this was verified

- `go test ./internal/httpapi/webui/...` — token-lint (no raw hex / no Tailwind
  `[...]` in `.templ`), AA-contrast computation over the committed tokens.
- `go test ./internal/httpapi/...` — login, cookie auth, CSRF, logout, role-based
  sidebar gating, `tokenFromRequest` precedence, route-sync stays at 31.
- `internal/httpapi/webui/` Playwright + `@axe-core/playwright` — login → dashboard →
  logout against the real binary, with **WCAG AA axe assertions** on login + shell +
  dashboard (the CI `e2e` job fails on any violation). A dashboard screenshot is
  archived as an artifact.

## Design system & tokens

- [x] **Tokens are the sole source of styling.** Every color/size/spacing value
  comes from a token-derived utility class defined in
  `internal/httpapi/webui/assets/css/input.css`. A committed test
  (`token_lint_test.go`) fails the build on a raw hex or an arbitrary Tailwind
  `[...]` value in any `.templ` file.
- [x] **Spacing on a 4/8/12 grid.** `--spacing: 0.25rem` (4px) generates the scale;
  every `p-*`/`gap-*`/`m-*` lands on the grid by construction.
- [x] **≤6 type sizes.** Exactly six: `--text-xs … --text-2xl` (12/13/14/16/20/28).
- [x] **Single UI typeface + a data/mono utility face.** One system-sans stack for
  the shell/prose; a monospace utility face for data, IDs, metrics, and code (the
  native font of an ops console). Tables and inline metrics use tabular figures.
- [x] **Monochrome base + exactly ONE accent.** A blue-slate monochrome ground/
  surfaces with a single brand accent (signal teal `--color-accent`). Status tones
  (ok/warn/danger/info/idle) are a SEPARATE semantic set, not a second brand color.
- [x] **Dark-mode-first.** `color-scheme: dark`; the palette is designed for the
  dark ground (no light-mode afterthought).
- [x] **≥4.5:1 contrast (AA).** `token_contrast_test.go` computes WCAG contrast for
  every text-on-surface pairing and fails below 4.5:1; axe confirms AA in-browser.

## Interactive primitives — six microstates each

For button, input, select, modal, table row, and toast: default / hover / focus /
active / disabled / loading.

- [x] **Button** (`.btn`, `.btn-primary`, `.btn-ghost`, `.btn-danger`): hover lift,
  `:active` translate, `disabled`/`aria-disabled` dim + no pointer events, and a
  `[data-loading]` / `.htmx-request` spinner that hides the label.
- [x] **Input / Select** (`.field`): hover border, focus ring, `disabled` dim,
  `aria-invalid` danger state, and a `[data-loading]` shimmer track.
- [x] **Table row** (`.tbl tbody tr`): hover and `focus-within` background; rows are
  focusable (`tabindex="0"`) so keyboard users get the same affordance.
- [x] **Toast** (`.toast` + tone variants): enter animation, per-tone left border,
  rendered into an `aria-live="polite"` region.
- [x] **Modal** (`.modal` + `.modal-backdrop`): backdrop fade + panel enter.
- [x] **Visible 2px focus ring everywhere.** `:focus-visible` → 2px accent outline
  with offset; fields use an equivalent 2px ring that hugs the control.

## Motion

- [x] **≤200ms.** All transitions/animations use `--dur-fast` (120ms) or
  `--dur-base` (180ms).
- [x] **`prefers-reduced-motion` honored.** A global media query collapses all
  animation/transition to ~0 for users who asked.
- [x] **One orchestrated moment, not scattered effects.** The live-flow pulse on the
  "LIVE" indicator is the single deliberate motion; everything else is static.

## State coverage (no dead ends)

- [x] **Loading.** The dashboard renders a skeleton board on first paint
  (`OverviewLoading`); the partial swaps in real data after the telemetry pull.
- [x] **Empty / idle.** Queue-empty, no-workers, and quiet-event-stream states are
  calm, in-voice invitations to act (not three zero-bars or a blank panel).
- [x] **Error.** The board has an in-voice error partial with a retry; the console
  has a standalone HTML error page (`ErrorPage`) for bad URLs/500s.

## Dashboard (task-centric)

- [x] **≤8 panels.** Six: a 3-card KPI row + the three named panels.
- [x] **The three required panels.** Queue-depth, worker-health, and event-stream,
  read from the existing `/v1/admin` telemetry/stats and the log ring.
- [x] **Task-centric.** Leads with the operator's standing questions: is the queue
  backing up, are the workers healthy, is anything erroring.
- [x] **Status by color AND text.** Every KPI shows a status word
  (`ok`/`watch`/`alert`/`idle`) beside its toned value; worker status is a
  text-labeled badge (the colored dot is decorative). Readable in grayscale.

## Information architecture

- [x] **Role-based sidebar.** Sections appear only when the viewer holds the
  matching admin read-scope (`authz.HasScope`); an admin sees all, a scoped key a
  strict subset. Verified by `TestUISidebarRoleGating`.
- [x] **Breadcrumbs.** Rendered under the topbar; the last crumb is the current
  page, earlier crumbs link up.
- [x] **Active state.** The current section carries `aria-current="page"` and the
  accent treatment.

## Copy

- [x] **Operator's voice, specific over clever.** "Sign in to the console", "Paste
  an admin API token to manage the fleet", "No workers connected — Start a worker
  with `agentgpu worker start` and it will register here."
- [x] **Actions say what they do, consistently.** "Sign in", "Sign out", "Refresh",
  "Try again". No "Submit".
- [x] **Errors explain and direct.** "That token isn't valid. Check it and try
  again." / "That token has no admin access. Ask for a key with an admin role or
  scope." No apologies, no vague mood.

## Accessibility (AA)

- [x] **`<html lang>` set, landmarks present.** `lang="en"`; `<nav aria-label>`,
  `<main>`, `<header>`, a skip-to-content link.
- [x] **Labels & relationships.** The token field has a real `<label for>` and an
  `aria-describedby` hint; the error alert uses `role="alert"`.
- [x] **axe AA passes.** `@axe-core/playwright` reports zero WCAG A/AA violations on
  login, shell, and dashboard (CI `e2e` job is the gate).

## "Does it look templated?" critique

- [x] **Not the default dark-emerald dashboard.** The ground is a deliberate cooler
  blue-slate (`#0b0f14`), and the accent reads as a *signal/phosphor* teal meaning
  "live data flowing" — chosen from the subject's world (telemetry through a fleet),
  not a stock success-green.
- [x] **Structure encodes meaning, not decoration.** No gratuitous 01/02/03
  numbering; the instrument-panel framing (mono data, status language, the live
  pulse) is specific to a control plane.
- [x] **Boldness spent in one place.** The mono-rooted data type + the single live
  pulse are the memorable moves; everything around them is quiet and disciplined.
