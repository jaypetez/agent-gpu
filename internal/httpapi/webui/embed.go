package webui

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
)

// assetsFS embeds the console's static assets — the Tailwind-built stylesheet
// (app.css), the vendored, self-hosted HTMX + Alpine bundles, and the favicon — so
// the shipped binary serves the whole UI with no Node, no CDN, and no filesystem
// dependency (issue #100 AC1/AC7). The CSS is a committed build artifact (see
// `make ui`); a release `go build` consumes it directly, never regenerating.
//
// Note: app.css MUST exist at compile time for this directive to succeed. It is
// committed to the repo and rebuilt by `make ui`; if you add a brand-new checkout
// without it, run `make ui` (or `cd ui && npm run build:css`) once before building.
//
//go:embed all:assets
var assetsFS embed.FS

// Assets returns the embedded asset tree rooted at the assets directory, so a
// caller mounts it at the console's asset path and a request for
// "/admin/assets/css/app.css" maps to "assets/css/app.css" in the binary.
func Assets() fs.FS {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		// The embedded path is a compile-time constant that always exists, so this
		// can only fail on a corrupt build; return the raw FS so callers still get a
		// usable (if unrooted) handler rather than a nil panic.
		return assetsFS
	}
	return sub
}

// DiskAssets returns an fs.FS rooted at the assets directory under uiPath, for the
// `--ui-path` development mode: an operator iterating on the console points
// --ui-path at internal/httpapi/webui so edits to app.css (rebuilt by
// `npm run watch:css`) and the vendored JS are served live without rebuilding the
// binary. The templates themselves are compiled Go, so a .templ change still needs
// `templ generate` + a rebuild — but styling and asset iteration are instant.
//
// It validates that uiPath/assets exists and is a directory, returning an error
// the caller surfaces at startup (a misconfigured --ui-path should fail loudly,
// not silently serve 404s).
func DiskAssets(uiPath string) (fs.FS, error) {
	dir := filepath.Join(uiPath, "assets")
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &fs.PathError{Op: "open", Path: dir, Err: fs.ErrInvalid}
	}
	return os.DirFS(dir), nil
}
