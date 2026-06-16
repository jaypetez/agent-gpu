// Package version exposes the build metadata for the agentgpu binary.
//
// The three string vars are overridden at build time via -ldflags -X by the
// release pipeline (see .goreleaser.yaml). When they are left at their defaults
// — e.g. a plain `go build` or `go install …/cmd/agentgpu@latest` — the package
// falls back to the module/VCS metadata embedded by the Go toolchain through
// runtime/debug.ReadBuildInfo, so users still get a useful version string.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Build metadata. These are intentionally plain vars (not consts) so the
// release build can set them with:
//
//	-X github.com/jaypetez/agent-gpu/internal/version.Version=v1.2.3
//	-X github.com/jaypetez/agent-gpu/internal/version.Commit=<sha>
//	-X github.com/jaypetez/agent-gpu/internal/version.Date=<rfc3339>
var (
	// Version is the released semantic version (e.g. "v0.1.0"). It stays "dev"
	// for non-release builds, which triggers the ReadBuildInfo fallback.
	Version = "dev"
	// Commit is the git commit the binary was built from.
	Commit = "none"
	// Date is the build timestamp (RFC 3339, set from the commit time for
	// reproducible builds).
	Date = "unknown"
)

// shortCommitLen bounds how much of a commit hash is shown in the version line.
const shortCommitLen = 12

// Info is a resolved snapshot of the build metadata. Fields are never empty:
// the resolver substitutes a placeholder ("dev"/"none"/"unknown") when a value
// is unavailable.
type Info struct {
	Version   string
	Commit    string
	Date      string
	GoVersion string
	OS        string
	Arch      string
}

// Get returns the resolved build metadata. When Version was injected at build
// time it is used verbatim; otherwise the function consults
// runtime/debug.ReadBuildInfo for the main-module version and the VCS
// revision/time stamped by `go build`.
func Get() Info {
	info := Info{
		Version:   Version,
		Commit:    Commit,
		Date:      Date,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
	if info.Version == "dev" {
		applyBuildInfo(&info, debug.ReadBuildInfo)
	}
	return info
}

// applyBuildInfo enriches a "dev" build with whatever the Go toolchain embedded
// via runtime/debug. It is split out (and takes the reader as a parameter) so
// the fallback path is unit-testable without rebuilding the binary.
func applyBuildInfo(info *Info, read func() (*debug.BuildInfo, bool)) {
	bi, ok := read()
	if !ok || bi == nil {
		return
	}
	// A module built with `go install pkg@v1.2.3` records that version on the
	// main module; `go build` from a checkout records "(devel)", which we keep
	// as "dev" since it carries no useful information.
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		info.Version = v
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if info.Commit == "none" && s.Value != "" {
				info.Commit = s.Value
			}
		case "vcs.time":
			if info.Date == "unknown" && s.Value != "" {
				info.Date = s.Value
			}
		}
	}
}

// shortCommit trims a commit hash to shortCommitLen characters for display,
// leaving placeholders and already-short values untouched.
func shortCommit(commit string) string {
	if len(commit) > shortCommitLen {
		return commit[:shortCommitLen]
	}
	return commit
}

// String renders a single-line, human-readable version banner, e.g.:
//
//	agentgpu v0.1.0 (commit abc123def456, built 2026-06-16T12:00:00Z, linux/amd64, go1.23.0)
func String() string {
	i := Get()
	return fmt.Sprintf("agentgpu %s (commit %s, built %s, %s/%s, %s)",
		i.Version, shortCommit(i.Commit), i.Date, i.OS, i.Arch, i.GoVersion)
}
