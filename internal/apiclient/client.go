// Package apiclient is a small, dependency-free HTTP client for the agent-gpu
// admin and catalog API. It is what the `agentgpu` CLI uses to manage a RUNNING
// server (mint/revoke/rotate keys, set permissions and quotas, list the model
// catalog) so a change takes effect immediately — the server reads its in-memory
// store fresh on every request, unlike a separate process writing the on-disk
// store file, which the running server would not observe until a restart.
//
// The client targets the public HTTP API (default 127.0.0.1:8080), NOT the gRPC
// control plane between server and workers. It authenticates with an admin
// Bearer token (agpu_<id>_<secret>); every admin endpoint requires the admin
// role, so a non-admin token gets a typed ErrForbidden.
//
// Wire shapes are decoded from the same JSON the server emits (see
// internal/httpapi/admin.go and models.go); the request/response structs here
// mirror those field-for-field. HTTP status codes map to the typed errors below
// (ErrUnauthorized, ErrForbidden, ErrNotFound, ErrRateLimited, plus a generic
// server error) so the CLI can branch on them for distinct exit codes, and the
// server's {"error":{message,code}} envelope is decoded for the human message.
//
// The client is deliberately injectable — a base URL plus an *http.Client — so
// tests drive it against an httptest.Server with no real network or server.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors for the HTTP status classes the CLI branches on. Each is
// wrapped with the decoded server message (APIError below) so callers can both
// errors.Is the class and surface the detail. They map to distinct CLI exit
// codes in cmd/agentgpu.
var (
	// ErrUnauthorized is a 401: the token is missing, malformed, or invalid.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrForbidden is a 403: authenticated but lacking the admin role.
	ErrForbidden = errors.New("forbidden")
	// ErrNotFound is a 404: no such key (or other resource).
	ErrNotFound = errors.New("not found")
	// ErrRateLimited is a 429: the request was throttled. Server errors (5xx)
	// surface as a plain APIError.
	ErrRateLimited = errors.New("rate limited")
	// ErrServer is any 5xx: the server failed to handle an otherwise valid request.
	ErrServer = errors.New("server error")
)

// APIError carries the HTTP status, the decoded error code/message from the
// server's {"error":{message,code}} envelope, and the sentinel for its status
// class (so errors.Is(err, ErrNotFound) works). It is what every non-2xx
// response becomes.
type APIError struct {
	Status  int
	Code    string
	Message string
	// class is the sentinel for the status class, exposed via Unwrap so callers
	// can errors.Is against ErrUnauthorized/ErrForbidden/ErrNotFound/etc.
	class error
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.class != nil {
		return e.class.Error()
	}
	return fmt.Sprintf("http %d", e.Status)
}

// Unwrap exposes the status-class sentinel so errors.Is(err, ErrNotFound) and
// friends report true for the matching class.
func (e *APIError) Unwrap() error { return e.class }

// defaultTimeout bounds a single CLI request. Admin operations are quick; this
// keeps a hung server from wedging the command indefinitely.
const defaultTimeout = 30 * time.Second

// Client talks to the agent-gpu HTTP admin + catalog API. Construct it with New;
// the zero value is not usable.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient overrides the underlying *http.Client (for tests, custom
// timeouts, or proxies). When unset, a client with defaultTimeout is used.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// New returns a Client for baseURL authenticating with token. A trailing slash
// on baseURL is trimmed so path joining is unambiguous. The default HTTP client
// applies defaultTimeout; override it with WithHTTPClient.
func New(baseURL, token string, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: defaultTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// ---- wire shapes (mirror internal/httpapi/admin.go and models.go) ----

// KeyView is the metadata-only projection of a key returned by the admin API
// (GET /v1/admin/keys, GET /v1/admin/keys/{id}, and the body of permission/quota
// updates). It never carries a secret. It mirrors httpapi.adminKeyView.
type KeyView struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Roles       []string `json:"roles"`
	AdminScopes []string `json:"admin_scopes"`
	AllowModels []string `json:"allow_models"`
	DenyModels  []string `json:"deny_models"`
	Revoked     bool     `json:"revoked"`
	UsageCount  uint64   `json:"usage_count"`
	Created     int64    `json:"created"`
	LastUsed    int64    `json:"last_used,omitempty"`
	Limits      *Limits  `json:"limits,omitempty"`
}

// Limits mirrors httpapi.limitsView / store.Limits: a per-key quota override. A
// zero field means "unlimited" for that dimension.
type Limits struct {
	RPM           uint64 `json:"rpm"`
	TPM           uint64 `json:"tpm"`
	DailyTokens   uint64 `json:"daily_tokens"`
	MonthlyTokens uint64 `json:"monthly_tokens"`
}

// CreateKeyResponse is the POST /v1/admin/keys response. Token is the one-time
// plaintext token — shown once, never recoverable. Mirrors
// httpapi.adminCreateKeyResponse.
type CreateKeyResponse struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Token       string   `json:"token"`
	Roles       []string `json:"roles"`
	AdminScopes []string `json:"admin_scopes"`
	AllowModels []string `json:"allow_models"`
	DenyModels  []string `json:"deny_models"`
	Created     int64    `json:"created"`
}

// RotateKeyResponse is the POST /v1/admin/keys/{id}/rotate response: the key id
// and its new one-time plaintext token. Mirrors httpapi.adminRotateKeyResponse.
type RotateKeyResponse struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

// QuotaUsage is the GET /v1/admin/keys/{id}/quota response: current usage per
// window versus effective limits, with reset timestamps (unix seconds). Mirrors
// httpapi.adminQuotaUsageResponse.
type QuotaUsage struct {
	KeyID  string `json:"key_id"`
	Limits Limits `json:"limits"`

	RequestsThisMinute uint64 `json:"requests_this_minute"`
	TokensThisMinute   uint64 `json:"tokens_this_minute"`
	TokensToday        uint64 `json:"tokens_today"`
	TokensThisMonth    uint64 `json:"tokens_this_month"`

	MinuteResetsAt int64 `json:"minute_resets_at"`
	DayResetsAt    int64 `json:"day_resets_at"`
	MonthResetsAt  int64 `json:"month_resets_at"`
}

// Model is one entry of the richer GET /models catalog: the model name, its
// digest, and the Online workers serving it. Mirrors httpapi.modelEntry.
type Model struct {
	Name        string   `json:"name"`
	Digest      string   `json:"digest"`
	WorkerCount int      `json:"worker_count"`
	Workers     []string `json:"workers"`
}

// Worker is one entry of GET /v1/admin/workers. Mirrors httpapi.adminWorkerView.
type Worker struct {
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

// WorkerDetail is the GET /v1/admin/workers/{id} response: the list view's fields
// plus the registration time and a derived uptime. Mirrors
// httpapi.adminWorkerDetailView. GPU capacity is the aggregate the control plane
// tracks (no per-GPU breakdown). RegisteredAt/UptimeSeconds are 0 when no
// registration time is recorded.
type WorkerDetail struct {
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

// FleetCapacity is the GET /v1/admin/gpus response: an aggregated, read-only GPU
// capacity inventory derived from the fleet heartbeat snapshot (no probing). It
// mirrors httpapi.adminGPUsResponse. Fleet is the roll-up across all workers,
// ByType groups workers by their reported GPU type, and Workers is the per-worker
// (not per-device) heatmap cells. The slices are never nil (empty for an empty
// fleet).
type FleetCapacity struct {
	Fleet   FleetCapacitySummary `json:"fleet"`
	ByType  []GPUTypeCapacity    `json:"by_type"`
	Workers []GPUWorkerCell      `json:"workers"`
}

// FleetCapacitySummary is the fleet roll-up section of FleetCapacity: worker
// count, summed total/free VRAM (bytes), and the mean/max coarse 0-100 load over
// the fleet. Mirrors httpapi.adminGPUFleet. Mean/Max are 0 for an empty fleet.
type FleetCapacitySummary struct {
	WorkerCount int    `json:"worker_count"`
	TotalVRAM   uint64 `json:"total_vram"`
	FreeVRAM    uint64 `json:"free_vram"`
	MeanLoad    uint32 `json:"mean_load"`
	MaxLoad     uint32 `json:"max_load"`
}

// GPUTypeCapacity is one by-GPU-type row of FleetCapacity: the reported GPU type
// string, how many workers report it, and the summed total/free VRAM for that
// type. Mirrors httpapi.adminGPUByType. The type string is the worker's reported
// value verbatim (e.g. "cpu" for a GPU-less worker); it is not parsed for device
// counts.
type GPUTypeCapacity struct {
	GPUType     string `json:"gpu_type"`
	WorkerCount int    `json:"worker_count"`
	TotalVRAM   uint64 `json:"total_vram"`
	FreeVRAM    uint64 `json:"free_vram"`
}

// GPUWorkerCell is one heatmap cell of FleetCapacity: a single worker's
// capacity/utilization (per worker, not per physical GPU). Mirrors
// httpapi.adminGPUCell.
type GPUWorkerCell struct {
	ID         string `json:"id"`
	GPUType    string `json:"gpu_type"`
	TotalVRAM  uint64 `json:"total_vram"`
	FreeVRAM   uint64 `json:"free_vram"`
	Load       uint32 `json:"load"`
	Status     string `json:"status"`
	ActiveJobs uint32 `json:"active_jobs"`
}

// AuditEntry is one record of the admin audit log (GET /v1/admin/audit): who did
// it (Actor key id), what (Op), to which resource (Target), the redacted
// before/after metadata projection of the affected object, the request
// correlation id, and the outcome. It mirrors httpapi/audit.Entry field-for-field
// and, like the server-side record, NEVER carries secret material (the
// before/after maps hold only safe metadata fields). Time is RFC3339; Before and
// After are absent for operations with no prior/posterior object.
type AuditEntry struct {
	Time      time.Time      `json:"time"`
	Actor     string         `json:"actor"`
	Op        string         `json:"op"`
	Target    string         `json:"target"`
	Before    map[string]any `json:"before,omitempty"`
	After     map[string]any `json:"after,omitempty"`
	RequestID string         `json:"request_id"`
	Outcome   string         `json:"outcome"`
}

// AuditFilter narrows a ListAudit query. Every field is optional; a zero
// AuditFilter requests the whole log. Non-empty string fields and non-zero time
// bounds are ANDed by the server, matching httpapi/audit.Filter: Since is
// inclusive, Until exclusive (a half-open window). The time bounds are sent as
// unix seconds (the admin API's timestamp convention); a zero time omits that
// bound.
type AuditFilter struct {
	// Actor, if non-empty, matches entries by exactly this actor key id.
	Actor string
	// Op, if non-empty, matches entries with exactly this operation name.
	Op string
	// Target, if non-empty, matches entries acting on exactly this resource id.
	Target string
	// Since, if non-zero, excludes entries recorded before it (inclusive bound).
	Since time.Time
	// Until, if non-zero, excludes entries recorded at or after it (exclusive bound).
	Until time.Time
}

// listEnvelope is the shared cursor-paginated list response shape every admin
// list endpoint returns ({"data":[...],"pagination":{...}}). It mirrors
// httpapi.listEnvelope. The client follows next_cursor to assemble the full list.
type listEnvelope[T any] struct {
	Data       []T `json:"data"`
	Pagination struct {
		NextCursor *string `json:"next_cursor"`
		HasMore    bool    `json:"has_more"`
	} `json:"pagination"`
}

// maxPageSize mirrors the server's maximum page size (httpapi.maxPageSize); the
// list methods request it to minimize round-trips while following the cursor.
const maxPageSize = 200

// CreateKeyRequest is the POST /v1/admin/keys body. Mirrors
// httpapi.adminCreateKeyRequest.
type CreateKeyRequest struct {
	Name        string   `json:"name"`
	Roles       []string `json:"roles,omitempty"`
	AdminScopes []string `json:"admin_scopes,omitempty"`
	AllowModels []string `json:"allow_models,omitempty"`
	DenyModels  []string `json:"deny_models,omitempty"`
}

// PermissionsRequest is the PUT /v1/admin/keys/{id}/permissions body — a full
// replace of roles, admin scopes, and allow/deny lists. Mirrors
// httpapi.adminPermissionsRequest.
type PermissionsRequest struct {
	Roles       []string `json:"roles"`
	AdminScopes []string `json:"admin_scopes,omitempty"`
	AllowModels []string `json:"allow_models"`
	DenyModels  []string `json:"deny_models"`
}

// QuotaRequest is the PUT /v1/admin/keys/{id}/quota body. Each field is a pointer
// so the server distinguishes "set to 0 (unlimited dimension)" from "omitted";
// when EVERY field is nil the per-key override is cleared and the key falls back
// to the global defaults. Mirrors httpapi.adminQuotaRequest.
type QuotaRequest struct {
	RPM           *uint64 `json:"rpm,omitempty"`
	TPM           *uint64 `json:"tpm,omitempty"`
	DailyTokens   *uint64 `json:"daily_tokens,omitempty"`
	MonthlyTokens *uint64 `json:"monthly_tokens,omitempty"`
}

// drainWorkerRequest is the optional POST /v1/admin/workers/{id}/drain body.
// DeadlineSeconds > 0 requests a timed forced drain; it is omitted (a pure soft
// drain) when zero. Mirrors httpapi.adminDrainRequest.
type drainWorkerRequest struct {
	DeadlineSeconds int64 `json:"deadline_seconds,omitempty"`
}

// pullModelRequest is the POST /v1/admin/workers/{id}/models body. Mirrors
// httpapi.adminPullModelRequest.
type pullModelRequest struct {
	Model string `json:"model"`
}

// ---- methods ----

// CreateKey mints a new key with the given permissions and returns its id and
// one-time plaintext token (POST /v1/admin/keys).
func (c *Client) CreateKey(ctx context.Context, req CreateKeyRequest) (CreateKeyResponse, error) {
	var out CreateKeyResponse
	err := c.do(ctx, http.MethodPost, "/v1/admin/keys", req, &out)
	return out, err
}

// ListKeys returns metadata for every key (GET /v1/admin/keys). No secrets. The
// server returns the cursor-paginated list envelope ({"data":[...],
// "pagination":{...}}); this method requests the maximum page size and follows
// the next_cursor until exhausted, so the CLI sees the full keyset as before.
func (c *Client) ListKeys(ctx context.Context) ([]KeyView, error) {
	var all []KeyView
	cursor := ""
	for {
		var out listEnvelope[KeyView]
		path := "/v1/admin/keys?limit=" + strconv.Itoa(maxPageSize)
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Data...)
		if out.Pagination.NextCursor == nil {
			return all, nil
		}
		cursor = *out.Pagination.NextCursor
	}
}

// GetKey returns one key's metadata (GET /v1/admin/keys/{id}), or ErrNotFound.
func (c *Client) GetKey(ctx context.Context, id string) (KeyView, error) {
	var out KeyView
	err := c.do(ctx, http.MethodGet, "/v1/admin/keys/"+id, nil, &out)
	return out, err
}

// RevokeKey revokes the key (DELETE /v1/admin/keys/{id}); subsequent
// authentication fails immediately. Returns ErrNotFound for an unknown id.
func (c *Client) RevokeKey(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/admin/keys/"+id, nil, nil)
}

// RotateKey replaces the key's secret and returns the new one-time plaintext
// token (POST /v1/admin/keys/{id}/rotate). The old token stops working at once.
func (c *Client) RotateKey(ctx context.Context, id string) (RotateKeyResponse, error) {
	var out RotateKeyResponse
	err := c.do(ctx, http.MethodPost, "/v1/admin/keys/"+id+"/rotate", nil, &out)
	return out, err
}

// SetPermissions replaces a key's roles and allow/deny lists (PUT
// /v1/admin/keys/{id}/permissions) and returns the updated metadata.
func (c *Client) SetPermissions(ctx context.Context, id string, req PermissionsRequest) (KeyView, error) {
	var out KeyView
	err := c.do(ctx, http.MethodPut, "/v1/admin/keys/"+id+"/permissions", req, &out)
	return out, err
}

// SetQuota sets (or, with an all-nil request, clears) a key's per-key quota
// override (PUT /v1/admin/keys/{id}/quota) and returns the updated metadata. The
// change is enforced immediately.
func (c *Client) SetQuota(ctx context.Context, id string, req QuotaRequest) (KeyView, error) {
	var out KeyView
	err := c.do(ctx, http.MethodPut, "/v1/admin/keys/"+id+"/quota", req, &out)
	return out, err
}

// GetQuota returns the key's current usage versus its effective limits (GET
// /v1/admin/keys/{id}/quota).
func (c *Client) GetQuota(ctx context.Context, id string) (QuotaUsage, error) {
	var out QuotaUsage
	err := c.do(ctx, http.MethodGet, "/v1/admin/keys/"+id+"/quota", nil, &out)
	return out, err
}

// ListModels returns the permission-filtered model catalog visible to the
// client's token (GET /models — the richer shape with digest + worker count).
func (c *Client) ListModels(ctx context.Context) ([]Model, error) {
	var out struct {
		Models []Model `json:"models"`
	}
	if err := c.do(ctx, http.MethodGet, "/models", nil, &out); err != nil {
		return nil, err
	}
	return out.Models, nil
}

// ListWorkers returns a point-in-time snapshot of the fleet (GET
// /v1/admin/workers). Like ListKeys, the server returns the cursor-paginated
// list envelope; this method requests the maximum page size and follows the
// next_cursor until exhausted so the CLI sees the whole fleet.
func (c *Client) ListWorkers(ctx context.Context) ([]Worker, error) {
	var all []Worker
	cursor := ""
	for {
		var out listEnvelope[Worker]
		path := "/v1/admin/workers?limit=" + strconv.Itoa(maxPageSize)
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Data...)
		if out.Pagination.NextCursor == nil {
			return all, nil
		}
		cursor = *out.Pagination.NextCursor
	}
}

// WorkerDetail returns the rich per-worker detail (GET /v1/admin/workers/{id}),
// or ErrNotFound if no such worker is connected.
func (c *Client) WorkerDetail(ctx context.Context, id string) (WorkerDetail, error) {
	var out WorkerDetail
	err := c.do(ctx, http.MethodGet, "/v1/admin/workers/"+url.PathEscape(id), nil, &out)
	return out, err
}

// FleetCapacity returns the aggregated GPU/fleet capacity inventory (GET
// /v1/admin/gpus): the fleet roll-up (worker count, summed total/free VRAM,
// mean/max load), the by-GPU-type grouping, and the per-worker heatmap cells. It
// is a live read over the heartbeat snapshot — no GPU probing — so it reflects the
// fleet as of the call.
func (c *Client) FleetCapacity(ctx context.Context) (FleetCapacity, error) {
	var out FleetCapacity
	err := c.do(ctx, http.MethodGet, "/v1/admin/gpus", nil, &out)
	return out, err
}

// DrainWorker drains the worker (POST /v1/admin/workers/{id}/drain): it stops
// receiving new jobs while its in-flight jobs finish. A deadline > 0 requests a
// timed forced drain — the worker is evicted once its in-flight jobs finish or
// the deadline elapses, whichever first; a deadline <= 0 is the pure soft drain.
// Returns ErrNotFound if no such worker is connected.
func (c *Client) DrainWorker(ctx context.Context, id string, deadline time.Duration) error {
	var body any
	if deadline > 0 {
		// Round up sub-second deadlines to one second so a small positive duration
		// still requests a forced drain rather than degrading to a soft drain.
		secs := int64(deadline / time.Second)
		if secs == 0 {
			secs = 1
		}
		body = drainWorkerRequest{DeadlineSeconds: secs}
	}
	return c.do(ctx, http.MethodPost, "/v1/admin/workers/"+url.PathEscape(id)+"/drain", body, nil)
}

// PullModel instructs the worker to pull model onto its local Ollama (POST
// /v1/admin/workers/{id}/models). It is fire-and-forget: the server accepts the
// request (202) and the model surfaces on the worker's next heartbeat once the
// pull completes. Returns ErrNotFound if no such worker is connected.
func (c *Client) PullModel(ctx context.Context, workerID, model string) error {
	return c.do(ctx, http.MethodPost, "/v1/admin/workers/"+url.PathEscape(workerID)+"/models",
		pullModelRequest{Model: model}, nil)
}

// UnloadModel asks the worker to unload model from its Ollama, freeing the VRAM
// (DELETE /v1/admin/workers/{id}/models/{model}). A model that is not currently
// loaded is a no-op and still reported as success. Returns ErrNotFound only when
// no such worker is connected.
func (c *Client) UnloadModel(ctx context.Context, workerID, model string) error {
	return c.do(ctx, http.MethodDelete,
		"/v1/admin/workers/"+url.PathEscape(workerID)+"/models/"+modelPathEscape(model), nil, nil)
}

// modelPathEscape escapes a model id for use as a path segment while preserving
// the '/' in a namespaced model id (e.g. "library/llama3"): the server's
// multi-segment wildcard captures the remainder of the path whole, so an embedded
// slash must stay a literal separator rather than be percent-encoded. Other
// reserved characters (notably the ':' in a colon tag like "qwen2:0.5b") are
// valid unescaped in a path segment and are left as-is.
func modelPathEscape(model string) string {
	parts := strings.Split(model, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// ListAudit returns the admin audit-log entries matching filter (GET
// /v1/admin/audit), newest first. Like ListKeys/ListWorkers the server returns
// the cursor-paginated list envelope; this method requests the maximum page size
// and follows the next_cursor until exhausted, so the caller sees the full
// matching set. The filter's string fields and (as unix seconds) its time bounds
// are sent as query parameters; the entries never carry secret material.
func (c *Client) ListAudit(ctx context.Context, filter AuditFilter) ([]AuditEntry, error) {
	base := auditQuery(filter)
	var all []AuditEntry
	cursor := ""
	for {
		var out listEnvelope[AuditEntry]
		path := "/v1/admin/audit?limit=" + strconv.Itoa(maxPageSize) + base
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Data...)
		if out.Pagination.NextCursor == nil {
			return all, nil
		}
		cursor = *out.Pagination.NextCursor
	}
}

// auditQuery renders an AuditFilter into the query-string suffix (each piece
// prefixed with "&", to follow the leading "?limit=") for ListAudit. Empty string
// fields and zero time bounds are omitted; the time bounds are encoded as unix
// seconds to match the server's parsing.
func auditQuery(f AuditFilter) string {
	q := url.Values{}
	if f.Actor != "" {
		q.Set("actor", f.Actor)
	}
	if f.Op != "" {
		q.Set("op", f.Op)
	}
	if f.Target != "" {
		q.Set("target", f.Target)
	}
	if !f.Since.IsZero() {
		q.Set("since", strconv.FormatInt(f.Since.Unix(), 10))
	}
	if !f.Until.IsZero() {
		q.Set("until", strconv.FormatInt(f.Until.Unix(), 10))
	}
	if len(q) == 0 {
		return ""
	}
	return "&" + q.Encode()
}

// Get performs a raw authenticated GET against path and decodes the JSON body
// into out. It is the seam the `--json`/`--openai` flags use to pass server JSON
// through verbatim (e.g. GET /v1/models in OpenAI shape) without a typed struct.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// ---- transport ----

// do executes an authenticated request: it JSON-encodes body (when non-nil),
// sets the Bearer token, decodes a 2xx JSON body into out (when non-nil), and
// maps a non-2xx response to a typed *APIError via the decoded error envelope.
// A transport-level failure (DNS, connection refused, timeout) is returned
// wrapped so the CLI can map it to the network exit code.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// Transport failure (unreachable server, timeout, TLS). Surfaced distinctly
		// so the CLI maps it to the network exit code rather than a generic error.
		return fmt.Errorf("request %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseError(resp)
	}

	if out == nil {
		// Drain so the connection can be reused; ignore content (e.g. 204).
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// parseError builds a typed *APIError from a non-2xx response: it decodes the
// {"error":{message,code}} envelope (best-effort — a non-JSON body still yields a
// useful error) and attaches the status-class sentinel for errors.Is.
func parseError(resp *http.Response) error {
	var env struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = json.Unmarshal(body, &env)

	apiErr := &APIError{
		Status:  resp.StatusCode,
		Code:    env.Error.Code,
		Message: env.Error.Message,
		class:   classify(resp.StatusCode),
	}
	if apiErr.Message == "" {
		// Fall back to the raw body (trimmed) when no envelope message was present,
		// so the user still sees something actionable.
		if trimmed := strings.TrimSpace(string(body)); trimmed != "" {
			apiErr.Message = trimmed
		}
	}
	return apiErr
}

// classify maps an HTTP status to its sentinel error class, or nil for an
// unrecognized non-2xx (which still surfaces as an *APIError with its status).
func classify(status int) error {
	switch {
	case status == http.StatusUnauthorized:
		return ErrUnauthorized
	case status == http.StatusForbidden:
		return ErrForbidden
	case status == http.StatusNotFound:
		return ErrNotFound
	case status == http.StatusTooManyRequests:
		return ErrRateLimited
	case status >= 500:
		return ErrServer
	default:
		return nil
	}
}
