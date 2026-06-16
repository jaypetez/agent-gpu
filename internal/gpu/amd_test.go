package gpu

import (
	"context"
	"os/exec"
	"testing"
)

// TestParseRocmSMI exercises the rocm-smi JSON parser: single and multi card,
// byte VRAM with free derived as total-used, load from "GPU use (%)", defensive
// key matching (units in key names vary), skipping non-card entries, and
// malformed input. VRAM fields are already in bytes.
func TestParseRocmSMI(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		out     string
		want    []Device
		wantErr bool
	}{
		{
			name: "single card",
			out: `{
			  "card0": {
			    "Card series": "Instinct MI210",
			    "VRAM Total Memory (B)": "68702699520",
			    "VRAM Total Used Memory (B)": "10747904",
			    "GPU use (%)": "7"
			  }
			}`,
			want: []Device{{
				Name:      "Instinct MI210",
				TotalVRAM: 68702699520,
				FreeVRAM:  68702699520 - 10747904,
				Load:      7,
			}},
		},
		{
			name: "multi card sorted by id",
			out: `{
			  "card1": {"Card series":"RX 7900 XTX","VRAM Total Memory (B)":"25753026560","VRAM Total Used Memory (B)":"1000000000","GPU use (%)":"50"},
			  "card0": {"Card series":"RX 7900 XTX","VRAM Total Memory (B)":"25753026560","VRAM Total Used Memory (B)":"500000000","GPU use (%)":"10"}
			}`,
			// Sorted by id: card0 first, then card1.
			want: []Device{
				{Name: "RX 7900 XTX", TotalVRAM: 25753026560, FreeVRAM: 25753026560 - 500000000, Load: 10},
				{Name: "RX 7900 XTX", TotalVRAM: 25753026560, FreeVRAM: 25753026560 - 1000000000, Load: 50},
			},
		},
		{
			name: "non-card entry skipped",
			out: `{
			  "system": {"Driver version": "6.0.0"},
			  "card0": {"Card model":"AMD Radeon Pro W7900","VRAM Total Memory (B)":"51539607552"}
			}`,
			want: []Device{{Name: "AMD Radeon Pro W7900", TotalVRAM: 51539607552, FreeVRAM: 0, Load: 0}},
		},
		{
			name: "used greater than total leaves free zero",
			out:  `{"card0":{"Card series":"X","VRAM Total Memory (B)":"100","VRAM Total Used Memory (B)":"200"}}`,
			want: []Device{{Name: "X", TotalVRAM: 100, FreeVRAM: 0, Load: 0}},
		},
		{
			name: "load over 100 clamped",
			out:  `{"card0":{"Card series":"X","VRAM Total Memory (B)":"100","GPU use (%)":"150"}}`,
			want: []Device{{Name: "X", TotalVRAM: 100, FreeVRAM: 0, Load: 100}},
		},
		{
			name:    "malformed json is an error",
			out:     `{not json`,
			wantErr: true,
		},
		{
			name:    "empty output is an error",
			out:     "  ",
			wantErr: true,
		},
		{
			name: "no card entries yields no devices",
			out:  `{"system":{"Driver version":"6.0.0"}}`,
			want: nil,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseRocmSMI([]byte(tc.out))
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

// TestParseAmdSMI exercises the amd-smi JSON-array parser across the unit
// variants its vram block has used (object with value+unit, bare number, GB).
func TestParseAmdSMI(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		out     string
		want    []Device
		wantErr bool
	}{
		{
			name: "value+unit MB",
			out:  `[{"asic":{"market_name":"Instinct MI300X"},"vram":{"total":{"value":196608,"unit":"MB"}}}]`,
			want: []Device{{Name: "Instinct MI300X", TotalVRAM: 196608 * mib}},
		},
		{
			name: "value+unit GB",
			out:  `[{"asic":{"market_name":"Radeon RX 7800 XT"},"vram":{"size":{"value":16,"unit":"GB"}}}]`,
			want: []Device{{Name: "Radeon RX 7800 XT", TotalVRAM: 16 * 1024 * 1024 * 1024}},
		},
		{
			name: "bare number defaults to MiB",
			out:  `[{"asic":{"market_name":"Radeon"},"vram":{"size":24576}}]`,
			want: []Device{{Name: "Radeon", TotalVRAM: 24576 * mib}},
		},
		{
			name: "multiple gpus",
			out: `[
			  {"asic":{"market_name":"MI300X"},"vram":{"total":{"value":192,"unit":"GB"}}},
			  {"asic":{"market_name":"MI300X"},"vram":{"total":{"value":192,"unit":"GB"}}}
			]`,
			want: []Device{
				{Name: "MI300X", TotalVRAM: 192 * 1024 * 1024 * 1024},
				{Name: "MI300X", TotalVRAM: 192 * 1024 * 1024 * 1024},
			},
		},
		{
			name:    "not an array is an error",
			out:     `{"asic":{}}`,
			wantErr: true,
		},
		{
			name: "empty array yields no devices",
			out:  `[]`,
			want: nil,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseAmdSMI([]byte(tc.out))
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

// TestDetectAMDPrefersAmdSmiThenFallsBack verifies the vendor-tool preference:
// amd-smi is used when it yields devices; otherwise rocm-smi supplies them
// (including the free VRAM and load amd-smi static omits).
func TestDetectAMDPrefersAmdSmiThenFallsBack(t *testing.T) {
	t.Parallel()

	t.Run("amd-smi used when usable", func(t *testing.T) {
		t.Parallel()
		rocmCalled := false
		runner := func(_ context.Context, name string, _ ...string) ([]byte, error) {
			switch name {
			case amdSMI:
				return []byte(`[{"asic":{"market_name":"MI300X"},"vram":{"total":{"value":192,"unit":"GB"}}}]`), nil
			case rocmSMI:
				rocmCalled = true
				return nil, exec.ErrNotFound
			default:
				return nil, exec.ErrNotFound
			}
		}
		d := NewDetector(withGOOS("linux"), WithRunner(runner), WithLogger(quietLogger()))
		devs, err := d.detectAMD(context.Background())
		if err != nil {
			t.Fatalf("detectAMD err: %v", err)
		}
		assertDevices(t, devs, []Device{{Name: "MI300X", TotalVRAM: 192 * 1024 * 1024 * 1024}})
		if rocmCalled {
			t.Fatalf("rocm-smi must not be called when amd-smi yields devices")
		}
	})

	t.Run("falls back to rocm-smi when amd-smi absent", func(t *testing.T) {
		t.Parallel()
		d := newTestDetector(t, "linux", map[string]fakeResponse{
			// amd-smi absent; rocm-smi present with full signal.
			rocmSMI: {out: `{"card0":{"Card series":"MI210","VRAM Total Memory (B)":"100","VRAM Total Used Memory (B)":"40","GPU use (%)":"25"}}`},
		})
		devs, err := d.detectAMD(context.Background())
		if err != nil {
			t.Fatalf("detectAMD err: %v", err)
		}
		assertDevices(t, devs, []Device{{Name: "MI210", TotalVRAM: 100, FreeVRAM: 60, Load: 25}})
	})
}
