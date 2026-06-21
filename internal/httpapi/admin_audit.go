package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/jaypetez/agent-gpu/internal/audit"
	"github.com/jaypetez/agent-gpu/internal/store"
)

// Audit recording for the admin write surface (#90). Every admin WRITE handler
// records exactly one audit.Entry capturing who did it (the actor key id from
// the request context), what they did (the op), the target resource, the
// redacted before/after projection of the affected object, the request
// correlation id, and the outcome (success/failure).
//
// Secret hygiene is structural: the before/after snapshots are built ONLY from
// auditKeyValues (which projects the same metadata-only fields as adminKeyView —
// never SecretHash/Salt) or from small explicit maps. There is no code path that
// places secret material into an audit entry.

// recordAudit appends one audit entry for an admin write, stamping the actor
// (from the authenticated key on the request context) and the request
// correlation id automatically. It is a no-op when no audit store is wired
// (nil-safe, so tests and embedders without auditing are unaffected). A
// checkpoint-write failure cannot occur here (Append is in-memory); durability
// is handled by the periodic checkpoint in cmd.
func (s *Server) recordAudit(r *http.Request, op, target string, outcome audit.Outcome, before, after audit.RedactedValues) {
	if s.auditLog == nil {
		return
	}
	actor := ""
	if key, ok := keyFromContext(r.Context()); ok {
		actor = key.ID
	}
	requestID, _ := requestIDFromContext(r.Context())
	_ = s.auditLog.Append(audit.Entry{
		Actor:     actor,
		Op:        op,
		Target:    target,
		Before:    before,
		After:     after,
		RequestID: requestID,
		Outcome:   outcome,
	})
}

// auditKeyValues projects an API key into the redacted before/after snapshot
// shape used in audit entries. It mirrors adminKeyView's safe fields and, like
// it, NEVER includes SecretHash or Salt — so an audit entry can never carry
// secret material. A zero key (e.g. the "before" of a create) yields nil so the
// field is omitted from the entry.
func auditKeyValues(k store.APIKey) audit.RedactedValues {
	if k.ID == "" {
		return nil
	}
	v := audit.RedactedValues{
		"id":           k.ID,
		"name":         k.Name,
		"roles":        orEmpty(k.Roles),
		"admin_scopes": orEmpty(k.AdminScopes),
		"allow_models": orEmpty(k.AllowModels),
		"deny_models":  orEmpty(k.DenyModels),
		"revoked":      k.Revoked(),
	}
	if k.Limits != nil {
		v["limits"] = map[string]uint64{
			"rpm":            k.Limits.RPM,
			"tpm":            k.Limits.TPM,
			"daily_tokens":   k.Limits.DailyTokens,
			"monthly_tokens": k.Limits.MonthlyTokens,
		}
	}
	return v
}

// auditOutcome maps a handler error to the audit outcome: nil → success, any
// error → failure. It keeps the call sites terse.
func auditOutcome(err error) audit.Outcome {
	if err != nil {
		return audit.OutcomeFailure
	}
	return audit.OutcomeSuccess
}

// Admin audit op names. Centralized so the audit vocabulary is consistent and
// greppable, and so the queryable endpoint (#91) and tests share one source.
const (
	auditOpKeyCreate      = "key.create"
	auditOpKeyRevoke      = "key.revoke"
	auditOpKeyRotate      = "key.rotate"
	auditOpKeyPermissions = "key.permissions"
	auditOpKeyQuota       = "key.quota"
	auditOpWorkerDrain    = "worker.drain"
	auditOpModelPull      = "model.pull"
	auditOpModelUnload    = "model.unload"
	auditOpConfigUpdate   = "config.update"
)

// handleAdminAudit serves GET /v1/admin/audit (#91): the queryable read seam over
// the append-only audit log built in #90. It returns the recorded entries —
// newest first — in the shared cursor-paginated list envelope
// ({"data":[...],"pagination":{...}}), narrowed by the optional filters:
//
//   - actor   — the actor key id that performed the operation
//   - op      — the operation name (e.g. "key.create", "worker.drain")
//   - target  — the resource id the operation acted on
//   - since   — unix-seconds lower bound, inclusive (entries at or after it)
//   - until   — unix-seconds upper bound, exclusive (entries strictly before it)
//
// Non-empty filters are ANDed (audit.Filter). The since/until bounds are unix
// seconds to match the timestamp convention used elsewhere in the admin API
// (the quota reset timestamps); an absent or unparseable bound is treated as
// unbounded on that side rather than erroring, so a stale or hand-edited query
// degrades to "wider window" instead of a 400. The handler is gated to the
// audit:read scope (s.requireScope), so a key lacking it gets 403 and an
// unauthenticated request 401 before this runs.
//
// Entries are already redacted by the store (the before/after snapshots carry
// only safe metadata fields — never SecretHash/Salt; see admin_audit.go), so
// they are passed straight through: audit.Entry's JSON tags are the wire shape.
// When no audit store is wired (nil-safe, mirroring recordAudit) the endpoint
// returns a well-formed empty page rather than failing.
func (s *Server) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := audit.Filter{
		Actor:  q.Get("actor"),
		Op:     q.Get("op"),
		Target: q.Get("target"),
		Since:  parseUnixSeconds(q.Get("since")),
		Until:  parseUnixSeconds(q.Get("until")),
	}

	var entries []audit.Entry
	if s.auditLog != nil {
		// List already returns newest-first and filtered; an uncapped read (limit 0)
		// hands the full matching set to the shared paginator, which slices the page.
		entries = s.auditLog.List(filter, 0)
	}

	limit, offset := parsePageParams(r)
	writeList(w, entries, limit, offset)
}

// parseUnixSeconds parses a unix-seconds timestamp query parameter into a UTC
// time.Time, returning the zero time (an unbounded time filter) for an absent or
// unparseable value. Keeping the "garbage → unbounded" rule local to the audit
// handler matches the graceful-degradation discipline of the cursor parser (a
// bad token restarts pagination rather than erroring) — a malformed date widens
// the window instead of failing the request.
func parseUnixSeconds(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}
