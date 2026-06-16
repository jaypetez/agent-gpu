package gpu

import "testing"

// TestParseNvidiaSMI exercises the CSV parser across single-GPU, multi-GPU,
// missing/N-A utilization, surrounding whitespace, blank lines, and malformed
// input. memory.total / memory.free are MiB (nounits) and must become bytes.
func TestParseNvidiaSMI(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		out     string
		want    []Device
		wantErr bool
	}{
		{
			name: "single gpu",
			out:  "NVIDIA GeForce RTX 4090, 24564, 24268, 13\n",
			want: []Device{{
				Name:      "NVIDIA GeForce RTX 4090",
				TotalVRAM: 24564 * mib,
				FreeVRAM:  24268 * mib,
				Load:      13,
			}},
		},
		{
			name: "multi gpu",
			out: "" +
				"NVIDIA A100-SXM4-40GB, 40960, 40000, 5\n" +
				"NVIDIA A100-SXM4-40GB, 40960, 39000, 8\n",
			want: []Device{
				{Name: "NVIDIA A100-SXM4-40GB", TotalVRAM: 40960 * mib, FreeVRAM: 40000 * mib, Load: 5},
				{Name: "NVIDIA A100-SXM4-40GB", TotalVRAM: 40960 * mib, FreeVRAM: 39000 * mib, Load: 8},
			},
		},
		{
			name: "utilization NA leaves load zero",
			out:  "NVIDIA GeForce GTX 1080, 8192, 8000, [N/A]\n",
			want: []Device{{Name: "NVIDIA GeForce GTX 1080", TotalVRAM: 8192 * mib, FreeVRAM: 8000 * mib, Load: 0}},
		},
		{
			name: "utilization column absent leaves load zero",
			out:  "NVIDIA T4, 15360, 15000\n",
			want: []Device{{Name: "NVIDIA T4", TotalVRAM: 15360 * mib, FreeVRAM: 15000 * mib, Load: 0}},
		},
		{
			name: "blank lines skipped",
			out:  "\n  \nNVIDIA T4, 15360, 15000, 0\n\n",
			want: []Device{{Name: "NVIDIA T4", TotalVRAM: 15360 * mib, FreeVRAM: 15000 * mib, Load: 0}},
		},
		{
			name: "load over 100 clamped",
			out:  "NVIDIA Weird, 1024, 512, 250\n",
			want: []Device{{Name: "NVIDIA Weird", TotalVRAM: 1024 * mib, FreeVRAM: 512 * mib, Load: 100}},
		},
		{
			name: "zero vram parses (full or unreported)",
			out:  "NVIDIA Empty, 0, 0, 0\n",
			want: []Device{{Name: "NVIDIA Empty", TotalVRAM: 0, FreeVRAM: 0, Load: 0}},
		},
		{
			name:    "non-numeric total is an error",
			out:     "NVIDIA Bad, not-a-number, 100, 0\n",
			wantErr: true,
		},
		{
			name:    "too few fields is an error",
			out:     "NVIDIA Bad, 1024\n",
			wantErr: true,
		},
		{
			name: "empty output yields no devices",
			out:  "",
			want: nil,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseNvidiaSMI([]byte(tc.out))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got devices %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			assertDevices(t, got, tc.want)
		})
	}
}

// assertDevices compares two device slices field by field with a clear message.
func assertDevices(t *testing.T, got, want []Device) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("device count = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("device[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
