# Contributing to agent-gpu

Thanks for your interest in contributing! This document explains how work is organized and how
to get changes merged.

## Project organization

Work is tracked as **GitHub Issues** grouped into **Milestones**, where each milestone represents
an epic from the roadmap (e.g. *Core Architecture*, *Permissions + Security*, *GPU Scheduling +
Capacity Management*). The [agent-gpu roadmap](https://github.com/users/jaypetez/projects/10)
project board tracks status across these columns:

`Backlog → Ready → In Progress → In Review → Done`

### Labels

- **Type:** `epic`, `enhancement`, `bug`, `chore`, `documentation`
- **Area:** `backend`, `infra`, `api`, `security`, `testing`, `docs`, `cli`, `observability`,
  `scheduler`, `ollama`, `docker`, `cross-platform`, `quota`
- **Priority:** `priority:high`, `priority:medium`, `priority:low`

## Development workflow

1. Find or open an issue describing the change. Comment to claim it.
2. Create a branch: `feature/<short-desc>`, `fix/<short-desc>`, or `chore/<short-desc>`.
3. Make your change with tests (see below).
4. Open a pull request that references the issue (e.g. `Closes #12`).
5. Ensure CI is green; address review feedback.

## Local setup

> The implementation language/runtime is decided in
> [#1 Server + Worker Architecture](https://github.com/jaypetez/agent-gpu/issues/1); build and run
> instructions will be filled in as the toolchain lands. Until then, use Docker Compose for a
> full local stack (see [#18](https://github.com/jaypetez/agent-gpu/issues/18)).

```bash
# Bring up server + workers + backing services
docker compose up
```

## Testing

Every change should ship with tests:

- **Unit tests** for individual packages/functions.
- **Integration tests** for cross-component flows (registration, routing, quota/permission
  enforcement).

Run the suite locally before opening a PR (`go test -race ./...`, as CI does). Coverage is reported
in CI.

See [docs/testing.md](docs/testing.md) for the integration-test layout and the rules that keep the
suite deterministic — poll with `waitFor` instead of `time.Sleep`, inject a clock and fast-forward
instead of sleeping through real windows, keep real timeouts short, guard cross-goroutine state with
a mutex, and run `-race`.

## Commit & PR conventions

- Keep PRs focused; one logical change per PR where possible.
- Write clear commit messages in the imperative mood.
- Link the issue the PR resolves.

## Code of conduct

Be respectful and constructive. Harassment or abuse will not be tolerated.

## Security

Please do **not** open public issues for security vulnerabilities. See [SECURITY.md](SECURITY.md).
