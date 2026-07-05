package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jaypetez/agent-gpu/internal/audit"
	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
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

// ---- GET /v1/admin/audit endpoint (#91) ----

// auditEntryView is the decoded wire shape of one GET /v1/admin/audit entry,
// mirroring audit.Entry's JSON tags so a test can assert on the projection.
type auditEntryView struct {
	Time      time.Time      `json:"time"`
	Actor     string         `json:"actor"`
	Op        string         `json:"op"`
	Target    string         `json:"target"`
	Before    map[string]any `json:"before"`
	After     map[string]any `json:"after"`
	RequestID string         `json:"request_id"`
	Outcome   string         `json:"outcome"`
}

// auditListView is the decoded list envelope returned by GET /v1/admin/audit.
type auditListView struct {
	Data       []auditEntryView `json:"data"`
	Pagination struct {
		NextCursor *string `json:"next_cursor"`
		HasMore    bool    `json:"has_more"`
	} `json:"pagination"`
}

func decodeAuditList(t *testing.T, rec *httptest.ResponseRecorder) auditListView {
	t.Helper()
	var out auditListView
	decode(t, rec, &out)
	return out
}

// auditAt builds a fixed UTC instant offset by sec seconds from a stable base, so
// seeded entries have deterministic, well-separated timestamps for ordering and
// time-window assertions.
func auditAt(sec int) time.Time {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return base.Add(time.Duration(sec) * time.Second)
}

func auditUnix(tm time.Time) string { return strconv.FormatInt(tm.Unix(), 10) }

// seedAuditEntries appends a fixed, deterministic set of entries (distinct
// actors, ops, targets, and well-separated timestamps) so the filter and
// ordering assertions over GET /v1/admin/audit are exact.
func seedAuditEntries(t *testing.T, log *audit.MemoryStore) {
	t.Helper()
	entries := []audit.Entry{
		{Time: auditAt(10), Actor: "key_a", Op: auditOpKeyCreate, Target: "key_x", Outcome: audit.OutcomeSuccess, RequestID: "r1", After: audit.RedactedValues{"id": "key_x"}},
		{Time: auditAt(20), Actor: "key_b", Op: auditOpKeyRevoke, Target: "key_x", Outcome: audit.OutcomeSuccess, RequestID: "r2"},
		{Time: auditAt(30), Actor: "key_a", Op: auditOpWorkerDrain, Target: "worker_1", Outcome: audit.OutcomeFailure, RequestID: "r3"},
		{Time: auditAt(40), Actor: "key_a", Op: auditOpKeyQuota, Target: "key_y", Outcome: audit.OutcomeSuccess, RequestID: "r4"},
	}
	for _, e := range entries {
		if err := log.Append(e); err != nil {
			t.Fatalf("seed audit entry: %v", err)
		}
	}
}

// TestAdminAuditEndpointHappy proves AC1/AC3: an admin key gets 200 and the
// recorded entries, newest first, in the shared {data,pagination} envelope.
func TestAdminAuditEndpointHappy(t *testing.T) {
	s, authSvc, log := adminTestServerWithAudit(t, &fakeFleet{})
	seedAuditEntries(t, log)
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodGet, "/v1/admin/audit", adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	out := decodeAuditList(t, rec)
	if len(out.Data) != 4 {
		t.Fatalf("entries = %d, want 4", len(out.Data))
	}
	// Newest first: auditAt(40) → auditAt(10).
	wantOps := []string{auditOpKeyQuota, auditOpWorkerDrain, auditOpKeyRevoke, auditOpKeyCreate}
	for i, op := range wantOps {
		if out.Data[i].Op != op {
			t.Errorf("entry[%d].op = %q, want %q (order should be newest-first)", i, out.Data[i].Op, op)
		}
	}
	for i := 1; i < len(out.Data); i++ {
		if out.Data[i-1].Time.Before(out.Data[i].Time) {
			t.Errorf("entries not newest-first at %d: %v before %v", i, out.Data[i-1].Time, out.Data[i].Time)
		}
	}
	// The redacted projection round-trips and the request id is present.
	if out.Data[3].After["id"] != "key_x" || out.Data[3].RequestID != "r1" {
		t.Errorf("create entry projection wrong: %+v", out.Data[3])
	}
	// The drain entry (auditAt(30)) is at index 1 in newest-first order and was
	// seeded as a failure.
	if out.Data[1].Op != auditOpWorkerDrain || out.Data[1].Outcome != string(audit.OutcomeFailure) {
		t.Errorf("drain entry = %+v, want op=%s outcome=failure", out.Data[1], auditOpWorkerDrain)
	}
	if out.Pagination.HasMore {
		t.Errorf("single page should not report has_more")
	}
}

// TestAdminAuditEndpointUnauthenticated proves AC1: no token → 401 (the route is
// gated before the handler runs).
func TestAdminAuditEndpointUnauthenticated(t *testing.T) {
	s, _, log := adminTestServerWithAudit(t, &fakeFleet{})
	seedAuditEntries(t, log)

	rec := req(t, s, http.MethodGet, "/v1/admin/audit", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestAdminAuditEndpointScopeGate proves AC1: a key WITHOUT audit:read (and not
// admin) is 403, while a key holding ONLY audit:read passes with 200.
func TestAdminAuditEndpointScopeGate(t *testing.T) {
	s, authSvc, log := adminTestServerWithAudit(t, &fakeFleet{})
	seedAuditEntries(t, log)

	// A key with an unrelated scope must not pass the audit:read gate.
	otherToken := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeKeysRead}})
	rec := req(t, s, http.MethodGet, "/v1/admin/audit", otherToken, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("keys:read key status = %d, want 403", rec.Code)
	}
	if code := errorCode(t, rec); code != "forbidden" {
		t.Errorf("error code = %q, want forbidden", code)
	}

	// A key holding exactly audit:read passes.
	auditToken := mustKey(t, authSvc, auth.Permissions{AdminScopes: []string{authz.ScopeAuditRead}})
	rec = req(t, s, http.MethodGet, "/v1/admin/audit", auditToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("audit:read key status = %d, want 200", rec.Code)
	}
}

// TestAdminAuditEndpointFilters proves AC2: each filter (actor, op, target,
// since, until) narrows the result set correctly, table-driven, including the
// AND-combination and the garbage-bound-is-unbounded rule.
func TestAdminAuditEndpointFilters(t *testing.T) {
	s, authSvc, log := adminTestServerWithAudit(t, &fakeFleet{})
	seedAuditEntries(t, log)
	adminToken := mustKey(t, authSvc, adminPerms())

	cases := []struct {
		name     string
		query    string
		wantOps  []string // expected ops, newest-first
		wantNone bool
	}{
		{
			name:    "actor",
			query:   "?actor=key_a",
			wantOps: []string{auditOpKeyQuota, auditOpWorkerDrain, auditOpKeyCreate},
		},
		{
			name:    "op",
			query:   "?op=" + auditOpKeyCreate,
			wantOps: []string{auditOpKeyCreate},
		},
		{
			name:    "target",
			query:   "?target=key_x",
			wantOps: []string{auditOpKeyRevoke, auditOpKeyCreate},
		},
		{
			// since is inclusive: auditAt(20) and later. Excludes auditAt(10).
			name:    "since inclusive",
			query:   "?since=" + auditUnix(auditAt(20)),
			wantOps: []string{auditOpKeyQuota, auditOpWorkerDrain, auditOpKeyRevoke},
		},
		{
			// until is exclusive: strictly before auditAt(30). Keeps auditAt(10),(20).
			name:    "until exclusive",
			query:   "?until=" + auditUnix(auditAt(30)),
			wantOps: []string{auditOpKeyRevoke, auditOpKeyCreate},
		},
		{
			// Half-open window [20,40): keeps auditAt(20),(30); excludes (10),(40).
			name:    "since and until window",
			query:   "?since=" + auditUnix(auditAt(20)) + "&until=" + auditUnix(auditAt(40)),
			wantOps: []string{auditOpWorkerDrain, auditOpKeyRevoke},
		},
		{
			// Combined AND: actor key_a within [30,...] → only auditAt(30),(40).
			name:    "actor and since combined",
			query:   "?actor=key_a&since=" + auditUnix(auditAt(30)),
			wantOps: []string{auditOpKeyQuota, auditOpWorkerDrain},
		},
		{
			name:     "no match",
			query:    "?actor=nobody",
			wantNone: true,
		},
		{
			// Garbage time bound is treated as unbounded, not an error: all 4 returned.
			name:    "garbage since is unbounded",
			query:   "?since=not-a-number",
			wantOps: []string{auditOpKeyQuota, auditOpWorkerDrain, auditOpKeyRevoke, auditOpKeyCreate},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := req(t, s, http.MethodGet, "/v1/admin/audit"+tc.query, adminToken, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			out := decodeAuditList(t, rec)
			if tc.wantNone {
				if len(out.Data) != 0 {
					t.Fatalf("want empty result, got %d entries: %+v", len(out.Data), out.Data)
				}
				if out.Pagination.HasMore {
					t.Errorf("empty result should not report has_more")
				}
				return
			}
			if len(out.Data) != len(tc.wantOps) {
				t.Fatalf("entries = %d, want %d: %+v", len(out.Data), len(tc.wantOps), out.Data)
			}
			for i, op := range tc.wantOps {
				if out.Data[i].Op != op {
					t.Errorf("entry[%d].op = %q, want %q", i, out.Data[i].Op, op)
				}
			}
		})
	}
}

// TestAdminAuditEndpointPagination proves AC1/AC3: ?limit= bounds the page and
// the returned cursor follows to the next page, with the newest-first order
// preserved across the boundary.
func TestAdminAuditEndpointPagination(t *testing.T) {
	s, authSvc, log := adminTestServerWithAudit(t, &fakeFleet{})
	seedAuditEntries(t, log)
	adminToken := mustKey(t, authSvc, adminPerms())

	// Page 1: two newest entries, has_more true with a cursor.
	rec := req(t, s, http.MethodGet, "/v1/admin/audit?limit=2", adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("page1 status = %d, want 200", rec.Code)
	}
	p1 := decodeAuditList(t, rec)
	if len(p1.Data) != 2 {
		t.Fatalf("page1 entries = %d, want 2", len(p1.Data))
	}
	if p1.Data[0].Op != auditOpKeyQuota || p1.Data[1].Op != auditOpWorkerDrain {
		t.Errorf("page1 order wrong: %s,%s", p1.Data[0].Op, p1.Data[1].Op)
	}
	if !p1.Pagination.HasMore || p1.Pagination.NextCursor == nil {
		t.Fatalf("page1 should have a next cursor: %+v", p1.Pagination)
	}

	// Page 2: the remaining two, via the cursor; last page.
	rec = req(t, s, http.MethodGet, "/v1/admin/audit?limit=2&cursor="+*p1.Pagination.NextCursor, adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("page2 status = %d, want 200", rec.Code)
	}
	p2 := decodeAuditList(t, rec)
	if len(p2.Data) != 2 {
		t.Fatalf("page2 entries = %d, want 2", len(p2.Data))
	}
	if p2.Data[0].Op != auditOpKeyRevoke || p2.Data[1].Op != auditOpKeyCreate {
		t.Errorf("page2 order wrong: %s,%s", p2.Data[0].Op, p2.Data[1].Op)
	}
	if p2.Pagination.HasMore || p2.Pagination.NextCursor != nil {
		t.Errorf("page2 should be the last page: %+v", p2.Pagination)
	}
}

// TestAdminAuditEndpointEmptyStore proves AC3: with no recorded entries the
// endpoint returns a well-formed empty page (data is [], not null; has_more
// false).
func TestAdminAuditEndpointEmptyStore(t *testing.T) {
	s, authSvc, _ := adminTestServerWithAudit(t, &fakeFleet{})
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodGet, "/v1/admin/audit", adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// data must be a JSON array, never null.
	if !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Errorf("empty data should marshal as [], got: %s", rec.Body.String())
	}
	out := decodeAuditList(t, rec)
	if len(out.Data) != 0 || out.Pagination.HasMore || out.Pagination.NextCursor != nil {
		t.Errorf("empty store should be an empty last page: %+v", out)
	}
}

// TestAdminAuditEndpointNilStore proves AC3: a Server built without an audit
// store does NOT 500 — it returns a well-formed empty page (the handler is
// nil-safe, mirroring recordAudit).
func TestAdminAuditEndpointNilStore(t *testing.T) {
	s, authSvc, _ := adminTestServerWithAudit(t, &fakeFleet{})
	s.auditLog = nil // simulate an embedder without auditing wired.
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodGet, "/v1/admin/audit", adminToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (nil store must not 500)", rec.Code)
	}
	out := decodeAuditList(t, rec)
	if len(out.Data) != 0 || out.Pagination.HasMore {
		t.Errorf("nil store should be an empty page: %+v", out)
	}
}

// TestAdminAuditEndpointNeverLeaksSecret proves AC3: the audit response body
// carries no secret material, even with entries whose before/after maps include
// key metadata.
func TestAdminAuditEndpointNeverLeaksSecret(t *testing.T) {
	s, authSvc, log := adminTestServerWithAudit(t, &fakeFleet{})
	seedAuditEntries(t, log)
	adminToken := mustKey(t, authSvc, adminPerms())

	rec := req(t, s, http.MethodGet, "/v1/admin/audit", adminToken, "")
	assertNoSecret(t, rec.Body.String())
}
