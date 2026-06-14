---
name: audit-issue
description: Audit one agent-gpu issue before implementing it — check what the codebase already ships against its acceptance criteria, research current best practices / dos-and-don'ts on the web, and surface genuine open questions to the user. Use after up-next picks an issue, or whenever the user says "audit issue N", "research this issue", or "what should I know before building N". Produces an implementation brief for the implementer subagent.
---

# audit-issue

Given an issue number, produce a tight **implementation brief** so the `implementer` subagent builds
the right thing once, the right way. Runs in the **main context** (not a fork) so that
`AskUserQuestion` reaches the user and pauses an autonomous `/goal` loop when a real decision is
needed.

Argument: the issue number (e.g. from `up-next`). If none is given, ask which issue.

## Step 1 — read the issue

```bash
gh issue view <N> --repo jaypetez/agent-gpu --json number,title,labels,milestone,body
```

Parse the standard sections: **Description**, **Scope / Tasks**, **Acceptance Criteria**,
**Out of scope**. Treat acceptance criteria as the definition of done.

## Step 2 — what's already shipped (avoid redoing work)

Spawn one or more `Explore` subagents to search the repo for existing code, config, docs, or tests
that already satisfy any acceptance criterion or task. For each acceptance criterion, classify it as
**done / partial / missing**, citing the files found. Note anything adjacent the implementer should
reuse rather than reinvent (existing utilities, patterns, modules).

> The repo may still be pre-code (language decided in #1). If so, say so and focus the brief on the
> decisions and scaffolding the issue establishes.

## Step 3 — best-practice research

Use `WebSearch` / `WebFetch` to gather current, authoritative guidance for this issue's topic
(e.g. API-key hashing, rate limiting, GPU scheduling, OpenAI-compatible APIs, gRPC vs HTTP). Capture:

- **Recommended approach(es)** and why.
- **Dos and don'ts / common pitfalls** specific to this problem.
- **Security considerations** (this project brokers inference access — be careful here).
- Prefer official docs and well-regarded sources; **cite every source** as a markdown link.

## Step 4 — surface genuine questions (only when needed)

Use `AskUserQuestion` **only** when a real decision blocks correct implementation — e.g.:

- A foundational choice the issue defers (the language/runtime/transport in #1).
- Ambiguous or conflicting requirements.
- A design fork with materially different trade-offs and no obvious default.

Do **not** ask about things with a sensible default, anything answered by the issue body, or routine
implementation detail — just proceed. Fold any answers into the brief.

## Step 5 — emit the implementation brief

Output a structured brief (this is the input the `implementer` subagent receives):

```text
## Audit brief — #<N> <title>

### Current state
- <criterion> — done/partial/missing — <file refs>

### Recommended approach
<approach, with the key decisions resolved>

### Dos / don'ts & pitfalls
- DO ... / DON'T ...   (each with a cited source where relevant)

### Acceptance-criteria checklist
- [ ] <criterion 1>
- [ ] <criterion 2>

### Implementation outline
1. <step> (reuse <existing thing> at <path>)
2. ...

### Open decisions resolved with the user
- <question> → <answer>   (omit if none)

### Sources
- [title](url)
```

Stop here — `audit-issue` does not write code. The burndown workflow (CLAUDE.md) hands this brief to
the `implementer` subagent.
