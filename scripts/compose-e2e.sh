#!/usr/bin/env bash
# End-to-end smoke test for the agent-gpu Compose stack.
#
# Proves issue #18's acceptance criteria against a real stack:
#   1. `docker compose up` yields a working server with >=1 registered worker.
#   2. State (keys/quotas) persists across a restart via the backing volume.
#   3. A sample inference request succeeds end to end.
#   4. Teardown is clean (always `down -v` on exit).
#
# It runs entirely from the host (which has curl), because the server image is
# distroless and has no shell/curl of its own. Used by `make compose-e2e` and the
# `compose` CI job.
#
# Env:
#   COMPOSE          docker compose invocation (default "docker compose")
#   AGENTGPU_MODEL   model to pull + infer (default qwen2:0.5b)
#   BASE_URL         server base URL (default http://localhost:8080)
#   SKIP_INFERENCE   if "1", stop after worker registration (skip the model
#                    pull + chat). Useful where a model pull is too slow/flaky.
set -euo pipefail

# On Git Bash / MSYS2 (Windows), a leading-slash argument like the in-container
# path `/agentgpu` is rewritten to a Windows path (C:/Program Files/Git/agentgpu)
# before it reaches `docker compose exec`, which then fails with exit 127. Disable
# that path conversion so the container-side path is passed through verbatim. The
# variable is inert on Linux/macOS, so this is a safe no-op there (and in CI).
export MSYS_NO_PATHCONV=1

COMPOSE="${COMPOSE:-docker compose}"
MODEL="${AGENTGPU_MODEL:-qwen2:0.5b}"
BASE_URL="${BASE_URL:-http://localhost:8080}"
SKIP_INFERENCE="${SKIP_INFERENCE:-0}"

# Resolve the repo root from this script's location so it runs from anywhere.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

log()  { printf '\n=== %s ===\n' "$*"; }
fail() { printf 'E2E FAILED: %s\n' "$*" >&2; exit 1; }

# Always tear the stack down (removing volumes) on exit so a failed run does not
# leak containers/volumes or wedge the next run.
cleanup() {
  log "teardown (docker compose down -v)"
  $COMPOSE down -v --remove-orphans || true
}
trap cleanup EXIT

# code <url> [auth] -> prints the HTTP status code for a GET (empty on connect failure).
code() {
  local url="$1" auth="${2:-}"
  if [ -n "$auth" ]; then
    curl -fsS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $auth" "$url" 2>/dev/null || true
  else
    curl -s -o /dev/null -w '%{http_code}' "$url" 2>/dev/null || true
  fi
}

# wait_for <desc> <timeout-seconds> <command...> -> polls command until it exits 0.
wait_for() {
  local desc="$1" timeout="$2"; shift 2
  local i=0
  until "$@"; do
    i=$((i + 1))
    if [ "$i" -ge "$timeout" ]; then
      return 1
    fi
    sleep 1
  done
  printf 'ready: %s (%ss)\n' "$desc" "$i"
}

# ---- 1. bring the stack up -------------------------------------------------
log "build + start the stack"
# Pull the model via ollama-init too; without it the worker has nothing to serve.
$COMPOSE up -d --build

# ---- 2. server readiness (unauthenticated GET /v1/models -> 401) -----------
log "wait for the server (GET /v1/models -> 401)"
server_up() { [ "$(code "$BASE_URL/v1/models")" = "401" ]; }
wait_for "server up" 90 server_up || {
  $COMPOSE logs server || true
  fail "server did not become ready (no 401 on /v1/models)"
}

# parse_token <key-create-output> -> prints the one-time plaintext token.
parse_token() { printf '%s\n' "$1" | sed -n 's/^Token: //p'; }

# ---- 3. key bootstrap ------------------------------------------------------
# The file-backed key store is loaded into memory ONCE at server start and is not
# hot-reloaded, so a key created with `exec ... key create` is written to the
# /data volume but is not seen by the ALREADY-RUNNING server until it reloads.
# We therefore create BOTH keys now and restart the server once (next step) so it
# picks them up — which doubles as the persistence proof. See docs/docker.md.
log "create admin + user keys with --local (written to the /data volume)"
admin_out="$($COMPOSE exec -T server /agentgpu key create --name e2e-admin --role admin --local)"
printf '%s\n' "$admin_out"
ADMIN_TOKEN="$(parse_token "$admin_out")"
[ -n "$ADMIN_TOKEN" ] || fail "could not parse admin token from key create output"

user_out="$($COMPOSE exec -T server /agentgpu key create --name e2e-user --role user --allow-model "$MODEL" --local)"
printf '%s\n' "$user_out"
USER_TOKEN="$(parse_token "$user_out")"
[ -n "$USER_TOKEN" ] || fail "could not parse user token from key create output"

# ---- 4. AC#2: persistence across a restart (also activates the new keys) ----
# Restart ONLY the server (keep the agentgpu-data volume). It reloads keys.json
# from the volume, so afterward the keys created above authenticate — proving both
# that state lives on the volume (survives a restart) and that it is the same file
# the CLI wrote. A token rejected before the restart and accepted after is the
# proof.
log "admin token BEFORE restart (expected 401 — server has not loaded it yet)"
echo "  -> HTTP $(code "$BASE_URL/v1/admin/workers" "$ADMIN_TOKEN")"
log "restart the server so it reloads the key store from the volume"
$COMPOSE restart server
wait_for "server back up" 90 server_up || fail "server did not come back after restart"
auth_ok() { [ "$(code "$BASE_URL/v1/admin/workers" "$ADMIN_TOKEN")" = "200" ]; }
wait_for "admin key valid after restart" 30 auth_ok \
  || fail "admin key did not authenticate after restart — persistence/reload broken"
log "PERSISTENCE OK: a key written to the volume authenticated after a server restart"

# ---- 5. AC#1: at least one registered worker -------------------------------
# The worker re-establishes its control stream after the server restart (reconnect
# backoff), so wait for the fleet to show it.
log "wait for >=1 registered worker (GET /v1/admin/workers)"
worker_count() {
  local body
  body="$(curl -fsS -H "Authorization: Bearer $ADMIN_TOKEN" "$BASE_URL/v1/admin/workers" 2>/dev/null)" || { echo 0; return; }
  printf '%s' "$body" | grep -o '"id"' | wc -l | tr -d ' '
}
have_worker() { [ "$(worker_count)" -ge 1 ]; }
wait_for "worker registered" 60 have_worker || {
  $COMPOSE logs worker || true
  fail "no worker registered with the server"
}
log "fleet snapshot"
curl -fsS -H "Authorization: Bearer $ADMIN_TOKEN" "$BASE_URL/v1/admin/workers"; echo

if [ "$SKIP_INFERENCE" = "1" ]; then
  log "SKIP_INFERENCE=1 — stopping after registration + persistence"
  log "E2E PASSED (registration + persistence; inference skipped)"
  exit 0
fi

# ---- 6. AC#3: end-to-end inference -----------------------------------------
# The user key (scoped to just this model; deny-by-default otherwise) was created
# above and activated by the restart. The model is only routable once the worker
# advertises it (from Ollama /api/tags), so wait for it to appear in the per-key
# catalog before sending the chat — otherwise the submit queues and blocks.

log "wait for $MODEL to be advertised (GET /v1/models)"
model_listed() {
  curl -fsS -H "Authorization: Bearer $USER_TOKEN" "$BASE_URL/v1/models" 2>/dev/null \
    | grep -q "\"id\":\"$MODEL\""
}
# Generous: the model pull (ollama-init) can take a while on a cold cache.
wait_for "$MODEL advertised" 600 model_listed || {
  $COMPOSE logs ollama-init || true
  $COMPOSE logs worker || true
  fail "$MODEL never advertised — pull or worker model refresh did not complete"
}

log "send a chat completion"
resp="$(curl -fsS -H "Authorization: Bearer $USER_TOKEN" -H 'Content-Type: application/json' \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Say hi in one word.\"}]}" \
  "$BASE_URL/v1/chat/completions")"
printf '%s\n' "$resp"
printf '%s' "$resp" | grep -q '"chat.completion"' || fail "response is not a chat.completion: $resp"
printf '%s' "$resp" | grep -q '"content"' || fail "response has no message content: $resp"

log "E2E PASSED: server up, worker registered, state persisted, inference returned a completion"
