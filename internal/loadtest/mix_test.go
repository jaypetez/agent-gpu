package loadtest

import "testing"

// TestParseMixValid covers the well-formed spec shapes: a single entry, several
// entries, whitespace tolerance, and a trailing comma.
func TestParseMixValid(t *testing.T) {
	cases := []struct {
		name      string
		spec      string
		wantTotal int
		wantLen   int
	}{
		{"single", "chat=1", 1, 1},
		{"two", "chat=80,models=20", 100, 2},
		{"three", "chat=70,completions=20,models=10", 100, 3},
		{"whitespace", "  chat = 80 , models = 20 ", 100, 2},
		{"trailing comma", "chat=80,models=20,", 100, 2},
		{"non-100 weights", "chat=3,models=1", 4, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseMix(tc.spec)
			if err != nil {
				t.Fatalf("ParseMix(%q) error: %v", tc.spec, err)
			}
			if m.total != tc.wantTotal {
				t.Errorf("total = %d, want %d", m.total, tc.wantTotal)
			}
			if len(m.entries) != tc.wantLen {
				t.Errorf("entries = %d, want %d", len(m.entries), tc.wantLen)
			}
		})
	}
}

// TestParseMixInvalid covers every rejection path so a typo fails loudly.
func TestParseMixInvalid(t *testing.T) {
	cases := []struct {
		name string
		spec string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"no equals", "chat"},
		{"unknown endpoint", "frobnicate=10"},
		{"non-integer weight", "chat=abc"},
		{"zero weight", "chat=0"},
		{"negative weight", "chat=-5"},
		{"duplicate endpoint", "chat=50,chat=50"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseMix(tc.spec); err == nil {
				t.Errorf("ParseMix(%q) = nil error, want error", tc.spec)
			}
		})
	}
}

// TestMixPickDistribution proves Pick spreads requests across the mix in the
// configured proportion over one full period. For chat=80,models=20 (sorted:
// chat then models), indices 0..99 should yield exactly 80 chat and 20 models.
func TestMixPickDistribution(t *testing.T) {
	m, err := ParseMix("chat=80,models=20")
	if err != nil {
		t.Fatalf("ParseMix: %v", err)
	}
	counts := map[Endpoint]int{}
	for i := 0; i < 100; i++ {
		counts[m.Pick(i)]++
	}
	if counts[EndpointChat] != 80 {
		t.Errorf("chat count = %d, want 80", counts[EndpointChat])
	}
	if counts[EndpointModels] != 20 {
		t.Errorf("models count = %d, want 20", counts[EndpointModels])
	}
}

// TestMixPickDeterministic proves Pick(i) is stable: the same index always maps
// to the same endpoint (so two runs issue an identical request sequence).
func TestMixPickDeterministic(t *testing.T) {
	m, _ := ParseMix("chat=70,completions=20,models=10")
	for i := 0; i < 250; i++ {
		if m.Pick(i) != m.Pick(i) {
			t.Fatalf("Pick(%d) not deterministic", i)
		}
	}
	// Negative index is handled (defensive) without panicking.
	_ = m.Pick(-1)
}

// TestSingleEndpointMix proves the degenerate mix always returns its endpoint.
func TestSingleEndpointMix(t *testing.T) {
	m := SingleEndpointMix(EndpointCompletions)
	for i := 0; i < 10; i++ {
		if got := m.Pick(i); got != EndpointCompletions {
			t.Fatalf("Pick(%d) = %q, want completions", i, got)
		}
	}
	if m.String() != "completions=1" {
		t.Errorf("String() = %q, want completions=1", m.String())
	}
}

// TestMixString proves the report-facing rendering round-trips the sorted spec.
func TestMixString(t *testing.T) {
	m, _ := ParseMix("models=20,chat=80")
	if got := m.String(); got != "chat=80,models=20" {
		t.Errorf("String() = %q, want chat=80,models=20", got)
	}
}
