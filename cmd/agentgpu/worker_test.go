package main

import "testing"

func TestParseModels(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"blank", "   ", nil},
		{"single", "llama3", []string{"llama3"}},
		{"multiple", "llama3,mistral", []string{"llama3", "mistral"}},
		{"trims and drops blanks", " llama3 , mistral ,,", []string{"llama3", "mistral"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseModels(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("parseModels(%q) = %d models, want %d", tc.in, len(got), len(tc.want))
			}
			for i, m := range got {
				if m.Name != tc.want[i] {
					t.Fatalf("parseModels(%q)[%d].Name = %q, want %q", tc.in, i, m.Name, tc.want[i])
				}
			}
		})
	}
}
