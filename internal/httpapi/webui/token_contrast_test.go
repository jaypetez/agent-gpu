package webui

import (
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// token_contrast_test.go verifies the committed design tokens meet WCAG AA contrast
// (issue #100 AC3/AC6: >=4.5:1 for normal text). It parses the color tokens out of
// assets/css/input.css (the single source of truth) and computes the contrast ratio
// for every foreground-on-background pair the console actually uses, failing the
// build if any text pairing would be unreadable. This keeps the palette honest: a
// future token tweak that darkens a foreground or lightens the ground below the AA
// threshold breaks the build rather than shipping an inaccessible console.

// colorTokenRe extracts `--color-name: #hex;` declarations from input.css.
var colorTokenRe = regexp.MustCompile(`--color-([a-z0-9-]+):\s*(#[0-9a-fA-F]{6});`)

// parseTokens reads the color tokens from input.css into a name->hex map.
func parseTokens(t *testing.T) map[string]string {
	t.Helper()
	path := filepath.Join("assets", "css", "input.css")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	tokens := map[string]string{}
	for _, m := range colorTokenRe.FindAllStringSubmatch(string(src), -1) {
		tokens[m[1]] = strings.ToLower(m[2])
	}
	if len(tokens) == 0 {
		t.Fatal("parsed no --color tokens from input.css; the token parser is out of date")
	}
	return tokens
}

// srgbToLinear converts one 0-255 sRGB channel to linear light per the WCAG
// relative-luminance definition.
func srgbToLinear(c8 int) float64 {
	c := float64(c8) / 255.0
	if c <= 0.03928 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

// luminance computes the WCAG relative luminance of a #rrggbb color.
func luminance(t *testing.T, hex string) float64 {
	t.Helper()
	hex = strings.TrimPrefix(hex, "#")
	r, _ := strconv.ParseInt(hex[0:2], 16, 0)
	g, _ := strconv.ParseInt(hex[2:4], 16, 0)
	b, _ := strconv.ParseInt(hex[4:6], 16, 0)
	return 0.2126*srgbToLinear(int(r)) + 0.7152*srgbToLinear(int(g)) + 0.0722*srgbToLinear(int(b))
}

// contrastRatio computes the WCAG contrast ratio between two colors (>=1).
func contrastRatio(t *testing.T, fg, bg string) float64 {
	t.Helper()
	l1 := luminance(t, fg)
	l2 := luminance(t, bg)
	if l1 < l2 {
		l1, l2 = l2, l1
	}
	return (l1 + 0.05) / (l2 + 0.05)
}

// TestDesignTokensMeetAAContrast asserts every text foreground-on-surface pairing
// the console uses clears the 4.5:1 AA threshold for normal text. The pairs mirror
// how the tokens are actually combined in the templates: body/muted/faint text and
// the status tones on the ground and on the raised panel surface, plus on-accent
// text on the solid accent fill. This is the committed contrast gate of AC3/AC6.
func TestDesignTokensMeetAAContrast(t *testing.T) {
	const aa = 4.5
	tk := parseTokens(t)

	mustToken := func(name string) string {
		v, ok := tk[name]
		if !ok {
			t.Fatalf("design token --color-%s not found in input.css", name)
		}
		return v
	}

	// Foreground tones that carry text, checked against both backgrounds text sits
	// on: the app ground and the raised panel surface (the harder, lighter one).
	textTones := []string{"fg", "fg-muted", "fg-faint", "ok", "warn", "danger", "info", "accent"}
	backgrounds := []string{"ground", "surface"}

	for _, bg := range backgrounds {
		for _, fg := range textTones {
			r := contrastRatio(t, mustToken(fg), mustToken(bg))
			if r < aa {
				t.Errorf("contrast --color-%s on --color-%s = %.2f:1, below AA %.1f:1", fg, bg, r, aa)
			}
		}
	}

	// Text on the solid accent fill (the primary button label) must also clear AA.
	if r := contrastRatio(t, mustToken("on-accent"), mustToken("accent")); r < aa {
		t.Errorf("contrast --color-on-accent on --color-accent = %.2f:1, below AA %.1f:1", r, aa)
	}
}
