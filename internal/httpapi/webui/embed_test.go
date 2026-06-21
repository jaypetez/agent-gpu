package webui

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// embed_test.go covers the asset-serving seam: the embedded production FS and the
// --ui-path development disk FS (issue #100 AC1).

// TestEmbeddedAssetsContainBuiltArtifacts proves the production embed actually
// carries the committed build artifacts the console needs: the Tailwind CSS, the
// vendored HTMX + Alpine, and the favicon. If `make ui` were skipped (a stale or
// missing app.css), this catches it rather than shipping a binary that 404s its own
// stylesheet.
func TestEmbeddedAssetsContainBuiltArtifacts(t *testing.T) {
	a := Assets()
	for _, name := range []string{
		"css/app.css",
		"js/htmx.min.js",
		"js/alpine.min.js",
		"img/favicon.svg",
	} {
		f, err := a.Open(name)
		if err != nil {
			t.Errorf("embedded asset %q not found: %v", name, err)
			continue
		}
		info, err := f.Stat()
		if err == nil && info.Size() == 0 {
			t.Errorf("embedded asset %q is empty", name)
		}
		_ = f.Close()
	}
}

// TestEmbeddedCSSCarriesTokens proves the embedded app.css is the real
// token-compiled build (not an empty placeholder): it must contain utility classes
// derived from the design tokens.
func TestEmbeddedCSSCarriesTokens(t *testing.T) {
	b, err := fs.ReadFile(Assets(), "css/app.css")
	if err != nil {
		t.Fatalf("read embedded app.css: %v", err)
	}
	css := string(b)
	for _, want := range []string{".bg-ground", ".text-fg", ".nav-item", ".panel"} {
		if !contains(css, want) {
			t.Errorf("embedded app.css is missing %q — run `make ui` and commit the result", want)
		}
	}
}

// TestDiskAssetsValidPath proves --ui-path resolves a disk FS rooted at the assets
// directory: a request path maps onto the on-disk file.
func TestDiskAssetsValidPath(t *testing.T) {
	// The package directory is a valid --ui-path target (it has assets/).
	got, err := DiskAssets(".")
	if err != nil {
		t.Fatalf("DiskAssets(.) error: %v", err)
	}
	if _, err := fs.Stat(got, "css/app.css"); err != nil {
		t.Errorf("disk assets FS missing css/app.css: %v", err)
	}
}

// TestDiskAssetsInvalidPath proves a bad --ui-path fails loudly (so cmd can surface
// it at startup) rather than silently serving 404s.
func TestDiskAssetsInvalidPath(t *testing.T) {
	if _, err := DiskAssets(filepath.Join(os.TempDir(), "agpu-nonexistent-ui-path-xyz")); err == nil {
		t.Error("DiskAssets on a path with no assets/ dir should return an error")
	}
}

// contains is a tiny substring helper kept local to avoid importing strings just
// for tests.
func contains(haystack, needle string) bool {
	return len(needle) == 0 || indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
