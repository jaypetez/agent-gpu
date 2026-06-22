package webui

import "strings"

// keys.go holds the small hand-written view helpers the keys/users/permissions
// templates (keys.templ, #102) use. They produce only text — never CSS class
// strings — so they need no Tailwind content scanning (the class vocabulary lives
// literally in keys.templ, which input.css already scans via ../../*.templ).

// hasString reports whether xs includes v. The role/scope pickers use it to mark a
// checkbox checked when the key already holds that role/scope (the permissions
// editor prefills the current grants).
func hasString(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// ownerTeam renders the owner/team labels as a single cell value: "owner · team"
// when both are set, otherwise whichever is present. The caller only invokes it
// when at least one is non-empty.
func ownerTeam(owner, team string) string {
	switch {
	case owner != "" && team != "":
		return owner + " · " + team
	case owner != "":
		return owner
	default:
		return team
	}
}

// joinLines renders a model-pattern list as newline-separated text for a textarea's
// initial value, so the permissions editor shows one pattern per line (the same
// shape parseModelList accepts back). An empty list yields an empty string.
func joinLines(xs []string) string {
	return strings.Join(xs, "\n")
}

// revealTokenLabel is the label above the one-time token field, phrased for a fresh
// create vs. a rotate so the reveal reads correctly in both flows.
func revealTokenLabel(rev KeyReveal) string {
	if rev.Rotated {
		return "New token (replaces the previous one)"
	}
	return "Token (copy it now)"
}
