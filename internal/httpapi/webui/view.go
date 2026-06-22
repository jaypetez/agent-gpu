// Package webui holds the agent-gpu admin console's server-rendered UI: the templ
// templates (compiled to *_templ.go, committed) and the view-model types the HTTP
// handlers populate and pass to them.
//
// The console is deliberately a thin, server-rendered shell (issue #100): templ
// for HTML, HTMX for partial updates, Alpine for small client interactions, and a
// Tailwind-built stylesheet — all served as embedded static assets so the shipped
// binary needs no Node and no CDN. The only third-party Go import the generated
// templates pull into the binary is github.com/a-h/templ's runtime root, which is
// stdlib-only (no third-party transitive deps), so the console adds no dependency
// weight to the release artifact.
//
// This file is hand-written Go (the view models + small helpers); the *_templ.go
// files alongside it are GENERATED from the *.templ sources by `make ui` (templ
// generate) and committed. Keeping the view models here — not in the templates —
// lets the httpapi handlers construct them and unit-test rendering without a
// browser.
package webui

// Section identifies one navigable area of the console. Each maps to a sidebar
// entry and (later issues) a screen; the dashboard shell (#100) renders the
// Overview screen and gates every section by the viewer's scopes.
type Section string

const (
	SectionOverview Section = "overview"
	SectionWorkers  Section = "workers"
	SectionKeys     Section = "keys"
	SectionUsage    Section = "usage"
	SectionLogs     Section = "logs"
	SectionAudit    Section = "audit"
	SectionConfig   Section = "config"
)

// NavEntry is one sidebar item: its target section, the human label, the path it
// links to, the read-scope a viewer must hold to see it (role-based IA), and the
// inline SVG glyph name. Visible reports whether the current viewer holds the
// scope — computed by the handler from the authenticated key — so the template
// renders only permitted sections (AC2/AC5).
type NavEntry struct {
	Section Section
	Label   string
	Href    string
	// Scope is the admin read-scope gating this section; empty means always shown.
	Scope string
	// Icon names the inline glyph drawn by the icon helper in the template.
	Icon string
	// Visible is set by the handler: true when the viewer may see this section.
	Visible bool
}

// Crumb is one breadcrumb segment. The last crumb is the current page (rendered
// non-interactive); earlier crumbs link back up the hierarchy.
type Crumb struct {
	Label string
	Href  string
}

// Viewer is the authenticated operator the shell renders for: an opaque key id
// (never the token), the roles held, and the resolved set of read-scopes used to
// gate the sidebar. It carries no secret.
type Viewer struct {
	KeyID   string
	Name    string
	Roles   []string
	IsAdmin bool
}

// ShellData is everything the layout chrome needs: who is viewing, which section
// is active (for aria-current), the role-filtered navigation, the breadcrumb
// trail, and the asset path prefix (so the same templates work whether assets are
// embedded at /admin/assets or served from --ui-path). Title is the document
// title suffix.
type ShellData struct {
	Viewer     Viewer
	Active     Section
	Nav        []NavEntry
	Crumbs     []Crumb
	Title      string
	AssetPath  string
	CSRFToken  string
	LiveStream bool
}

// LoginData backs the login page: the asset path prefix, the CSRF token the form
// posts back, an optional error message (shown on a failed attempt), and the
// destination to return to after a successful sign-in.
type LoginData struct {
	AssetPath string
	CSRFToken string
	Error     string
	Next      string
}

// DashboardData backs the Overview screen's initial server render. The live
// numbers are loaded by HTMX after first paint (so the shell is instant and the
// telemetry endpoint is hit once authenticated); this struct carries only what
// the first paint needs plus the asset/stream context inherited via ShellData.
type DashboardData struct {
	Shell ShellData
}

// KPI is one headline metric card on the dashboard: a short label, the formatted
// value, an optional unit, a status tone (ok/warn/danger/idle — conveyed by color
// AND the text label), and a one-line caption. It is populated from the telemetry
// JSON by the partial handler.
type KPI struct {
	Label   string
	Value   string
	Unit    string
	Tone    string
	Caption string
}

// WorkerRow is one row of the worker-health panel: the worker id, its status
// string, in-flight jobs, coarse load (0-100), and the status tone. Status is
// always shown as text next to the colored badge.
type WorkerRow struct {
	ID         string
	Status     string
	Tone       string
	ActiveJobs uint32
	Load       uint32
}

// QueueDepth is the queue-depth panel's data: the total pending plus the
// per-priority split, each rendered as a labeled bar.
type QueueDepth struct {
	Total  int
	High   int
	Normal int
	Low    int
}

// EventRow is one line in the event-stream panel: a timestamp, a level
// (info/warn/error → tone), and the message. It is sourced from the structured
// log query so the dashboard's event stream reuses the existing log ring.
type EventRow struct {
	Time    string
	Level   string
	Tone    string
	Message string
}

// WorkersData backs the Workers screen's initial server render (#101). The shell
// supplies the chrome; the live worker list + GPU heatmap are loaded by HTMX
// after first paint from their partials (so the page is instant and the data is
// always one fresh pull), then refreshed on a calm cadence.
type WorkersData struct {
	Shell ShellData
}

// WorkerListItem is one row of the live worker list (#101): the worker id, its
// lifecycle status shown as a text-labeled badge plus tone, in-flight jobs, coarse
// load (0-100), the reported GPU type, free/total VRAM rendered for humans, and a
// last-seen relative string. Status is ALWAYS conveyed by the text label beside
// the colored badge, never color alone.
type WorkerListItem struct {
	ID         string
	Status     string
	Tone       string
	ActiveJobs uint32
	Load       uint32
	GPUType    string
	VRAM       string
	LastSeen   string
}

// WorkerDetail backs the per-worker detail screen (#101). It is the rich
// projection an operator needs on one worker — the same fields GET
// /v1/admin/workers/{id} exposes — plus the derived presentation strings (status
// tone/word, human VRAM, uptime, last-seen) and the loaded model list the
// pull/unload controls act on. Draining gates which write controls are offered.
type WorkerDetail struct {
	ID         string
	Status     string
	Tone       string
	Draining   bool
	ActiveJobs uint32
	Load       uint32
	LoadTone   string
	GPUType    string
	TotalVRAM  string
	FreeVRAM   string
	UsedPct    int
	LastSeen   string
	Uptime     string
	Models     []string
	// LogsHref is the deep link to this worker's logs (the logs page lands in
	// #103); the detail screen offers it as a "View logs" affordance so an operator
	// reaches a stalled worker's logs within ~3 clicks from the dashboard (AC1).
	LogsHref string
}

// HeatCell is one cell of the GPU utilization heatmap (#101): one WORKER (the
// fleet snapshot tracks GPU capacity per worker, not per device — see admin_gpu.go).
// It carries the worker id, the coarse load 0-100, a load BAND (ok/watch/hot —
// the green<60 / yellow 60-85 / red>85 thresholds of AC2) conveyed by both a tone
// color AND a text label, the human VRAM usage, and the link to the worker's
// detail (one click from a cell, AC2).
type HeatCell struct {
	ID       string
	Load     uint32
	Band     string
	BandWord string
	Tone     string
	VRAM     string
	Href     string
}

// HeatmapData backs the GPU heatmap partial (#101): the per-worker cells plus the
// fleet roll-up an operator reads above the grid (worker count, mean/max load,
// free/total VRAM). It is computed from the same aggregation behind GET
// /v1/admin/gpus so the console and the API never disagree.
type HeatmapData struct {
	Cells       []HeatCell
	WorkerCount int
	MeanLoad    uint32
	MaxLoad     uint32
	MeanTone    string
	TotalVRAM   string
	FreeVRAM    string
}

// WorkerActionResult backs the inline toast a write action (drain/evict/pull/
// unload) swaps in on success or failure (#101). Status is conveyed by the tone
// AND the text, and the message is in the operator's voice. It is announced via
// the toast region's aria-live.
type WorkerActionResult struct {
	Tone    string
	Title   string
	Message string
}

// --- Keys / users / permissions (#102) --------------------------------------

// KeysData backs the API-keys screen's initial server render (#102). The shell
// supplies the chrome; the screen carries the masked key rows plus the role and
// admin-scope catalog (sourced from authz.AllRoles / authz.AllScopes) the create
// and permissions editors populate their pickers from — so a picker can never
// offer a role/scope the authorization engine doesn't recognize. No secret is
// ever carried here: the rows are masked projections and the one-time plaintext
// token is shown only in the reveal partial returned by create/rotate.
type KeysData struct {
	Shell ShellData
	Keys  []KeyRow
	Roles []RoleOption
	// AdminScopes is the full admin-scope vocabulary, for the scope picker.
	AdminScopes []string
}

// KeyRow is one row of the keys table (#102): the key's opaque id (NEVER the
// token — the secret is shown once at creation and never again), its name, the
// owner/team labels, the assigned roles + admin scopes, the created / last-used /
// expiry strings, and a status (active / revoked / expired) conveyed by BOTH a
// tone color AND a text word. MaskedSecret is a fixed visual placeholder making
// it explicit the table shows no secret; it is the same for every row.
type KeyRow struct {
	ID          string
	Name        string
	Owner       string
	Team        string
	Roles       []string
	AdminScopes []string
	// AllowModels / DenyModels prefill the permissions editor's model textareas so a
	// full-replace edit starts from the key's current lists rather than blank.
	AllowModels  []string
	DenyModels   []string
	MaskedSecret string
	Created      string
	LastUsed     string
	Expiry       string
	// Status is the lifecycle word shown beside the colored badge: "active",
	// "revoked", or "expired". Tone is its severity (ok/danger/warn).
	Status string
	Tone   string
	// Revoked / Expired gate which write controls the row offers (a revoked or
	// expired key cannot be rotated; a revoked key cannot be revoked again).
	Revoked bool
	Expired bool
}

// RoleOption is one selectable role in the create/permissions pickers (#102),
// projected from authz.RoleInfo: the role name (the value submitted) plus a short
// human description shown beside the checkbox so an operator understands what the
// role grants without leaving the editor.
type RoleOption struct {
	Name        string
	Description string
}

// KeyReveal backs the one-time token reveal returned by create and rotate (#102):
// the just-minted plaintext token shown exactly once, with a copy affordance, and
// never stored or shown again. Title/Message frame the reveal in the operator's
// voice (e.g. "Copy it now — it won't be shown again"). It is its own fragment so
// the create/rotate handlers can swap it into a modal/toast slot distinct from the
// masked table.
type KeyReveal struct {
	KeyID   string
	Name    string
	Token   string
	Title   string
	Message string
	// Rotated is true when the reveal is from a rotate (vs. a fresh create), so
	// the copy is phrased as a replacement rather than a new key.
	Rotated bool
}

// KeyActionResult backs the inline toast a key write (revoke / set-permissions)
// swaps into #toasts on success or failure (#102), mirroring WorkerActionResult.
// Status is conveyed by the tone AND the text.
type KeyActionResult struct {
	Tone    string
	Title   string
	Message string
}

// Section additions for the observability + settings screens (#103).
const (
	// SectionTelemetry is the live telemetry dashboard (request rate/latency,
	// throttles, fleet, sessions, affinity). It is its own section so the metrics
	// roll-up an operator watches is distinct from the task-centric Overview.
	SectionTelemetry Section = "telemetry"
)

// --- Usage (#103) -----------------------------------------------------------

// UsageData backs the Usage screen's initial server render (#103). The shell
// supplies the chrome; the per-key usage rows are loaded by HTMX after first paint
// from the usage partial (so the page is instant and the data is one fresh pull),
// then refreshed on a calm cadence. Enabled is false when the quota engine is not
// wired — the screen then renders a clear "usage reporting is off" notice rather
// than empty bars.
type UsageData struct {
	Shell ShellData
}

// UsageBoard backs the loaded usage partial (#103): the fleet-wide throttle summary
// over the per-key rows. Enabled mirrors the JSON endpoint's nil-quota gate (false
// → the engine is off and the board shows the disabled notice). KeyCount is the
// number of rows after any filter.
type UsageBoard struct {
	Enabled         bool
	KeyCount        int
	GlobalThrottled string
	KeyThrottled    string
	Rows            []UsageRow
}

// UsageRow is one key's usage row (#103): identity/labels plus the consumption-vs-
// limit METERS (daily/monthly token budgets and the per-minute request/token rate),
// a 7-day token SPARKLINE, and a best-effort exhaustion FORECAST. It is projected
// from the same per-key quota snapshot + series + forecast the JSON GET
// /v1/admin/usage row carries, so the console and the API never disagree. Every
// meter is rendered as a horizontal progress bar (never a pie) whose fill tone
// crosses the AC thresholds, and the forecast/severity is conveyed by BOTH a tone
// AND a text word.
type UsageRow struct {
	KeyID string
	Name  string
	Owner string
	Team  string

	// Meters are the consumption-vs-limit bars (daily tokens, monthly tokens, RPM,
	// TPM). A meter with no limit (0 = unlimited) renders as "no limit" rather than a
	// full or empty bar, so an unlimited dimension never reads as "at capacity".
	DailyTokens   UsageMeter
	MonthlyTokens UsageMeter
	Requests      UsageMeter
	TokensPerMin  UsageMeter

	// Spark is the 7-day daily-token sparkline: the polyline points string (an SVG
	// viewBox-relative path) plus whether there is enough history to draw it. Empty
	// when the series store is disabled or the key has no history.
	Spark Sparkline

	// Forecast is the exhaustion estimate: a human relative phrase ("~2d", "~14h")
	// for the soonest budget to run out, a tone for its urgency, and whether any
	// estimate exists. It is documented as an estimate, never an enforcement signal.
	Forecast UsageForecast
}

// UsageMeter is one consumption-vs-limit bar (#103). Used/Limit are the formatted
// human values; Pct is the integer fill percentage (0-100, clamped); Tone crosses
// the usage thresholds (ok < 75% < warn < 90% < danger); Limited is false when the
// dimension is unlimited (0 limit), in which case the bar renders as an informational
// "no limit" track rather than a meter. Label/Unit name the dimension for the
// aria-label and the row.
type UsageMeter struct {
	Label   string
	Used    string
	Limit   string
	Pct     int
	Tone    string
	Limited bool
}

// Sparkline is a tiny inline-SVG trend line (#103): Points is the polyline points
// attribute computed over a fixed 100x28 viewBox from the daily token series
// (oldest→newest), Has reports whether there were >= 2 points to draw a line, and
// Last/Peak are the formatted latest and peak day values for the accessible label.
// It is generated server-side (no client charting library, CSP-clean) and carries
// its own aria-label so it is not color/shape alone.
type Sparkline struct {
	Points string
	Has    bool
	Last   string
	Peak   string
}

// UsageForecast is a key's exhaustion estimate for the row (#103): Has reports
// whether any budget is projected to exhaust; Phrase is the human relative time to
// the soonest exhaustion ("~2d", "~14h", "< 1h"); Dimension names which budget
// ("daily" / "monthly"); Tone is the urgency (danger within a day, warn within a
// few). When Has is false the row shows a calm "no forecast" note.
type UsageForecast struct {
	Has       bool
	Phrase    string
	Dimension string
	Tone      string
}

// --- Telemetry (#103) -------------------------------------------------------

// TelemetryData backs the Telemetry screen's initial server render (#103). The
// shell supplies the chrome; the live metrics are loaded by HTMX after first paint
// from the telemetry partial and refreshed on a calm cadence (slower than the
// Overview board, since these are roll-ups, not the at-a-glance health strip).
type TelemetryData struct {
	Shell ShellData
}

// TelemetryBoard is the loaded telemetry partial's model (#103): the same dashboard
// summary GET /v1/admin/telemetry serves — request rate/latency, throttles, fleet
// health by status, queue depth, the time-in-queue and request-latency histograms,
// active sessions, and session affinity — projected for rendering. Every section is
// read from the in-process collectors the JSON endpoint reads, so the two agree.
type TelemetryBoard struct {
	KPIs     []KPI
	Latency  Histogram
	WaitTime Histogram
	Fleet    []TelemetryStatus
	Affinity TelemetryAffinity
}

// TelemetryStatus is one worker-status row of the fleet-health breakdown (#103):
// the status word (online/draining/stale), its tone, and the count. The status is
// always shown as text beside the tone, never color alone.
type TelemetryStatus struct {
	Status string
	Tone   string
	Count  int
}

// TelemetryAffinity is the session-affinity panel's model (#103): the hit/miss/
// rebind counts plus a derived hit-rate percentage and its tone, so an operator
// reads warm-worker reuse at a glance. The hit rate is reinforced by the raw counts.
type TelemetryAffinity struct {
	Hits     string
	Misses   string
	Rebinds  string
	HitRate  int
	RateTone string
	HasData  bool
}

// Histogram is a cumulative le-bucketed latency/wait distribution rendered as
// labeled horizontal bars (#103): a Count/Mean/Max summary over the per-bucket bars.
// Each bar's width is the bucket's share of the total, so the shape of the
// distribution reads without a charting library (CSP-clean). Empty when no
// observations have been recorded (the panel shows its calm empty state).
type Histogram struct {
	Count uint64
	MeanMs uint64
	MaxMs  uint64
	Bars   []HistogramBar
}

// HistogramBar is one bucket of a Histogram: the human bound label ("≤ 100ms",
// "> 10s" for +Inf), the count in that bucket, and the integer percentage width of
// the bar relative to the largest bucket.
type HistogramBar struct {
	Label string
	Count uint64
	Pct   int
}

// --- Logs (#103) ------------------------------------------------------------

// LogsData backs the Logs screen's initial server render (#103): the shell, the
// current filter state (so the form reflects a deep link like
// /admin/logs?worker=w1), the level options for the picker, and whether the log
// source is wired (Enabled false → the screen shows the "logging is off" notice).
// The filtered line table loads via HTMX from the logs partial; the live tail
// connects over SSE.
type LogsData struct {
	Shell    ShellData
	Enabled  bool
	Filter   LogFilterState
	Levels   []string
	// StreamURL is the SSE endpoint the live tail connects to (the cookie
	// authenticates it). It carries the active filters as a query string so the tail
	// shows only matching new lines.
	StreamURL string
	// CSVHref is the export link for the current filter (the JSON endpoint's
	// ?format=csv), so an operator downloads exactly what they filtered.
	CSVHref string
}

// LogFilterState is the parsed log-viewer filter reflected back into the form
// (#103): the minimum level and the request_id/session_id/worker exact-match
// filters plus the since/until window (datetime-local strings). Empty fields are
// "no filter" for that dimension. It mirrors the JSON endpoint's parseLogFilter
// semantics so the console and the API filter identically.
type LogFilterState struct {
	Level     string
	RequestID string
	SessionID string
	Worker    string
	Since     string
	Until     string
}

// LogsTable backs the loaded logs partial (#103): the filtered, newest-first lines
// plus the count shown. It is a point-in-time query over the in-memory ring (the
// same Snapshot+filter the JSON GET /v1/admin/logs uses); the live tail appends new
// lines client-side over SSE on top of this initial page.
type LogsTable struct {
	Lines []LogLine
	Count int
}

// LogLine is one structured log row for the viewer (#103): the timestamp, the level
// (with a tone), the human message, and the structured fields as DISCRETE badges
// (request_id/session_id/worker and any other attrs) — never embedded in the message
// text, so an operator scans fields without parsing prose. Fields are sorted for a
// stable render.
type LogLine struct {
	Time    string
	Level   string
	Tone    string
	Message string
	Fields  []LogField
}

// LogField is one structured attribute shown as a discrete key=value badge beside a
// log line (#103). Primary marks the first-class filter fields (request_id/
// session_id/worker) so they can be visually emphasized over incidental attrs.
type LogField struct {
	Key     string
	Value   string
	Primary bool
}

// --- Settings (#103) --------------------------------------------------------

// SettingsData backs the Settings screen (#103): the shell, the editable tunable
// fields grouped into tabs, the read-only boot fields, and whether the runtime-
// config holder is wired (Enabled false → the "settings are unavailable" notice).
// CanWrite gates whether the editor is interactive (the viewer holds config:write)
// or shown read-only with a note (config:read but not config:write).
type SettingsData struct {
	Shell    ShellData
	Enabled  bool
	CanWrite bool
	Tabs     []SettingsTab
	ReadOnly []SettingsField
}

// SettingsTab is one group of related tunables in the Settings editor (#103): a
// title and its fields. Grouping (General / Quotas / Sessions / Advanced) keeps the
// long flat config legible and lets an operator find a tunable by concern.
type SettingsTab struct {
	ID     string
	Title  string
	Fields []SettingsField
}

// SettingsField is one editable (or read-only) configuration field in the Settings
// editor (#103): the form field name (the config key), a human Label and Help
// string, the current Value, the input Kind (number / duration / text / a select
// with Options), and an optional Warning shown when the field is behavior-changing
// (a consequence note so an operator understands the live effect before applying).
// ReadOnly marks a boot-only field, rendered disabled with a "restart to change"
// note. Min is the lower bound for number inputs (the non-negative tunables).
type SettingsField struct {
	Name     string
	Label    string
	Help     string
	Value    string
	Kind     string
	Options  []string
	Warning  string
	ReadOnly bool
	Min      string
}

// SettingsResult backs the inline toast a settings PUT swaps in on success or
// failure (#103), mirroring KeyActionResult. Status is conveyed by the tone AND the
// text; an inline field error is rendered separately (see SettingsError).
type SettingsResult struct {
	Tone    string
	Title   string
	Message string
}

const (
	// FieldNumber is a non-negative integer input (counts, RPM/TPM, token budgets).
	FieldNumber = "number"
	// FieldDuration is a Go-duration text input (e.g. "30m", "45s").
	FieldDuration = "duration"
	// FieldText is a free-text input.
	FieldText = "text"
	// FieldSelect is a fixed-option picker (log level, overflow policy).
	FieldSelect = "select"
)
