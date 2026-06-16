package gpu

import (
	"context"
	"os/exec"
	"testing"
)

// Apple Silicon system_profiler output: a single GPU entry with sppci_model set
// to the chipset name and NO discrete VRAM field (the GPU uses unified memory).
const appleSiliconSP = `{
  "SPDisplaysDataType": [
    {
      "_name": "Apple M3 Pro",
      "sppci_model": "Apple M3 Pro",
      "spdisplays_mtlgpufamilysupport": "spdisplays_metal3",
      "sppci_cores": "18"
    }
  ]
}`

// Intel Mac system_profiler output: a discrete GPU with spdisplays_vram "8 GB".
const intelDiscreteSP = `{
  "SPDisplaysDataType": [
    {
      "_name": "Radeon Pro 5500M",
      "sppci_model": "AMD Radeon Pro 5500M",
      "spdisplays_vram": "8 GB"
    }
  ]
}`

// TestParseSystemProfiler covers both Apple-Silicon (unified, needs memsize) and
// Intel-discrete (VRAM string parsed to bytes) shapes, plus the shared-VRAM
// field, MB units, empty list, and malformed JSON.
func TestParseSystemProfiler(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		out         string
		want        []Device
		wantNeedMem bool
		wantErr     bool
	}{
		{
			name:        "apple silicon needs unified memory",
			out:         appleSiliconSP,
			want:        []Device{{Name: "Apple M3 Pro"}},
			wantNeedMem: true,
		},
		{
			name: "intel discrete vram GB",
			out:  intelDiscreteSP,
			want: []Device{{Name: "AMD Radeon Pro 5500M", TotalVRAM: 8 * 1024 * 1024 * 1024, FreeVRAM: 8 * 1024 * 1024 * 1024}},
		},
		{
			name: "shared vram field used when dedicated absent",
			out:  `{"SPDisplaysDataType":[{"sppci_model":"Intel Iris","spdisplays_vram_shared":"1536 MB"}]}`,
			want: []Device{{Name: "Intel Iris", TotalVRAM: 1536 * mib, FreeVRAM: 1536 * mib}},
		},
		{
			name:        "falls back to _name when sppci_model absent",
			out:         `{"SPDisplaysDataType":[{"_name":"Apple M1"}]}`,
			want:        []Device{{Name: "Apple M1"}},
			wantNeedMem: true,
		},
		{
			name: "empty display list yields no devices",
			out:  `{"SPDisplaysDataType":[]}`,
			want: nil,
		},
		{
			name:    "malformed json is an error",
			out:     `{not json`,
			wantErr: true,
		},
		{
			name:    "empty output is an error",
			out:     "",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, needMem, err := parseSystemProfiler([]byte(tc.out))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got devices %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if needMem != tc.wantNeedMem {
				t.Fatalf("needUnifiedMemory = %v, want %v", needMem, tc.wantNeedMem)
			}
			assertDevices(t, got, tc.want)
		})
	}
}

// TestParseAppleVRAM covers the GB/MB unit handling and the unrecognized case
// (which signals "no discrete VRAM" for the Apple-Silicon path).
func TestParseAppleVRAM(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in     string
		want   uint64
		wantOK bool
	}{
		{"8 GB", 8 * 1024 * 1024 * 1024, true},
		{"1536 MB", 1536 * mib, true},
		{"512 MB", 512 * mib, true},
		{"  4 GB  ", 4 * 1024 * 1024 * 1024, true},
		{"", 0, false},
		{"unknown", 0, false},
		{"8", 0, false},
		{"8 TB", 0, false},
		{"abc GB", 0, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, ok := parseAppleVRAM(tc.in)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("parseAppleVRAM(%q) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestParseMemsize covers the sysctl hw.memsize decimal-string parsing.
func TestParseMemsize(t *testing.T) {
	t.Parallel()
	got, err := parseMemsize([]byte("38654705664\n"))
	if err != nil || got != 38654705664 {
		t.Fatalf("parseMemsize = (%d, %v), want 38654705664", got, err)
	}
	if _, err := parseMemsize([]byte("  ")); err == nil {
		t.Fatalf("empty memsize should error")
	}
	if _, err := parseMemsize([]byte("not-a-number")); err == nil {
		t.Fatalf("non-numeric memsize should error")
	}
}

// TestDetectAppleSiliconUsesUnifiedMemory verifies the end-to-end Apple Silicon
// path: no discrete VRAM, so total and free are filled from hw.memsize.
func TestDetectAppleSiliconUsesUnifiedMemory(t *testing.T) {
	t.Parallel()
	const memBytes = 38654705664 // 36 GiB
	d := newTestDetector(t, "darwin", map[string]fakeResponse{
		systemProfiler: {out: appleSiliconSP},
		sysctl:         {out: "38654705664\n"},
	})
	got := d.Detect(context.Background())
	if got.Type != "Apple M3 Pro" {
		t.Fatalf("Type = %q, want Apple M3 Pro", got.Type)
	}
	if got.TotalVRAM != memBytes || got.FreeVRAM != memBytes {
		t.Fatalf("unified VRAM total=%d free=%d, want %d/%d", got.TotalVRAM, got.FreeVRAM, memBytes, memBytes)
	}
}

// TestDetectAppleSiliconMemsizeFailureStillSchedulable verifies that if memsize
// can't be read, detection still succeeds (the Mac is identified) with zero
// VRAM rather than erroring — graceful degradation within the Apple path.
func TestDetectAppleSiliconMemsizeFailureStillSchedulable(t *testing.T) {
	t.Parallel()
	d := newTestDetector(t, "darwin", map[string]fakeResponse{
		systemProfiler: {out: appleSiliconSP},
		// sysctl absent -> ErrNotFound; memsize stays 0.
	})
	got := d.Detect(context.Background())
	if got.Type != "Apple M3 Pro" {
		t.Fatalf("Type = %q, want Apple M3 Pro even without memsize", got.Type)
	}
	if got.TotalVRAM != 0 || got.FreeVRAM != 0 {
		t.Fatalf("expected zero VRAM when memsize unavailable, got total=%d free=%d", got.TotalVRAM, got.FreeVRAM)
	}
}

// TestDetectIntelMacDiscreteVRAM verifies the Intel discrete-GPU path reports
// the parsed VRAM and never consults sysctl (discrete VRAM is known).
func TestDetectIntelMacDiscreteVRAM(t *testing.T) {
	t.Parallel()
	sysctlCalled := false
	runner := func(_ context.Context, name string, _ ...string) ([]byte, error) {
		switch name {
		case systemProfiler:
			return []byte(intelDiscreteSP), nil
		case sysctl:
			sysctlCalled = true
			return []byte("17179869184\n"), nil
		default:
			return nil, exec.ErrNotFound
		}
	}
	d := NewDetector(withGOOS("darwin"), WithRunner(runner), WithLogger(quietLogger()))
	got := d.Detect(context.Background())
	if want := uint64(8) * 1024 * 1024 * 1024; got.TotalVRAM != want {
		t.Fatalf("TotalVRAM = %d, want %d", got.TotalVRAM, want)
	}
	if sysctlCalled {
		t.Fatalf("sysctl must not be consulted when discrete VRAM is present")
	}
}
