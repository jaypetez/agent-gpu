package webui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// token_lint_test.go is the build-failing guardrail that keeps the design-token
// system the SOLE source of styling truth (issue #100, AC3). Every color, size,
// and spacing value the console renders must come from a token-derived utility
// class defined in assets/css/input.css; a .templ file may NOT introduce a raw hex
// color or a Tailwind arbitrary value (the `[...]` escape hatch, e.g. text-[#abc]
// or bg-[12px]). Either would let a one-off value drift outside the token system
// and is exactly the "slop" the cold reviewer checks for. This test fails the
// build the moment such a value appears in a template.
//
// It deliberately scans the .templ SOURCES (not the generated *_templ.go, which is
// just string concatenation of the same content) and not the CSS (input.css IS the
// token definitions — it is where hex values legitimately live, once).

// rawHexRe matches a CSS hex color literal: # followed by 3, 4, 6, or 8 hex
// digits, on a word boundary so it does not match an id fragment mid-word. This is
// what must NEVER appear in a template — colors come only from token classes.
var rawHexRe = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)

// arbitraryValueRe matches a Tailwind arbitrary value: a utility followed by a
// bracketed literal, e.g. `text-[#fff]`, `bg-[rgb(0,0,0)]`, `w-[37px]`,
// `grid-cols-[1fr]`. The pattern looks for a `-[` … `]` attached to a class-like
// token. Arbitrary values bypass the token scale, so they are forbidden in
// templates. (Plain attribute brackets in HTML, like an empty `[]`, do not match
// because the `-` prefix is required.)
var arbitraryValueRe = regexp.MustCompile(`[a-zA-Z0-9]-\[[^\]]+\]`)

// allowedHexContexts are substrings whose line is exempt from the hex scan: an SVG
// path/viewBox carries no color, but inline SVG attributes never contain a hex
// here (icons inherit currentColor). There are intentionally no color exemptions —
// if a template needs a color it must use a token class. This slice exists so a
// future, justified exemption is explicit and reviewable rather than silently
// loosening the regex.
var allowedHexContexts = []string{}

// scanTemplFiles returns the .templ source files in this package directory.
func scanTemplFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".templ") {
			files = append(files, e.Name())
		}
	}
	if len(files) == 0 {
		t.Fatal("no .templ files found to lint; the token-lint guard is not actually scanning anything")
	}
	return files
}

// lineExempt reports whether a line is exempt from the hex scan.
func lineExempt(line string) bool {
	for _, ctx := range allowedHexContexts {
		if strings.Contains(line, ctx) {
			return true
		}
	}
	return false
}

// TestTemplatesUseOnlyDesignTokens fails if any .templ file contains a raw hex
// color or a Tailwind arbitrary `[...]` value. This is the committed build gate
// of AC3.
func TestTemplatesUseOnlyDesignTokens(t *testing.T) {
	for _, name := range scanTemplFiles(t) {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			lineNo := i + 1
			if !lineExempt(line) {
				if m := rawHexRe.FindString(line); m != "" {
					t.Errorf("%s:%d: raw hex color %q — use a design-token utility class (defined in assets/css/input.css) instead:\n\t%s",
						name, lineNo, m, strings.TrimSpace(line))
				}
			}
			if m := arbitraryValueRe.FindString(line); m != "" {
				t.Errorf("%s:%d: Tailwind arbitrary value %q — use a token-scale utility instead:\n\t%s",
					name, lineNo, m, strings.TrimSpace(line))
			}
		}
	}
}

// TestTokenLintCatchesViolations proves the guard actually fires — a regression in
// the regexes that made them match nothing would otherwise let real violations
// through silently. It checks the patterns against representative offending and
// clean snippets.
func TestTokenLintCatchesViolations(t *testing.T) {
	offendingHex := []string{
		`<div class="bg-surface" style="color:#38e1c6">x</div>`,
		`<span class="text-[#ff0000]">x</span>`,
		`<i style="border:1px solid #abc"></i>`,
	}
	for _, s := range offendingHex {
		if !rawHexRe.MatchString(s) {
			t.Errorf("rawHexRe failed to flag an offending hex literal: %q", s)
		}
	}
	offendingArbitrary := []string{
		`<div class="w-[37px]">x</div>`,
		`<div class="bg-[rgb(1,2,3)]">x</div>`,
		`<div class="grid-cols-[1fr_2fr]">x</div>`,
	}
	for _, s := range offendingArbitrary {
		if !arbitraryValueRe.MatchString(s) {
			t.Errorf("arbitraryValueRe failed to flag an offending arbitrary value: %q", s)
		}
	}
	clean := []string{
		`<div class="bg-surface text-fg p-4 grid-cols-3">x</div>`,
		`<span class="badge badge-ok font-mono">online</span>`,
		`<a href="/admin/" class="nav-item">Overview</a>`,
	}
	for _, s := range clean {
		if rawHexRe.MatchString(s) {
			t.Errorf("rawHexRe falsely flagged a clean line: %q", s)
		}
		if arbitraryValueRe.MatchString(s) {
			t.Errorf("arbitraryValueRe falsely flagged a clean line: %q", s)
		}
	}
}

// TestInputCSSIsTheTokenSource confirms the token definitions live where they
// should: input.css must declare the @theme block and the core color tokens, so
// the "single source of truth" claim is verified rather than asserted. If the
// tokens were moved or renamed, this catches it.
func TestInputCSSIsTheTokenSource(t *testing.T) {
	path := filepath.Join("assets", "css", "input.css")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	css := string(src)
	for _, want := range []string{"@theme", "--color-ground", "--color-fg", "--color-accent", "--spacing"} {
		if !strings.Contains(css, want) {
			t.Errorf("input.css is missing the token %q — the design-token source is incomplete", want)
		}
	}
}
