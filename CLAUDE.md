# CLAUDE.md

Guidance for Claude Code when working in this repository.

## Project

**agent-gpu** is a distributed inference layer for Ollama: a central **server** owns the public
OpenAI-compatible API, auth, quotas, permissions, and scheduling; **workers** run Ollama and execute
dispatched jobs. See [README.md](README.md), [docs/architecture.md](docs/architecture.md), and
[CONTRIBUTING.md](CONTRIBUTING.md). The implementation language/runtime is decided in issue #1.

Work is tracked as GitHub Issues on the **agent-gpu roadmap** Projects v2 board
(`jaypetez`, project #10) with Status `Backlog → Ready → In Progress → In Review → Done`.

## Conventions

- Branch names: `feature/<desc>`, `fix/<desc>`, `chore/<desc>`. Never commit to `main`.
- PR titles follow Conventional Commits (`feat:`, `fix:`, `chore:`, `ci:`, `docs:`, `test:`, …) —
  enforced by the PR-title check. Reference the issue (`Closes #N`).
- Every change ships with tests; CI must be green before merge.

## Autonomous burndown workflow

This repo is set up to be driven to completion mostly autonomously with the built-in **`/goal`**
command. `/goal` only stores a completion *condition* and re-runs each turn until it's met — the
per-issue recipe below is what each turn should follow. Kick it off with, e.g.:

```text
/goal every open issue on the agent-gpu roadmap (project #10) has a PR opened and the Ready and Backlog columns are empty
```

Each turn, work **one** issue through these steps:

1. **Pick** — run the `up-next` skill. It selects the next actionable issue (Ready, unblocked,
   highest priority) and moves its card to **In Progress**. If `up-next` reports nothing actionable,
   the burndown is complete — stop.
2. **Audit** — run the `audit-issue` skill on that issue. It reports what's already shipped,
   researches best practices, and **surfaces genuine questions via `AskUserQuestion`**. When it
   asks, the loop pauses for the user — answer before continuing. Do **not** blindly implement.
3. **Implement** — spawn a **fresh `implementer` subagent** (Opus 4.8 [1m], `effort: max`) via the
   Agent tool, passing the audit brief + issue. It writes code and tests on a `feature/…` branch and
   commits. It does **not** open the PR.
4. **Review** — spawn a **fresh `code-reviewer` subagent** (Opus 4.8 [1m], `effort: max`) on the
   branch diff. It reviews cold and returns blocking vs non-blocking findings.
5. **Fix loop** — if there are blocking findings, re-spawn the `implementer` to address them, then
   re-spawn the `code-reviewer`. Repeat until the verdict is **APPROVE**.
6. **Ship** — the **main loop** (not a subagent) pushes the branch, opens the PR (Conventional
   Commit title, `Closes #N`), and moves the card **In Progress → In Review**.
7. **Next** — continue to the next issue.

Rules:

- Implementation and code review **always** run in fresh subagents (the `implementer` and
  `code-reviewer` agent types) so each gets clean, isolated context. The reviewer is read-only.
- All git/board **side effects that go outward** (push, PR, board moves to In Review/Done) stay in
  the main loop, never inside a subagent.
- Pause for the user on any genuine question; otherwise keep the loop moving.
