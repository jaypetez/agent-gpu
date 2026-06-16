package version

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

// withVars temporarily overrides the package build-metadata vars for the
// duration of a test and restores them afterwards.
func withVars(t *testing.T, ver, commit, date string) {
	t.Helper()
	origV, origC, origD := Version, Commit, Date
	Version, Commit, Date = ver, commit, date
	t.Cleanup(func() { Version, Commit, Date = origV, origC, origD })
}

func TestGetUsesInjectedValues(t *testing.T) {
	withVars(t, "v1.2.3", "abcdef1234567890", "2026-06-16T12:00:00Z")

	got := Get()
	if got.Version != "v1.2.3" {
		t.Errorf("Version = %q, want v1.2.3", got.Version)
	}
	if got.Commit != "abcdef1234567890" {
		t.Errorf("Commit = %q, want abcdef1234567890", got.Commit)
	}
	if got.Date != "2026-06-16T12:00:00Z" {
		t.Errorf("Date = %q, want 2026-06-16T12:00:00Z", got.Date)
	}
	if got.GoVersion != runtime.Version() {
		t.Errorf("GoVersion = %q, want %q", got.GoVersion, runtime.Version())
	}
	if got.OS != runtime.GOOS || got.Arch != runtime.GOARCH {
		t.Errorf("OS/Arch = %s/%s, want %s/%s", got.OS, got.Arch, runtime.GOOS, runtime.GOARCH)
	}
}

func TestStringFormat(t *testing.T) {
	withVars(t, "v0.1.0", "0123456789abcdef", "2026-06-16T12:00:00Z")

	s := String()
	// Commit must be truncated to the short length.
	wantCommit := "0123456789ab"
	for _, sub := range []string{
		"agentgpu v0.1.0",
		"commit " + wantCommit,
		"built 2026-06-16T12:00:00Z",
		runtime.GOOS + "/" + runtime.GOARCH,
		runtime.Version(),
	} {
		if !strings.Contains(s, sub) {
			t.Errorf("String() = %q, missing %q", s, sub)
		}
	}
	// The full hash must not appear (proves truncation happened).
	if strings.Contains(s, "0123456789abcdef") {
		t.Errorf("String() = %q, commit was not truncated", s)
	}
	if strings.Contains(s, "\n") {
		t.Errorf("String() must be a single line, got %q", s)
	}
}

func TestShortCommit(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"none", "none"},
		{"abcdef", "abcdef"},
		{"0123456789ab", "0123456789ab"},     // exactly the limit, unchanged
		{"0123456789abcdef", "0123456789ab"}, // longer, truncated
	}
	for _, tc := range cases {
		if got := shortCommit(tc.in); got != tc.want {
			t.Errorf("shortCommit(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestApplyBuildInfoFallback verifies that when Version is "dev", the metadata
// from runtime/debug.ReadBuildInfo is used to fill in version/commit/date.
func TestApplyBuildInfoFallback(t *testing.T) {
	info := Info{Version: "dev", Commit: "none", Date: "unknown"}
	read := func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Main: debug.Module{Version: "v9.9.9"},
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "deadbeefcafe"},
				{Key: "vcs.time", Value: "2025-01-02T03:04:05Z"},
				{Key: "vcs.modified", Value: "false"},
			},
		}, true
	}
	applyBuildInfo(&info, read)

	if info.Version != "v9.9.9" {
		t.Errorf("Version = %q, want v9.9.9", info.Version)
	}
	if info.Commit != "deadbeefcafe" {
		t.Errorf("Commit = %q, want deadbeefcafe", info.Commit)
	}
	if info.Date != "2025-01-02T03:04:05Z" {
		t.Errorf("Date = %q, want 2025-01-02T03:04:05Z", info.Date)
	}
}

// TestApplyBuildInfoIgnoresDevel ensures a checkout build ("(devel)") does not
// clobber the placeholder, and that absent VCS settings leave defaults intact.
func TestApplyBuildInfoIgnoresDevel(t *testing.T) {
	info := Info{Version: "dev", Commit: "none", Date: "unknown"}
	read := func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, true
	}
	applyBuildInfo(&info, read)

	if info.Version != "dev" {
		t.Errorf("Version = %q, want dev (unchanged)", info.Version)
	}
	if info.Commit != "none" {
		t.Errorf("Commit = %q, want none (unchanged)", info.Commit)
	}
	if info.Date != "unknown" {
		t.Errorf("Date = %q, want unknown (unchanged)", info.Date)
	}
}

// TestApplyBuildInfoUnavailable covers the path where ReadBuildInfo reports no
// info (e.g. some test binaries): the placeholders must survive.
func TestApplyBuildInfoUnavailable(t *testing.T) {
	info := Info{Version: "dev", Commit: "none", Date: "unknown"}
	applyBuildInfo(&info, func() (*debug.BuildInfo, bool) { return nil, false })

	if info.Version != "dev" || info.Commit != "none" || info.Date != "unknown" {
		t.Errorf("placeholders changed: %+v", info)
	}
}

// TestApplyBuildInfoDoesNotOverrideInjected verifies that values already set
// (not placeholders) are preserved even when build info is present.
func TestApplyBuildInfoDoesNotOverrideInjected(t *testing.T) {
	info := Info{Version: "dev", Commit: "injectedcommit", Date: "injecteddate"}
	read := func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Main: debug.Module{Version: "v1.0.0"},
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "fromvcs"},
				{Key: "vcs.time", Value: "fromvcs-time"},
			},
		}, true
	}
	applyBuildInfo(&info, read)

	if info.Commit != "injectedcommit" {
		t.Errorf("Commit = %q, want injectedcommit (preserved)", info.Commit)
	}
	if info.Date != "injecteddate" {
		t.Errorf("Date = %q, want injecteddate (preserved)", info.Date)
	}
	// Version was the "dev" placeholder, so it should be filled from build info.
	if info.Version != "v1.0.0" {
		t.Errorf("Version = %q, want v1.0.0", info.Version)
	}
}
