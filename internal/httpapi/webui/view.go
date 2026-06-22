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
