package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/jaypetez/agent-gpu/internal/audit"
	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/authz"
	"github.com/jaypetez/agent-gpu/internal/queue"
	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/store"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// The admin API (#4, scoped in #90): admin-only HTTP endpoints to manage API
// keys (CRUD), per-key quotas, roles + per-model allow/deny lists, and to
// list/inspect/drain workers. Every route is gated by authMiddleware + the scope
// middleware (registered via s.requireScope / s.requireScopeWrite in
// httpapi.go), so a key lacking the route's admin scope gets 403 and an
// unauthenticated request 401 before any handler runs. The RoleAdmin superuser
// holds every scope, so an admin key passes every route exactly as before.
//
// This package only adds the HTTP surface; the underlying key/quota/permission
// and worker implementations live in internal/auth, internal/quota, and
// internal/server (their own issues, all shipped). Because authorization and the
// quota engine read the key fresh from the store on every check, a change made
// here (permissions, limits, revoke) takes effect immediately with no restart —
// see the immediate-effect integration tests.
//
// Secret hygiene: responses NEVER include the stored SecretHash/Salt. The
// plaintext token is returned exactly once, only on create and rotate (it cannot
// be recovered afterward). The explicit request/response structs below are the
// enforcement: handlers map store.APIKey into a metadata-only view rather than
// serializing the record directly.

// ---- response shapes ----

// adminKeyView is the metadata-only projection of a store.APIKey returned by the
// admin API. It deliberately omits SecretHash and Salt so a secret can never be
// serialized. Revoked is a convenience boolean; LastUsed is omitted (0) when the
// key has never authenticated. The enrichment fields (Owner/Team/ExpiresAt/
// CreatedBy, #96) are omitted when unset so a key created without them renders
// exactly as before.
type adminKeyView struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Owner       string      `json:"owner,omitempty"`
	Team        string      `json:"team,omitempty"`
	Roles       []string    `json:"roles"`
	AdminScopes []string    `json:"admin_scopes"`
	AllowModels []string    `json:"allow_models"`
	DenyModels  []string    `json:"deny_models"`
	Revoked     bool        `json:"revoked"`
	UsageCount  uint64      `json:"usage_count"`
	Created     int64       `json:"created"`
	CreatedBy   string      `json:"created_by,omitempty"`
	LastUsed    int64       `json:"last_used,omitempty"`
	ExpiresAt   *int64      `json:"expires_at,omitempty"`
	Limits      *limitsView `json:"limits,omitempty"`
}

// limitsView mirrors store.Limits for the admin key view. It is nil in the view
// when the key has no per-key override (falls back to the global defaults).
type limitsView struct {
	RPM           uint64 `json:"rpm"`
	TPM           uint64 `json:"tpm"`
	DailyTokens   uint64 `json:"daily_tokens"`
	MonthlyTokens uint64 `json:"monthly_tokens"`
}

// newAdminKeyView projects a store.APIKey into its metadata-only view. Slices are
// emitted as [] (never null) so clients can iterate without a nil guard.
func newAdminKeyView(k store.APIKey) adminKeyView {
	v := adminKeyView{
		ID:          k.ID,
		Name:        k.Name,
		Owner:       k.Owner,
		Team:        k.Team,
		Roles:       orEmpty(k.Roles),
		AdminScopes: orEmpty(k.AdminScopes),
		AllowModels: orEmpty(k.AllowModels),
		DenyModels:  orEmpty(k.DenyModels),
		Revoked:     k.Revoked(),
		UsageCount:  k.UsageCount,
		Created:     k.CreatedAt.Unix(),
		CreatedBy:   k.CreatedBy,
	}
	if !k.LastUsedAt.IsZero() {
		v.LastUsed = k.LastUsedAt.Unix()
	}
	if k.ExpiresAt != nil {
		exp := k.ExpiresAt.Unix()
		v.ExpiresAt = &exp
	}
	if k.Limits != nil {
		v.Limits = &limitsView{
			RPM:           k.Limits.RPM,
			TPM:           k.Limits.TPM,
			DailyTokens:   k.Limits.DailyTokens,
			MonthlyTokens: k.Limits.MonthlyTokens,
		}
	}
	return v
}

// orEmpty returns xs, or a non-nil empty slice when xs is nil, so JSON renders
// [] instead of null.
func orEmpty(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

// adminCreateKeyResponse is the POST /v1/admin/keys response. It is the only
// place (besides rotate) Token is populated: the one-time plaintext token, shown
// once and never recoverable.
type adminCreateKeyResponse struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Owner       string   `json:"owner,omitempty"`
	Team        string   `json:"team,omitempty"`
	Token       string   `json:"token"`
	Roles       []string `json:"roles"`
	AdminScopes []string `json:"admin_scopes"`
	AllowModels []string `json:"allow_models"`
	DenyModels  []string `json:"deny_models"`
	Created     int64    `json:"created"`
	CreatedBy   string   `json:"created_by,omitempty"`
	ExpiresAt   *int64   `json:"expires_at,omitempty"`
}

// adminRotateKeyResponse is the POST /v1/admin/keys/{id}/rotate response: the key
// id and its new one-time plaintext token.
type adminRotateKeyResponse struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

// adminWorkerView is the per-worker projection returned by GET /v1/admin/workers.
// Models is flattened to model names; LastSeen is a unix timestamp.
type adminWorkerView struct {
	ID         string   `json:"id"`
	Models     []string `json:"models"`
	Status     string   `json:"status"`
	ActiveJobs uint32   `json:"active_jobs"`
	TotalVRAM  uint64   `json:"total_vram"`
	FreeVRAM   uint64   `json:"free_vram"`
	Load       uint32   `json:"load"`
	GPUType    string   `json:"gpu_type"`
	LastSeen   int64    `json:"last_seen"`
}

// adminWorkerDetailView is the richer per-worker projection returned by GET
// /v1/admin/workers/{id} (#93). It extends the list view's capacity/status fields
// with the registration timestamp and a derived uptime so an operator can inspect
// one worker fully. Draining is a convenience boolean (also implied by
// Status == "draining"). GPU detail is reported at the aggregate level the
// control plane tracks (GPUType + TotalVRAM/FreeVRAM); there is no per-GPU
// breakdown in the fleet snapshot. RegisteredAt/UptimeSeconds are omitted (0)
// when the worker has no recorded registration time.
type adminWorkerDetailView struct {
	ID            string   `json:"id"`
	Models        []string `json:"models"`
	Status        string   `json:"status"`
	Draining      bool     `json:"draining"`
	ActiveJobs    uint32   `json:"active_jobs"`
	TotalVRAM     uint64   `json:"total_vram"`
	FreeVRAM      uint64   `json:"free_vram"`
	Load          uint32   `json:"load"`
	GPUType       string   `json:"gpu_type"`
	LastSeen      int64    `json:"last_seen"`
	RegisteredAt  int64    `json:"registered_at,omitempty"`
	UptimeSeconds int64    `json:"uptime_seconds,omitempty"`
}

// newAdminWorkerDetailView projects a fleet snapshot into the detail view. Uptime
// is derived from RegisteredAt against the snapshot's LastSeen (the most recent
// server-observed time for the worker) rather than wall-clock now, so it stays
// consistent with the timestamps in the same response and needs no clock here; it
// is clamped at 0. Models is emitted as [] (never null).
func newAdminWorkerDetailView(wk types.Worker) adminWorkerDetailView {
	models := make([]string, len(wk.Models))
	for i, m := range wk.Models {
		models[i] = m.Name
	}
	v := adminWorkerDetailView{
		ID:         wk.ID,
		Models:     models,
		Status:     wk.Status.String(),
		Draining:   wk.Status == types.WorkerDraining,
		ActiveJobs: wk.ActiveJobs,
		TotalVRAM:  wk.TotalVRAM,
		FreeVRAM:   wk.FreeVRAM,
		Load:       wk.Load,
		GPUType:    wk.GPUType,
		LastSeen:   wk.LastSeen.Unix(),
	}
	if !wk.RegisteredAt.IsZero() {
		v.RegisteredAt = wk.RegisteredAt.Unix()
		if up := wk.LastSeen.Sub(wk.RegisteredAt); up > 0 {
			v.UptimeSeconds = int64(up.Seconds())
		}
	}
	return v
}

// adminStatsResponse is the GET /v1/admin/stats response (#10): a consolidated,
// operator-facing snapshot of queue depth, per-worker load, and the time-in-queue
// distribution. It is a live read (no caching) and contains no secrets. The
// Prometheus /metrics surface is deferred to #24; this endpoint is the
// human/JSON view over the same accessors.
type adminStatsResponse struct {
	Queue    adminQueueStats   `json:"queue"`
	Workers  []adminStatWorker `json:"workers"`
	WaitTime adminWaitTime     `json:"wait_time"`
}

// adminQueueStats is the queue-depth section: the total pending plus a
// per-priority breakdown keyed by priority name (only non-empty levels appear,
// mirroring queue.Stats.ByPriority).
type adminQueueStats struct {
	Total      int            `json:"total"`
	ByPriority map[string]int `json:"by_priority"`
}

// adminStatWorker is the per-worker load projection in the stats response: id,
// in-flight jobs, reported load, and status. It deliberately omits VRAM/model
// detail (available via GET /v1/admin/workers) to keep the operator dashboard
// focused on load.
type adminStatWorker struct {
	ID         string `json:"id"`
	ActiveJobs uint32 `json:"active_jobs"`
	Load       uint32 `json:"load"`
	Status     string `json:"status"`
}

// adminWaitTime is the time-in-queue section: count/sum/max/mean (ms) over jobs
// that were placed from the queue (the fast path is excluded), plus the
// cumulative le-bucketed histogram (trailing bucket is +Inf, le_ms == 0).
type adminWaitTime struct {
	Count   uint64            `json:"count"`
	SumMs   uint64            `json:"sum_ms"`
	MaxMs   uint64            `json:"max_ms"`
	MeanMs  uint64            `json:"mean_ms"`
	Buckets []adminWaitBucket `json:"buckets"`
}

// adminWaitBucket is one cumulative histogram bucket: the count of waits <= le_ms
// (le_ms == 0 is the +Inf bucket).
type adminWaitBucket struct {
	LeMs  uint64 `json:"le_ms"`
	Count uint64 `json:"count"`
}

// priorityName maps a queue.Priority to a stable JSON key for the by_priority
// breakdown. Unknown levels fall back to "normal" so a future priority never
// produces an unkeyed entry.
func priorityName(p queue.Priority) string {
	switch p {
	case queue.PriorityLow:
		return "low"
	case queue.PriorityHigh:
		return "high"
	default:
		return "normal"
	}
}

// handleAdminStats serves GET /v1/admin/stats. It reads queue depth, the fleet
// snapshot, and the time-in-queue distribution live (no caching) and returns
// them as one consolidated JSON document. Gated to the telemetry:read scope
// (s.requireScope), so a key lacking it gets 403 and an unauthenticated request
// 401 before this runs.
func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	qs := s.fleet.QueueStats()
	byPriority := make(map[string]int, len(qs.ByPriority))
	for p, n := range qs.ByPriority {
		byPriority[priorityName(p)] += n
	}

	fleet := s.fleet.Fleet()
	workers := make([]adminStatWorker, len(fleet))
	for i, wk := range fleet {
		workers[i] = adminStatWorker{
			ID:         wk.ID,
			ActiveJobs: wk.ActiveJobs,
			Load:       wk.Load,
			Status:     wk.Status.String(),
		}
	}

	wt := s.fleet.WaitTimeStats()
	var meanMs uint64
	if wt.Count > 0 {
		meanMs = wt.SumMs / wt.Count
	}
	buckets := make([]adminWaitBucket, len(wt.Buckets))
	for i, b := range wt.Buckets {
		buckets[i] = adminWaitBucket{LeMs: b.LeMs, Count: b.Count}
	}

	writeJSON(w, http.StatusOK, adminStatsResponse{
		Queue:   adminQueueStats{Total: qs.Total, ByPriority: byPriority},
		Workers: workers,
		WaitTime: adminWaitTime{
			Count:   wt.Count,
			SumMs:   wt.SumMs,
			MaxMs:   wt.MaxMs,
			MeanMs:  meanMs,
			Buckets: buckets,
		},
	})
}

// adminQuotaUsageResponse is the GET /v1/admin/keys/{id}/quota response: the
// key's current usage in each window versus its effective limits, plus when each
// window next resets (UTC unix timestamps). A limit of 0 means unlimited.
type adminQuotaUsageResponse struct {
	KeyID  string     `json:"key_id"`
	Limits limitsView `json:"limits"`

	RequestsThisMinute uint64 `json:"requests_this_minute"`
	TokensThisMinute   uint64 `json:"tokens_this_minute"`
	TokensToday        uint64 `json:"tokens_today"`
	TokensThisMonth    uint64 `json:"tokens_this_month"`

	MinuteResetsAt int64 `json:"minute_resets_at"`
	DayResetsAt    int64 `json:"day_resets_at"`
	MonthResetsAt  int64 `json:"month_resets_at"`
}

// ---- request shapes ----

// adminCreateKeyRequest is the POST /v1/admin/keys body. Name is a human label;
// the role, admin-scope, and allow/deny lists set the new key's initial
// permissions. Roles are validated against the known vocabulary (authz.ValidRole,
// #95) and AdminScopes grants the fine-grained management scopes (#90); an unknown
// role or scope is rejected with 400 (no key is created).
//
// Owner/Team are optional free-form labels (descriptive metadata only — they
// grant nothing). ExpiresAt is an optional TTL as unix seconds: after it the key
// fails authentication. It must be in the future; a past or zero ExpiresAt is
// rejected with 400 (a key born already-expired is almost certainly a mistake).
// Omit it for a non-expiring key — the pre-existing behavior (#96).
type adminCreateKeyRequest struct {
	Name        string   `json:"name"`
	Owner       string   `json:"owner,omitempty"`
	Team        string   `json:"team,omitempty"`
	Roles       []string `json:"roles"`
	AdminScopes []string `json:"admin_scopes"`
	AllowModels []string `json:"allow_models"`
	DenyModels  []string `json:"deny_models"`
	ExpiresAt   *int64   `json:"expires_at,omitempty"`
}

// adminPermissionsRequest is the PUT /v1/admin/keys/{id}/permissions body. It is
// a full replace (not a merge): the supplied lists become the key's roles,
// admin scopes, and allow/deny lists; an omitted/null list clears that
// dimension. Both the role names and the admin scopes are validated against the
// known vocabularies (authz.ValidRole / authz.ValidScope) BEFORE any mutation, so
// an unknown role or scope is rejected with 400 and the key is left unchanged
// (#95). GET /v1/admin/roles enumerates the valid roles + scopes for a GUI editor.
type adminPermissionsRequest struct {
	Roles       []string `json:"roles"`
	AdminScopes []string `json:"admin_scopes"`
	AllowModels []string `json:"allow_models"`
	DenyModels  []string `json:"deny_models"`
}

// adminQuotaRequest is the PUT /v1/admin/keys/{id}/quota body. Each field is a
// pointer so the handler can tell "set to 0 (unlimited for that dimension)" from
// "field omitted". When EVERY field is omitted/null the per-key override is
// cleared entirely (SetLimits(nil)) so the key falls back to the global defaults.
type adminQuotaRequest struct {
	RPM           *uint64 `json:"rpm"`
	TPM           *uint64 `json:"tpm"`
	DailyTokens   *uint64 `json:"daily_tokens"`
	MonthlyTokens *uint64 `json:"monthly_tokens"`
}

// adminDrainRequest is the optional POST /v1/admin/workers/{id}/drain body (#93).
// DeadlineSeconds, when > 0, turns the soft drain into a timed forced drain: the
// worker is evicted once its in-flight jobs finish OR the deadline elapses. An
// absent or 0 deadline (including an empty body) is the pure soft drain — the
// preserved original behavior. A negative value is rejected with 400.
type adminDrainRequest struct {
	DeadlineSeconds int64 `json:"deadline_seconds"`
}

// adminPullModelRequest is the POST /v1/admin/workers/{id}/models body (#93): the
// model to pull onto the worker. An empty Model is rejected with 400.
type adminPullModelRequest struct {
	Model string `json:"model"`
}

// ---- handlers ----

// handleAdminCreateKey serves POST /v1/admin/keys. It mints a new key with the
// requested permissions and returns its id and the one-time plaintext token.
func (s *Server) handleAdminCreateKey(w http.ResponseWriter, r *http.Request) {
	var req adminCreateKeyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validateRoles(w, req.Roles) {
		return
	}
	if !validateScopes(w, req.AdminScopes) {
		return
	}
	// An optional TTL is validated up front: it must be a positive unix timestamp
	// strictly in the future. A non-positive or already-past value is rejected
	// with 400 (and no key is created) rather than silently minting a key that can
	// never authenticate. An omitted expires_at leaves the key non-expiring.
	expiresAt, ok := parseExpiresAt(w, req.ExpiresAt)
	if !ok {
		return
	}
	// Record the creating actor (the authenticated admin key's id) for provenance.
	// Empty when the handler is invoked outside the auth middleware (some unit
	// tests), preserving the pre-existing behavior of an unattributed key.
	actor := ""
	if k, ok := keyFromContext(r.Context()); ok {
		actor = k.ID
	}
	token, key, err := s.auth.CreateWithPermissions(r.Context(), req.Name, auth.Permissions{
		Roles:       req.Roles,
		AdminScopes: req.AdminScopes,
		AllowModels: req.AllowModels,
		DenyModels:  req.DenyModels,
		Owner:       req.Owner,
		Team:        req.Team,
		ExpiresAt:   expiresAt,
		CreatedBy:   actor,
	})
	if err != nil {
		s.recordAudit(r, auditOpKeyCreate, "", audit.OutcomeFailure, nil, nil)
		s.reqLog(r.Context()).Error("admin create key failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "could not create key")
		return
	}
	// Audit the create: there is no Before (the key did not exist); the After is
	// the redacted projection of the new key. The one-time token is NEVER recorded.
	s.recordAudit(r, auditOpKeyCreate, key.ID, audit.OutcomeSuccess, nil, auditKeyValues(key))
	resp := adminCreateKeyResponse{
		ID:          key.ID,
		Name:        key.Name,
		Owner:       key.Owner,
		Team:        key.Team,
		Token:       token,
		Roles:       orEmpty(key.Roles),
		AdminScopes: orEmpty(key.AdminScopes),
		AllowModels: orEmpty(key.AllowModels),
		DenyModels:  orEmpty(key.DenyModels),
		Created:     key.CreatedAt.Unix(),
		CreatedBy:   key.CreatedBy,
	}
	if key.ExpiresAt != nil {
		exp := key.ExpiresAt.Unix()
		resp.ExpiresAt = &exp
	}
	writeJSON(w, http.StatusCreated, resp)
}

// parseExpiresAt validates the optional create-key TTL. A nil pointer (omitted
// field) means "no expiry" and yields (nil, true). A present value must be a
// positive unix-seconds timestamp strictly in the future; otherwise it writes a
// 400 and returns ok=false so the handler aborts before any key is minted. On
// success it returns the corresponding UTC *time.Time.
func parseExpiresAt(w http.ResponseWriter, expiresAt *int64) (*time.Time, bool) {
	if expiresAt == nil {
		return nil, true
	}
	if *expiresAt <= 0 || !time.Unix(*expiresAt, 0).After(time.Now()) {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "expires_at must be a unix timestamp in the future")
		return nil, false
	}
	t := time.Unix(*expiresAt, 0).UTC()
	return &t, true
}

// handleAdminListKeys serves GET /v1/admin/keys. It returns metadata for every
// key (never any secret), in the shared cursor-paginated list envelope
// ({"data":[...],"pagination":{...}}). Keys are stably sorted by id so a cursor
// names a stable position across requests. Honors ?limit= and ?cursor=.
func (s *Server) handleAdminListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.auth.List(r.Context())
	if err != nil {
		s.reqLog(r.Context()).Error("admin list keys failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "could not list keys")
		return
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	views := make([]adminKeyView, len(keys))
	for i, k := range keys {
		views[i] = newAdminKeyView(k)
	}
	limit, offset := parsePageParams(r)
	writeList(w, views, limit, offset)
}

// handleAdminGetKey serves GET /v1/admin/keys/{id}. It returns one key's
// metadata, or 404 if unknown.
func (s *Server) handleAdminGetKey(w http.ResponseWriter, r *http.Request) {
	key, err := s.auth.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeAdminKeyError(r, w, err)
		return
	}
	writeJSON(w, http.StatusOK, newAdminKeyView(key))
}

// handleAdminRevokeKey serves DELETE /v1/admin/keys/{id}. It revokes the key
// (subsequent authentication fails immediately) and returns 204, or 404 if
// unknown. The revoke is audited with the key's before/after state.
func (s *Server) handleAdminRevokeKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	before, _ := s.auth.Get(r.Context(), id)
	if err := s.auth.Revoke(r.Context(), id); err != nil {
		s.recordAudit(r, auditOpKeyRevoke, id, audit.OutcomeFailure, auditKeyValues(before), nil)
		s.writeAdminKeyError(r, w, err)
		return
	}
	after, _ := s.auth.Get(r.Context(), id)
	s.recordAudit(r, auditOpKeyRevoke, id, audit.OutcomeSuccess, auditKeyValues(before), auditKeyValues(after))
	w.WriteHeader(http.StatusNoContent)
}

// handleAdminRotateKey serves POST /v1/admin/keys/{id}/rotate. It replaces the
// key's secret (the old token stops verifying immediately) and returns the new
// one-time plaintext token, or 404 if unknown.
func (s *Server) handleAdminRotateKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	token, err := s.auth.Rotate(r.Context(), id)
	if err != nil {
		s.recordAudit(r, auditOpKeyRotate, id, audit.OutcomeFailure, nil, nil)
		s.writeAdminKeyError(r, w, err)
		return
	}
	// The rotation changes only the secret (which is never recorded), so there is
	// no meaningful before/after metadata to capture — the op + target + outcome
	// is the audit trail. The new token is NEVER recorded.
	s.recordAudit(r, auditOpKeyRotate, id, audit.OutcomeSuccess, nil, nil)
	writeJSON(w, http.StatusOK, adminRotateKeyResponse{ID: id, Token: token})
}

// handleAdminSetPermissions serves PUT /v1/admin/keys/{id}/permissions. It
// replaces the key's roles and allow/deny lists (full replace) and returns the
// updated metadata, or 404 if unknown. The change takes effect immediately.
func (s *Server) handleAdminSetPermissions(w http.ResponseWriter, r *http.Request) {
	var req adminPermissionsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// Validate both vocabularies BEFORE any mutation, so a rejected request (400)
	// leaves the key entirely unchanged (the full-replace SetPermissions never
	// runs). Roles are validated here (#95); admin scopes were validated since #90.
	if !validateRoles(w, req.Roles) {
		return
	}
	if !validateScopes(w, req.AdminScopes) {
		return
	}
	id := r.PathValue("id")
	before, _ := s.auth.Get(r.Context(), id)
	key, err := s.auth.SetPermissions(r.Context(), id, auth.Permissions{
		Roles:       req.Roles,
		AdminScopes: req.AdminScopes,
		AllowModels: req.AllowModels,
		DenyModels:  req.DenyModels,
	})
	if err != nil {
		s.recordAudit(r, auditOpKeyPermissions, id, audit.OutcomeFailure, auditKeyValues(before), nil)
		s.writeAdminKeyError(r, w, err)
		return
	}
	s.recordAudit(r, auditOpKeyPermissions, id, audit.OutcomeSuccess, auditKeyValues(before), auditKeyValues(key))
	writeJSON(w, http.StatusOK, newAdminKeyView(key))
}

// handleAdminSetQuota serves PUT /v1/admin/keys/{id}/quota. It sets the key's
// per-key quota override and returns the updated metadata, or 404 if unknown.
// When every field is omitted/null the override is cleared (the key falls back
// to the global defaults). The change takes effect immediately.
func (s *Server) handleAdminSetQuota(w http.ResponseWriter, r *http.Request) {
	var req adminQuotaRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	var limits *store.Limits
	// All fields omitted/null clears the override entirely; otherwise an omitted
	// dimension defaults to 0 ("unlimited for that dimension").
	if req.RPM != nil || req.TPM != nil || req.DailyTokens != nil || req.MonthlyTokens != nil {
		limits = &store.Limits{
			RPM:           deref(req.RPM),
			TPM:           deref(req.TPM),
			DailyTokens:   deref(req.DailyTokens),
			MonthlyTokens: deref(req.MonthlyTokens),
		}
	}
	id := r.PathValue("id")
	before, _ := s.auth.Get(r.Context(), id)
	key, err := s.auth.SetLimits(r.Context(), id, limits)
	if err != nil {
		s.recordAudit(r, auditOpKeyQuota, id, audit.OutcomeFailure, auditKeyValues(before), nil)
		s.writeAdminKeyError(r, w, err)
		return
	}
	s.recordAudit(r, auditOpKeyQuota, id, audit.OutcomeSuccess, auditKeyValues(before), auditKeyValues(key))
	writeJSON(w, http.StatusOK, newAdminKeyView(key))
}

// handleAdminGetQuota serves GET /v1/admin/keys/{id}/quota. It returns the key's
// current usage versus its effective limits, or 404 if unknown.
func (s *Server) handleAdminGetQuota(w http.ResponseWriter, r *http.Request) {
	if s.quota == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "quota is not enabled")
		return
	}
	key, err := s.auth.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeAdminKeyError(r, w, err)
		return
	}
	snap, err := s.quota.UsageForKey(r.Context(), key)
	if err != nil {
		s.reqLog(r.Context()).Error("admin quota usage failed", "key_id", key.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "could not read usage")
		return
	}
	writeJSON(w, http.StatusOK, newQuotaUsageResponse(snap))
}

// newQuotaUsageResponse projects a quota.Snapshot into its wire shape.
func newQuotaUsageResponse(s quota.Snapshot) adminQuotaUsageResponse {
	return adminQuotaUsageResponse{
		KeyID: s.KeyID,
		Limits: limitsView{
			RPM:           s.Limits.RPM,
			TPM:           s.Limits.TPM,
			DailyTokens:   s.Limits.DailyTokens,
			MonthlyTokens: s.Limits.MonthlyTokens,
		},
		RequestsThisMinute: s.RequestsThisMinute,
		TokensThisMinute:   s.TokensThisMinute,
		TokensToday:        s.TokensToday,
		TokensThisMonth:    s.TokensThisMonth,
		MinuteResetsAt:     s.MinuteResetsAt.Unix(),
		DayResetsAt:        s.DayResetsAt.Unix(),
		MonthResetsAt:      s.MonthResetsAt.Unix(),
	}
}

// handleAdminListWorkers serves GET /v1/admin/workers. It returns a point-in-time
// snapshot of every worker in the fleet, in the shared cursor-paginated list
// envelope. Workers are stably sorted by id so a cursor names a stable position.
// Honors ?limit= and ?cursor=.
func (s *Server) handleAdminListWorkers(w http.ResponseWriter, r *http.Request) {
	fleet := s.fleet.Fleet()
	sort.Slice(fleet, func(i, j int) bool { return fleet[i].ID < fleet[j].ID })
	views := make([]adminWorkerView, len(fleet))
	for i, wk := range fleet {
		models := make([]string, len(wk.Models))
		for j, m := range wk.Models {
			models[j] = m.Name
		}
		views[i] = adminWorkerView{
			ID:         wk.ID,
			Models:     models,
			Status:     wk.Status.String(),
			ActiveJobs: wk.ActiveJobs,
			TotalVRAM:  wk.TotalVRAM,
			FreeVRAM:   wk.FreeVRAM,
			Load:       wk.Load,
			GPUType:    wk.GPUType,
			LastSeen:   wk.LastSeen.Unix(),
		}
	}
	limit, offset := parsePageParams(r)
	writeList(w, views, limit, offset)
}

// handleAdminGetWorker serves GET /v1/admin/workers/{id} (#93). It returns the
// rich per-worker detail projection (models, status/draining, GPU type + VRAM
// aggregate, load, active jobs, last_seen, registered_at + derived uptime), or
// 404 if no such worker is connected. Gated to workers:read (s.requireScope).
func (s *Server) handleAdminGetWorker(w http.ResponseWriter, r *http.Request) {
	wk, ok := s.fleet.WorkerByID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "worker not found")
		return
	}
	writeJSON(w, http.StatusOK, newAdminWorkerDetailView(wk))
}

// handleAdminDrainWorker serves POST /v1/admin/workers/{id}/drain. It marks the
// worker draining (no new jobs; in-flight jobs finish) and returns 204, or 404 if
// no such worker is connected. The drain is audited.
//
// An optional body {"deadline_seconds": N} (#93) turns the soft drain into a
// timed forced drain: with N > 0 the worker is evicted once its in-flight jobs
// finish OR N seconds elapse, whichever first. An absent/empty body or N == 0 is
// the pure soft drain (preserved original behavior); a negative N is 400.
func (s *Server) handleAdminDrainWorker(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// The body is optional: an empty body is the pure soft drain. Decode only when
	// one is present, tolerating io.EOF (no body) but rejecting malformed JSON.
	var req adminDrainRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "malformed request body")
		return
	}
	if req.DeadlineSeconds < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "deadline_seconds must be >= 0")
		return
	}
	deadline := time.Duration(req.DeadlineSeconds) * time.Second

	if err := s.fleet.DrainWorkerWithDeadline(id, deadline); err != nil {
		s.recordAudit(r, auditOpWorkerDrain, id, audit.OutcomeFailure, nil, nil)
		if errors.Is(err, server.ErrWorkerNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "worker not found")
			return
		}
		s.reqLog(r.Context()).Error("admin drain worker failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "could not drain worker")
		return
	}
	// The after-snapshot records whether a forced deadline was attached, so the
	// audit trail distinguishes a soft drain from a timed/forced one.
	after := audit.RedactedValues{"status": "draining"}
	if deadline > 0 {
		after["deadline_seconds"] = req.DeadlineSeconds
	}
	s.recordAudit(r, auditOpWorkerDrain, id, audit.OutcomeSuccess, nil, after)
	w.WriteHeader(http.StatusNoContent)
}

// handleAdminPullModel serves POST /v1/admin/workers/{id}/models (#93). It
// dispatches a pull of the requested model onto the worker (fire-and-forget; the
// model surfaces on the worker's next heartbeat) and returns 202 Accepted, 404 if
// no such worker is connected, or 400 for an empty model. Gated to models:write
// (s.requireScopeWrite, which also layers idempotency); the pull is audited.
func (s *Server) handleAdminPullModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req adminPullModelRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	target := id + "/" + req.Model
	if err := s.fleet.AdminPullModel(r.Context(), id, req.Model); err != nil {
		s.recordAudit(r, auditOpModelPull, target, audit.OutcomeFailure, nil, nil)
		if errors.Is(err, server.ErrWorkerNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "worker not found")
			return
		}
		s.reqLog(r.Context()).Error("admin pull model failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "could not pull model")
		return
	}
	s.recordAudit(r, auditOpModelPull, target, audit.OutcomeSuccess, nil,
		audit.RedactedValues{"worker": id, "model": req.Model})
	// The pull is asynchronous on the worker; 202 communicates "accepted, not yet
	// complete" — the model appears in the worker detail after its next heartbeat.
	w.WriteHeader(http.StatusAccepted)
}

// handleAdminUnloadModel serves DELETE /v1/admin/workers/{id}/models/{model}
// (#93). It dispatches a best-effort unload of the model from the worker's Ollama
// and returns 204; a model that is not currently loaded is a worker-side no-op
// and still reported as success (the "missing model is success" contract). Returns
// 404 only when no such worker is connected. Gated to models:write
// (s.requireScopeWrite); the unload is audited.
func (s *Server) handleAdminUnloadModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	model := r.PathValue("model")
	target := id + "/" + model
	if err := s.fleet.AdminUnloadModel(r.Context(), id, model); err != nil {
		s.recordAudit(r, auditOpModelUnload, target, audit.OutcomeFailure, nil, nil)
		if errors.Is(err, server.ErrWorkerNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "worker not found")
			return
		}
		s.reqLog(r.Context()).Error("admin unload model failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "could not unload model")
		return
	}
	s.recordAudit(r, auditOpModelUnload, target, audit.OutcomeSuccess, nil,
		audit.RedactedValues{"worker": id, "model": model})
	w.WriteHeader(http.StatusNoContent)
}

// ---- helpers ----

// decodeJSON decodes the request body into v, writing a 400 and returning false
// on a malformed body. A handler returns immediately when it returns false.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "malformed request body")
		return false
	}
	return true
}

// deref returns *p, or 0 when p is nil.
func deref(p *uint64) uint64 {
	if p == nil {
		return 0
	}
	return *p
}

// validateScopes checks every requested admin scope against the known scope
// vocabulary (authz.ValidScope), writing a 400 and returning false on the first
// unknown scope so a key is never granted a string that gates nothing. An empty
// or nil set is valid (no scopes granted). A handler returns immediately when it
// returns false.
func validateScopes(w http.ResponseWriter, scopes []string) bool {
	for _, sc := range scopes {
		if !authz.ValidScope(sc) {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "unknown admin scope")
			return false
		}
	}
	return true
}

// validateRoles checks every requested role against the known role vocabulary
// (authz.ValidRole), writing a 400 and returning false on the first unknown role
// so a key is never assigned a role string that grants nothing (the editor GUI
// can enumerate the valid roles via GET /v1/admin/roles). An empty or nil set is
// valid (no roles granted — an opt-in, do-nothing key). It mirrors
// validateScopes; a handler returns immediately when it returns false.
func validateRoles(w http.ResponseWriter, roles []string) bool {
	for _, role := range roles {
		if !authz.ValidRole(role) {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "unknown role")
			return false
		}
	}
	return true
}

// writeAdminKeyError maps an auth/store error from a key operation to its HTTP
// status: store.ErrNotFound is 404 (unknown key id); anything else is a server
// fault (500) logged with the underlying cause via the request-scoped logger (so
// the line carries request_id). The message never echoes the key id or any
// secret.
func (s *Server) writeAdminKeyError(r *http.Request, w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "key not found")
		return
	}
	s.reqLog(r.Context()).Error("admin key operation failed", "err", err)
	writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
}
