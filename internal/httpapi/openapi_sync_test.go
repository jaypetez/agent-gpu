package httpapi

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// This test is the guardrail that keeps the OpenAPI 3.1 spec (openapi.yaml at the
// repo root) in lockstep with the routes the HTTP server actually registers
// (#14): it derives the live route set straight from the source of truth —
// httpapi.go's mux.Handle(...) registrations — and the documented route set from
// the spec's paths, then asserts the two are identical. Adding, removing, or
// renaming a route without making the matching spec edit fails this test, so the
// spec cannot silently drift from the implementation.
//
// The live side is parsed from source rather than introspected from a built
// *http.ServeMux because net/http exposes no public route enumeration. Parsing
// the registration literals is robust for this package because every route is
// registered with a single string-literal pattern in one file (httpapi.go).

// httpMethods is the set of leading tokens that count as an explicit method in a
// Go 1.22 ServeMux pattern ("GET /x", "POST /x", …). A pattern without one of
// these prefixes has no method restriction at the mux layer; its verb is pinned
// by the handler instead and is supplied by noMethodRoutes below.
var httpMethods = map[string]bool{
	"GET":     true,
	"POST":    true,
	"PUT":     true,
	"DELETE":  true,
	"PATCH":   true,
	"HEAD":    true,
	"OPTIONS": true,
}

// noMethodRoutes pins the verb for the routes registered without a method token
// in their mux pattern (the discovery and inference routes, whose handlers
// enforce the method internally). Keyed by the bare path. A route registered
// without a method that is missing here fails the test loudly (see deriveRoutes),
// so a new no-method route forces a conscious update of this map AND the spec.
var noMethodRoutes = map[string]string{
	"/v1/models":           "GET",
	"/models":              "GET",
	"/v1/chat/completions": "POST",
	"/v1/completions":      "POST",
}

// muxHandleRe matches the pattern string literal in a `mux.Handle("…", …)` call,
// e.g. mux.Handle("GET /v1/sessions/{id}", …). Registrations in httpapi.go go
// through either mux.Handle directly (Handler) or mux.Handle(...) in
// registerAdminRoutes, so this single shape captures all of them.
var muxHandleRe = regexp.MustCompile(`mux\.Handle\("([^"]+)"`)

// route is a normalized (METHOD, path) operation, the common currency both sides
// of the comparison reduce to.
type route struct {
	method string
	path   string
}

func (r route) String() string { return r.method + " " + r.path }

// repoRoot returns the repository root relative to this test file
// (internal/httpapi → ../..), so the test reads the real openapi.yaml and
// httpapi.go regardless of the working directory the test binary runs in.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the repo root")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}

// deriveRoutes parses httpapi.go and returns the set of routes the server
// registers, normalized to (METHOD, path). It is the authoritative live route
// set the spec is checked against.
func deriveRoutes(t *testing.T, root string) map[route]bool {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(root, "internal", "httpapi", "httpapi.go"))
	if err != nil {
		t.Fatalf("read httpapi.go: %v", err)
	}
	matches := muxHandleRe.FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		t.Fatal("no mux.Handle(...) registrations found in httpapi.go; the route parser is out of date")
	}
	routes := make(map[route]bool, len(matches))
	for _, m := range matches {
		pattern := m[1]
		method, path := splitPattern(t, pattern)
		r := route{method: method, path: path}
		if routes[r] {
			t.Fatalf("duplicate route registration parsed from httpapi.go: %s", r)
		}
		routes[r] = true
	}
	return routes
}

// splitPattern normalizes a ServeMux pattern into (METHOD, path). A pattern with
// a leading method token ("POST /v1/sessions") splits on the space; a pattern
// without one ("/v1/models") is resolved through noMethodRoutes. The path is run
// through normalizePath so a multi-segment wildcard ("{model...}") matches the
// single-brace OpenAPI form ("{model}").
func splitPattern(t *testing.T, pattern string) (method, path string) {
	t.Helper()
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		tok := pattern[:i]
		if httpMethods[tok] {
			return tok, normalizePath(pattern[i+1:])
		}
		t.Fatalf("pattern %q has a leading token %q that is not an HTTP method", pattern, tok)
	}
	m, ok := noMethodRoutes[pattern]
	if !ok {
		t.Fatalf("route %q is registered without a method token and is not pinned in noMethodRoutes; "+
			"add its verb there (and document it in openapi.yaml)", pattern)
	}
	return m, normalizePath(pattern)
}

// trailingWildcardRe matches Go 1.22's multi-segment wildcard form "{name...}",
// which the OpenAPI spec writes as a plain "{name}" path parameter.
var trailingWildcardRe = regexp.MustCompile(`\{([^}]+)\.\.\.\}`)

// normalizePath rewrites a Go ServeMux path so it matches how the same operation
// is written in openapi.yaml: a multi-segment wildcard "{name...}" (used so a
// path parameter can contain slashes/colons, e.g. /v1/models/{model...}) is
// reduced to the single-brace "{name}" the spec declares.
func normalizePath(path string) string {
	return trailingWildcardRe.ReplaceAllString(path, "{$1}")
}

// specDoc is the minimal slice of the OpenAPI document the sync check needs: the
// paths object mapping each path to the HTTP methods (operations) defined on it.
type specDoc struct {
	Paths map[string]map[string]yaml.Node `yaml:"paths"`
}

// documentedRoutes parses openapi.yaml and returns the set of documented
// operations, normalized to (METHOD, path). Only genuine operation keys (the HTTP
// methods) are counted; path-level siblings such as "parameters" or "summary"
// are skipped.
func documentedRoutes(t *testing.T, root string) map[route]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var doc specDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	if len(doc.Paths) == 0 {
		t.Fatal("openapi.yaml declares no paths")
	}
	routes := make(map[route]bool)
	for path, ops := range doc.Paths {
		for method := range ops {
			up := strings.ToUpper(method)
			if !httpMethods[up] {
				// Path-level keys (parameters, summary, description, $ref, servers)
				// are not operations; skip them.
				continue
			}
			routes[route{method: up, path: path}] = true
		}
	}
	return routes
}

// TestOpenAPISpecMatchesRegisteredRoutes asserts the OpenAPI spec documents
// exactly the routes the server registers — no more, no fewer. This is the
// "keep the spec in sync with the implementation" acceptance criterion of #14.
func TestOpenAPISpecMatchesRegisteredRoutes(t *testing.T) {
	root := repoRoot(t)
	registered := deriveRoutes(t, root)
	documented := documentedRoutes(t, root)

	// The project currently exposes exactly 22 public HTTP routes (20 + the
	// settings/config GET+PUT added in #92). Pin the count so an accidental over- or
	// under-registration (or a parser regression that silently drops routes) is
	// caught even if both sides happen to agree.
	const wantRoutes = 22
	if len(registered) != wantRoutes {
		t.Errorf("parsed %d registered routes from httpapi.go, want %d:\n%s",
			len(registered), wantRoutes, formatRoutes(registered))
	}

	var missingFromSpec, extraInSpec []string
	for r := range registered {
		if !documented[r] {
			missingFromSpec = append(missingFromSpec, r.String())
		}
	}
	for r := range documented {
		if !registered[r] {
			extraInSpec = append(extraInSpec, r.String())
		}
	}
	sort.Strings(missingFromSpec)
	sort.Strings(extraInSpec)

	if len(missingFromSpec) > 0 {
		t.Errorf("routes registered by the server but MISSING from openapi.yaml (document them):\n  %s",
			strings.Join(missingFromSpec, "\n  "))
	}
	if len(extraInSpec) > 0 {
		t.Errorf("routes documented in openapi.yaml but NOT registered by the server (remove or fix them):\n  %s",
			strings.Join(extraInSpec, "\n  "))
	}
}

// formatRoutes renders a route set as a stable, sorted, newline-joined list for
// readable failure output.
func formatRoutes(set map[route]bool) string {
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, "  "+r.String())
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}
