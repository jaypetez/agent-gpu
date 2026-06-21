package httpapi

import (
	"net/http"

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
)
