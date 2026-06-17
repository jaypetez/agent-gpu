package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/queue"
	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/server"
	"github.com/jaypetez/agent-gpu/internal/store"
)

// The admin API (#4): admin-only HTTP endpoints to manage API keys (CRUD),
// per-key quotas, roles + per-model allow/deny lists, and to list/inspect/drain
// workers. Every route is gated by authMiddleware + adminMiddleware (registered
// via s.admin in httpapi.go), so a non-admin key gets 403 and an unauthenticated
// request 401 on every endpoint before any handler runs.
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
// key has never authenticated.
type adminKeyView struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Roles       []string    `json:"roles"`
	AllowModels []string    `json:"allow_models"`
	DenyModels  []string    `json:"deny_models"`
	Revoked     bool        `json:"revoked"`
	UsageCount  uint64      `json:"usage_count"`
	Created     int64       `json:"created"`
	LastUsed    int64       `json:"last_used,omitempty"`
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
		Roles:       orEmpty(k.Roles),
		AllowModels: orEmpty(k.AllowModels),
		DenyModels:  orEmpty(k.DenyModels),
		Revoked:     k.Revoked(),
		UsageCount:  k.UsageCount,
		Created:     k.CreatedAt.Unix(),
	}
	if !k.LastUsedAt.IsZero() {
		v.LastUsed = k.LastUsedAt.Unix()
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
	Token       string   `json:"token"`
	Roles       []string `json:"roles"`
	AllowModels []string `json:"allow_models"`
	DenyModels  []string `json:"deny_models"`
	Created     int64    `json:"created"`
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
// them as one consolidated JSON document. Gated to admin keys (s.admin), so a
// non-admin gets 403 and an unauthenticated request 401 before this runs.
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
// the role and allow/deny lists set the new key's initial permissions.
type adminCreateKeyRequest struct {
	Name        string   `json:"name"`
	Roles       []string `json:"roles"`
	AllowModels []string `json:"allow_models"`
	DenyModels  []string `json:"deny_models"`
}

// adminPermissionsRequest is the PUT /v1/admin/keys/{id}/permissions body. It is
// a full replace (not a merge): the supplied lists become the key's roles and
// allow/deny lists; an omitted/null list clears that dimension.
type adminPermissionsRequest struct {
	Roles       []string `json:"roles"`
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

// ---- handlers ----

// handleAdminCreateKey serves POST /v1/admin/keys. It mints a new key with the
// requested permissions and returns its id and the one-time plaintext token.
func (s *Server) handleAdminCreateKey(w http.ResponseWriter, r *http.Request) {
	var req adminCreateKeyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	token, key, err := s.auth.CreateWithPermissions(r.Context(), req.Name, auth.Permissions{
		Roles:       req.Roles,
		AllowModels: req.AllowModels,
		DenyModels:  req.DenyModels,
	})
	if err != nil {
		s.reqLog(r.Context()).Error("admin create key failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "could not create key")
		return
	}
	writeJSON(w, http.StatusCreated, adminCreateKeyResponse{
		ID:          key.ID,
		Name:        key.Name,
		Token:       token,
		Roles:       orEmpty(key.Roles),
		AllowModels: orEmpty(key.AllowModels),
		DenyModels:  orEmpty(key.DenyModels),
		Created:     key.CreatedAt.Unix(),
	})
}

// handleAdminListKeys serves GET /v1/admin/keys. It returns metadata for every
// key (never any secret).
func (s *Server) handleAdminListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.auth.List(r.Context())
	if err != nil {
		s.reqLog(r.Context()).Error("admin list keys failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "could not list keys")
		return
	}
	views := make([]adminKeyView, len(keys))
	for i, k := range keys {
		views[i] = newAdminKeyView(k)
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": views})
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
// unknown.
func (s *Server) handleAdminRevokeKey(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.Revoke(r.Context(), r.PathValue("id")); err != nil {
		s.writeAdminKeyError(r, w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAdminRotateKey serves POST /v1/admin/keys/{id}/rotate. It replaces the
// key's secret (the old token stops verifying immediately) and returns the new
// one-time plaintext token, or 404 if unknown.
func (s *Server) handleAdminRotateKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	token, err := s.auth.Rotate(r.Context(), id)
	if err != nil {
		s.writeAdminKeyError(r, w, err)
		return
	}
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
	key, err := s.auth.SetPermissions(r.Context(), r.PathValue("id"), auth.Permissions{
		Roles:       req.Roles,
		AllowModels: req.AllowModels,
		DenyModels:  req.DenyModels,
	})
	if err != nil {
		s.writeAdminKeyError(r, w, err)
		return
	}
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
	key, err := s.auth.SetLimits(r.Context(), r.PathValue("id"), limits)
	if err != nil {
		s.writeAdminKeyError(r, w, err)
		return
	}
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
// snapshot of every worker in the fleet.
func (s *Server) handleAdminListWorkers(w http.ResponseWriter, r *http.Request) {
	fleet := s.fleet.Fleet()
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
	writeJSON(w, http.StatusOK, map[string]any{"workers": views})
}

// handleAdminDrainWorker serves POST /v1/admin/workers/{id}/drain. It marks the
// worker draining (no new jobs; in-flight jobs finish) and returns 204, or 404 if
// no such worker is connected.
func (s *Server) handleAdminDrainWorker(w http.ResponseWriter, r *http.Request) {
	if err := s.fleet.DrainWorker(r.PathValue("id")); err != nil {
		if errors.Is(err, server.ErrWorkerNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "worker not found")
			return
		}
		s.reqLog(r.Context()).Error("admin drain worker failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "could not drain worker")
		return
	}
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
