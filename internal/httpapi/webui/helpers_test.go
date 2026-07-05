package webui

import "testing"

// helpers_test.go unit-tests the hand-written view helpers (the pure Go behind the
// templates), so their logic is verified independently of a rendered page.

func TestPageTitle(t *testing.T) {
	if got := pageTitle(""); got != "agent-gpu console" {
		t.Errorf("pageTitle(\"\") = %q", got)
	}
	if got := pageTitle("Overview"); got != "Overview · agent-gpu console" {
		t.Errorf("pageTitle(Overview) = %q", got)
	}
}

func TestCSRFHeaderJSON(t *testing.T) {
	got := csrfHeaderJSON("abc123")
	want := `{"X-CSRF-Token":"abc123"}`
	if got != want {
		t.Errorf("csrfHeaderJSON = %q, want %q", got, want)
	}
}

func TestShortID(t *testing.T) {
	if got := shortID("abc"); got != "abc" {
		t.Errorf("shortID(short) = %q, want unchanged", got)
	}
	if got := shortID("0123456789abcdef"); got != "01234567…" {
		t.Errorf("shortID(long) = %q", got)
	}
}

func TestRoleLabel(t *testing.T) {
	if got := roleLabel(Viewer{IsAdmin: true}); got != "admin" {
		t.Errorf("roleLabel(admin) = %q", got)
	}
	if got := roleLabel(Viewer{Roles: []string{"user", "read-only"}}); got != "user, read-only" {
		t.Errorf("roleLabel(roles) = %q", got)
	}
	if got := roleLabel(Viewer{}); got != "restricted" {
		t.Errorf("roleLabel(none) = %q", got)
	}
}

func TestToneText(t *testing.T) {
	cases := map[string]string{
		ToneOK:     "text-ok",
		ToneWarn:   "text-warn",
		ToneDanger: "text-danger",
		ToneInfo:   "text-info",
		ToneIdle:   "text-idle",
		"unknown":  "text-fg-muted",
	}
	for tone, want := range cases {
		if got := toneText(tone); got != want {
			t.Errorf("toneText(%q) = %q, want %q", tone, got, want)
		}
	}
}

func TestToneBadgeAndBarAndWord(t *testing.T) {
	if toneBadge(ToneOK) != "badge-ok" || toneBadge("x") != "badge-idle" {
		t.Error("toneBadge mapping wrong")
	}
	if toneBar(ToneInfo) != "bg-accent" || toneBar(ToneOK) != "bg-ok" || toneBar("x") != "bg-idle" {
		t.Error("toneBar mapping wrong")
	}
	if toneWord(ToneWarn) != "watch" || toneWord(ToneDanger) != "alert" || toneWord(ToneOK) != "ok" {
		t.Error("toneWord mapping wrong")
	}
}

func TestBarWidth(t *testing.T) {
	cases := []struct {
		n, total int
		want     string
	}{
		{0, 0, "width:0%"},
		{0, 10, "width:0%"},
		{10, 10, "width:100%"},
		{20, 10, "width:100%"}, // clamped
		{5, 10, "width:50%"},
		{1, 1000, "width:2%"}, // tiny-but-nonzero sliver
	}
	for _, c := range cases {
		if got := barWidth(c.n, c.total); got != c.want {
			t.Errorf("barWidth(%d,%d) = %q, want %q", c.n, c.total, got, c.want)
		}
	}
}

func TestIntFormatters(t *testing.T) {
	if itoa(42) != "42" {
		t.Error("itoa")
	}
	if itoaU32(7) != "7" {
		t.Error("itoaU32")
	}
}
