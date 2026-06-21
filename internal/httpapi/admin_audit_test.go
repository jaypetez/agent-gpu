package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jaypetez/agent-gpu/internal/audit"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// TestAuditRecordsEveryWrite proves AC3: every admin WRITE records one audit
// entry with the actor, op, target, request_id, and outcome — and reads do NOT.
func TestAuditRecordsEveryWrite(t *testing.T) {
	fleet := &fakeFleet{snapshot: []types.Worker{{ID: "w1", Status: types.WorkerOnline}}}
	s, authSvc, auditLog := adminTestServerWithAudit(t, fleet)
	adminToken := mustKey(t, authSvc, adminPerms())

	// Create (write).
	rec := req(t, s, http.MethodPost, "/v1/admin/keys", adminToken,
		`{"name":"svc","roles":["user"],"admin_scopes":["keys:read"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rec.Code)
	}
	var created struct {
		ID string `json:"id"`
	}
	decode(t, rec, &created)
	id := created.ID

	// A read must NOT record an audit entry.
	_ = req(t, s, http.MethodGet, "/v1/admin/keys", adminToken, "")
	_ = req(t, s, http.MethodGet, "/v1/admin/keys/"+id, adminToken, "")

	// The remaining writes.
	_ = req(t, s, http.MethodPut, "/v1/admin/keys/"+id+"/permissions", adminToken, `{"roles":["read-only"]}`)
	_ = req(t, s, http.MethodPut, "/v1/admin/keys/"+id+"/quota", adminToken, `{"rpm":5}`)
	_ = req(t, s, http.MethodPost, "/v1/admin/keys/"+id+"/rotate", adminToken, "")
	_ = req(t, s, http.MethodPost, "/v1/admin/workers/w1/drain", adminToken, "")
	_ = req(t, s, http.MethodDelete, "/v1/admin/keys/"+id, adminToken, "")

	entries := auditLog.List(audit.Filter{}, 0)
	// 6 writes: create, permissions, quota, rotate, drain, revoke. (2 reads excluded.)
	if len(entries) != 6 {
		t.Fatalf("recorded %d audit entries, want 6:\n%v", len(entries), opsOf(entries))
	}

	wantOps := map[string]bool{
		auditOpKeyCreate: false, auditOpKeyPermissions: false, auditOpKeyQuota: false,
		auditOpKeyRotate: false, auditOpWorkerDrain: false, auditOpKeyRevoke: false,
	}
	for _, e := range entries {
		if _, ok := wantOps[e.Op]; !ok {
			t.Errorf("unexpected op %q", e.Op)
			continue
		}
		wantOps[e.Op] = true
		if e.Outcome != audit.OutcomeSuccess {
			t.Errorf("op %q outcome = %q, want success", e.Op, e.Outcome)
		}
		// Every write was performed by the admin key, and the request_id is stamped
		// (the routed handler runs behind requestIDMiddleware).
		if e.RequestID == "" {
			t.Errorf("op %q missing request_id", e.Op)
		}
		if e.Actor == "" {
			t.Errorf("op %q missing actor", e.Op)
		}
	}
	for op, seen := range wantOps {
		if !seen {
			t.Errorf("missing audit entry for op %q", op)
		}
	}
}

// TestAuditCapturesBeforeAfter proves a permissions change records the prior and
// resulting redacted state, so the trail shows what changed.
func TestAuditCapturesBeforeAfter(t *testing.T) {
	s, authSvc, auditLog := adminTestServerWithAudit(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodPost, "/v1/admin/keys", adminToken,
		`{"name":"svc","roles":["user"]}`)
	var created struct {
		ID string `json:"id"`
	}
	decode(t, rec, &created)

	_ = req(t, s, http.MethodPut, "/v1/admin/keys/"+created.ID+"/permissions", adminToken,
		`{"roles":["read-only"]}`)

	entries := auditLog.List(audit.Filter{Op: auditOpKeyPermissions}, 0)
	if len(entries) != 1 {
		t.Fatalf("want 1 permissions entry, got %d", len(entries))
	}
	e := entries[0]
	beforeRoles, _ := e.Before["roles"].([]string)
	afterRoles, _ := e.After["roles"].([]string)
	if len(beforeRoles) != 1 || beforeRoles[0] != "user" {
		t.Errorf("before roles = %v, want [user]", e.Before["roles"])
	}
	if len(afterRoles) != 1 || afterRoles[0] != "read-only" {
		t.Errorf("after roles = %v, want [read-only]", e.After["roles"])
	}
}

// TestAuditFailureRecorded proves a failed write (revoke of an unknown key) is
// recorded with outcome=failure.
func TestAuditFailureRecorded(t *testing.T) {
	s, authSvc, auditLog := adminTestServerWithAudit(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodDelete, "/v1/admin/keys/ghost", adminToken, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("revoke unknown status = %d, want 404", rec.Code)
	}
	entries := auditLog.List(audit.Filter{Op: auditOpKeyRevoke}, 0)
	if len(entries) != 1 || entries[0].Outcome != audit.OutcomeFailure {
		t.Fatalf("failed revoke not recorded as failure: %+v", entries)
	}
	if entries[0].Target != "ghost" {
		t.Errorf("target = %q, want ghost", entries[0].Target)
	}
}

// TestAuditNeverLeaksSecret proves AC3's redaction requirement: no audit entry —
// including the create entry for a key whose plaintext token was just returned —
// serializes any secret material.
func TestAuditNeverLeaksSecret(t *testing.T) {
	s, authSvc, auditLog := adminTestServerWithAudit(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodPost, "/v1/admin/keys", adminToken, `{"name":"svc"}`)
	var created struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	decode(t, rec, &created)
	_ = req(t, s, http.MethodPost, "/v1/admin/keys/"+created.ID+"/rotate", adminToken, "")

	data, err := json.Marshal(auditLog.List(audit.Filter{}, 0))
	if err != nil {
		t.Fatalf("marshal entries: %v", err)
	}
	body := strings.ToLower(string(data))
	for _, banned := range []string{"secrethash", "secret_hash", "\"salt\"", "token"} {
		if strings.Contains(body, banned) {
			t.Fatalf("audit entries leak secret material (%q): %s", banned, body)
		}
	}
	// The one-time tokens themselves must never appear.
	if created.Token != "" && strings.Contains(string(data), created.Token) {
		t.Fatal("audit entries contain the plaintext token")
	}
}

// TestAuditNilSafe proves a server without an audit store (the default unit
// harness path) still serves writes — the recording calls are no-ops.
func TestAuditNilSafe(t *testing.T) {
	s, authSvc := testServer(t, &fakeFleet{}) // no audit store, no quota engine
	s.quota = nil
	adminToken := mustKey(t, authSvc, adminPerms())
	rec := req(t, s, http.MethodPost, "/v1/admin/keys", adminToken, `{"name":"svc"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create without audit store status = %d, want 201", rec.Code)
	}
}

func opsOf(es []audit.Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Op + "/" + string(e.Outcome)
	}
	return out
}
