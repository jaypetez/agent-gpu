package loadtest

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Endpoint identifies which API surface a single request exercises. The mix
// (Config.Mix) is a weighted set of these, so one run can blend, e.g., 80% chat
// completions with 20% model-list discovery to mimic a realistic client.
type Endpoint string

const (
	// EndpointChat is POST /v1/chat/completions (the primary inference path).
	EndpointChat Endpoint = "chat"
	// EndpointCompletions is POST /v1/completions (legacy text completion).
	EndpointCompletions Endpoint = "completions"
	// EndpointModels is GET /v1/models (discovery; not rate-limited server-side,
	// useful as a low-cost control in a mix).
	EndpointModels Endpoint = "models"
)

// validEndpoints is the set of endpoint names the driver knows how to issue. It
// gates both the --endpoint flag and the keys of a --mix spec.
var validEndpoints = map[Endpoint]bool{
	EndpointChat:        true,
	EndpointCompletions: true,
	EndpointModels:      true,
}

// ValidEndpoint reports whether ep is a known endpoint the driver can issue. The
// cmd layer uses it to validate the --endpoint flag.
func ValidEndpoint(ep Endpoint) bool { return validEndpoints[ep] }

// MixEntry is one weighted endpoint in a request mix: the endpoint and its
// integer weight relative to the other entries. Weights need not sum to 100; the
// driver normalizes by the total.
type MixEntry struct {
	Endpoint Endpoint
	Weight   int
}

// Mix is a weighted distribution of endpoints the driver selects from per
// request. It is built from a single --endpoint (weight 1) or a --mix spec like
// "chat=80,models=20". Entries are kept sorted by endpoint name so selection is
// deterministic given the same request index.
type Mix struct {
	entries []MixEntry
	total   int
}

// SingleEndpointMix returns a Mix that always selects ep (weight 1). It is the
// degenerate mix the --endpoint flag produces.
func SingleEndpointMix(ep Endpoint) Mix {
	return Mix{entries: []MixEntry{{Endpoint: ep, Weight: 1}}, total: 1}
}

// ParseMix parses a mix spec of the form "name=weight,name=weight" (e.g.
// "chat=80,models=20") into a Mix. Whitespace around tokens is tolerated.
// Weights must be positive integers; endpoint names must be known. A duplicate
// endpoint, a non-positive weight, an unknown endpoint, or a malformed token is
// an error so a typo fails loudly rather than silently skewing the run. An empty
// spec is an error (the caller should use SingleEndpointMix for the default).
func ParseMix(spec string) (Mix, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Mix{}, fmt.Errorf("empty mix spec")
	}
	seen := make(map[Endpoint]bool)
	var entries []MixEntry
	total := 0
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue // tolerate a trailing comma / doubled separators
		}
		name, weightStr, ok := strings.Cut(part, "=")
		if !ok {
			return Mix{}, fmt.Errorf("mix entry %q is not name=weight", part)
		}
		ep := Endpoint(strings.TrimSpace(name))
		if !validEndpoints[ep] {
			return Mix{}, fmt.Errorf("mix entry %q: unknown endpoint %q (want chat|completions|models)", part, ep)
		}
		if seen[ep] {
			return Mix{}, fmt.Errorf("mix entry %q: endpoint %q listed twice", part, ep)
		}
		weight, err := strconv.Atoi(strings.TrimSpace(weightStr))
		if err != nil {
			return Mix{}, fmt.Errorf("mix entry %q: weight %q is not an integer", part, weightStr)
		}
		if weight <= 0 {
			return Mix{}, fmt.Errorf("mix entry %q: weight must be positive", part)
		}
		seen[ep] = true
		entries = append(entries, MixEntry{Endpoint: ep, Weight: weight})
		total += weight
	}
	if len(entries) == 0 {
		return Mix{}, fmt.Errorf("empty mix spec")
	}
	// Deterministic order so Pick(i) is stable across runs given the same i.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Endpoint < entries[j].Endpoint })
	return Mix{entries: entries, total: total}, nil
}

// Pick selects an endpoint for the request with index i using a deterministic
// weighted round-robin over i mod total: it walks the (sorted) weighted entries
// and returns the one whose cumulative weight window contains i mod total. This
// spreads the mix evenly and reproducibly across the request stream without an
// RNG, so two runs with the same config issue the same sequence of endpoints —
// which keeps baselines comparable. A single-entry mix always returns that
// entry.
func (m Mix) Pick(i int) Endpoint {
	if len(m.entries) == 0 {
		return EndpointChat // defensive: a zero-value Mix should not occur in practice
	}
	if m.total <= 0 {
		return m.entries[0].Endpoint
	}
	slot := i % m.total
	if slot < 0 {
		slot += m.total
	}
	cum := 0
	for _, e := range m.entries {
		cum += e.Weight
		if slot < cum {
			return e.Endpoint
		}
	}
	// Unreachable (slot < total == sum of weights), but return the last entry
	// rather than panicking if invariants ever drift.
	return m.entries[len(m.entries)-1].Endpoint
}

// Entries returns the mix's weighted entries (sorted by endpoint name) for
// rendering in the run report. The returned slice is a copy so callers cannot
// mutate the Mix.
func (m Mix) Entries() []MixEntry {
	out := make([]MixEntry, len(m.entries))
	copy(out, m.entries)
	return out
}

// String renders the mix as a compact "chat=80,models=20" spec for the report
// header.
func (m Mix) String() string {
	parts := make([]string, 0, len(m.entries))
	for _, e := range m.entries {
		parts = append(parts, fmt.Sprintf("%s=%d", e.Endpoint, e.Weight))
	}
	return strings.Join(parts, ",")
}
