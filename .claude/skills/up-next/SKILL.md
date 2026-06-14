---
name: up-next
description: Pick the next actionable issue off the agent-gpu roadmap board (Projects v2 #10) and move it to In Progress. Use at the start of each autonomous burndown turn, or whenever the user asks "what's next", "pick the next issue", or "up next". Reports the chosen issue (number, title, body) and why, or reports clearly when nothing is actionable.
---

# up-next

Select the single next issue to work on from the **agent-gpu roadmap** Projects v2 board, move its
card to **In Progress**, and report it. Runs in the main context (no fork) so the choice is visible
to the rest of the burndown loop.

## Board facts (agent-gpu)

- Owner/repo: `jaypetez/agent-gpu`
- Project: user `jaypetez`, project number **10**, id `PVT_kwHOBKgqLs4BaouJ`
- Status options (in order): `Backlog → Ready → In Progress → In Review → Done`
- Priority labels: `priority:high`, `priority:medium`, `priority:low`
- Milestones are epics, numbered 1→13 (lower = earlier in the roadmap).
- **Issue #1** decides the implementation language/runtime. Until #1 is **Done**, any issue that
  requires writing application code is **blocked** — only #1 itself, docs, and tooling/infra issues
  are actionable before then.

## Step 1 — read the board

Fetch every item with its Status, issue number, state, labels, milestone, and body. Also grab the
Status field id and option ids (needed for the move in Step 4):

```bash
gh api graphql -f query='
query($org:String!, $num:Int!){
  user(login:$org){
    projectV2(number:$num){
      id
      field(name:"Status"){ ... on ProjectV2SingleSelectField{ id options{ id name } } }
      items(first:100){
        nodes{
          id
          status: fieldValueByName(name:"Status"){ ... on ProjectV2ItemFieldSingleSelectValue{ name } }
          content{
            ... on Issue{
              number title state body
              labels(first:20){ nodes{ name } }
              milestone{ title number }
            }
          }
        }
      }
    }
  }
}' -F org=jaypetez -F num=10
```

## Step 2 — build the candidate set

1. Keep only items whose issue `state` is `OPEN`.
2. Candidates = items with Status **`Ready`**. If `Ready` is empty, fall back to Status **`Backlog`**.
3. Ignore items already in `In Progress`, `In Review`, or `Done`.

## Step 3 — drop dependency-blocked items, then rank

**Drop** any candidate that is blocked:

- Its body references `depends on #N`, `blocked by #N`, or `needs #N` where issue `#N` is still open.
- It is a sub-issue whose parent is still open.
- It requires writing application code while **#1 is not yet Done** (see Board facts). When unsure
  whether an issue "requires application code", treat language/transport/infra-decision and
  documentation issues as unblocked and feature/implementation issues as blocked-by-#1.

**Rank** the survivors and take the top one:

1. Priority label: `priority:high` > `priority:medium` > `priority:low` > (none).
2. Milestone number ascending (epic order).
3. Issue number ascending.

If **no candidate survives**, do **not** move anything. Report exactly:
`No actionable items: <N> open issues remain but all are blocked or not Ready.` — and list the
blockers. This signal lets a `/goal` evaluator decide whether the burndown is complete.

## Step 4 — move the chosen card to In Progress

Using the chosen item's node id, the project id, the Status field id, and the `In Progress` option
id (all from Step 1):

```bash
gh api graphql -f query='
mutation($proj:ID!, $item:ID!, $field:ID!, $opt:String!){
  updateProjectV2ItemFieldValue(input:{
    projectId:$proj, itemId:$item, fieldId:$field,
    value:{ singleSelectOptionId:$opt }
  }){ projectV2Item{ id } }
}' -F proj=PVT_kwHOBKgqLs4BaouJ -F item=<ITEM_NODE_ID> -F field=<STATUS_FIELD_ID> -F opt=<IN_PROGRESS_OPTION_ID>
```

Only move the card after you are certain of the selection. Never move more than one item.

## Step 5 — report

Output, concisely:

- **Chosen:** `#<number> <title>` — milestone, labels, priority.
- **Why:** one line (e.g. "highest-priority unblocked item in Ready").
- **Body:** the issue's full body (Description / Scope / Acceptance Criteria / Out of scope) so the
  next step (`audit-issue`) and the implementer have it.
- Confirm the card was moved to **In Progress**.

Do **not** start implementing here — that is the job of the burndown workflow (see CLAUDE.md).
