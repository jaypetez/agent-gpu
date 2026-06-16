package gpu

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"testing"
)

// quietLogger returns a logger that discards output so tests stay clean.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeRunner builds a Runner from a map of command name -> (output, error).
// Unknown commands return exec.ErrNotFound (vendor absent), the realistic
// default for a host that lacks a given vendor's CLI.
func fakeRunner(t *testing.T, responses map[string]fakeResponse) Runner {
	t.Helper()
	return func(_ context.Context, name string, _ ...string) ([]byte, error) {
		resp, ok := responses[name]
		if !ok {
			return nil, exec.ErrNotFound
		}
		return []byte(resp.out), resp.err
	}
}

type fakeResponse struct {
	out string
	err error
}

// newTestDetector builds a Detector pinned to goos with the given fake runner.
func newTestDetector(t *testing.T, goos string, responses map[string]fakeResponse) *Detector {
	t.Helper()
	return NewDetector(
		withGOOS(goos),
		WithRunner(fakeRunner(t, responses)),
		WithLogger(quietLogger()),
	)
}

// TestDetectCPUFallbackNoTools covers the core AC: a host with no GPU tooling
// runs in CPU mode without error or panic. The runner reports every tool as
// not-found.
func TestDetectCPUFallbackNoTools(t *testing.T) {
	t.Parallel()
	for _, goos := range []string{"linux", "windows", "darwin", "freebsd"} {
		goos := goos
		t.Run(goos, func(t *testing.T) {
			t.Parallel()
			d := newTestDetector(t, goos, nil) // empty -> everything ErrNotFound
			got := d.Detect(context.Background())
			want := Capacity{Type: CPUType}
			if got != want {
				t.Fatalf("goos %s: Detect() = %+v, want CPU fallback %+v", goos, got, want)
			}
		})
	}
}

// TestDetectNVIDIASingle verifies an NVIDIA single-GPU host is identified with
// VRAM normalized from MiB to bytes and the load carried through.
func TestDetectNVIDIASingle(t *testing.T) {
	t.Parallel()
	d := newTestDetector(t, "linux", map[string]fakeResponse{
		nvidiaSMI: {out: "NVIDIA GeForce RTX 4090, 24564, 24268, 13\n"},
	})
	got := d.Detect(context.Background())
	if got.Type != "NVIDIA GeForce RTX 4090" {
		t.Fatalf("Type = %q", got.Type)
	}
	if want := uint64(24564) * mib; got.TotalVRAM != want {
		t.Fatalf("TotalVRAM = %d, want %d", got.TotalVRAM, want)
	}
	if want := uint64(24268) * mib; got.FreeVRAM != want {
		t.Fatalf("FreeVRAM = %d, want %d", got.FreeVRAM, want)
	}
	if got.Load != 13 {
		t.Fatalf("Load = %d, want 13", got.Load)
	}
}

// TestDetectMultiGPUAggregation covers the multi-GPU AC: VRAM totals are summed,
// the type string reflects the count, and load is averaged.
func TestDetectMultiGPUAggregation(t *testing.T) {
	t.Parallel()
	d := newTestDetector(t, "linux", map[string]fakeResponse{
		nvidiaSMI: {out: "" +
			"NVIDIA GeForce RTX 4090, 24564, 20000, 10\n" +
			"NVIDIA GeForce RTX 4090, 24564, 24564, 30\n"},
	})
	got := d.Detect(context.Background())
	if got.Type != "2x NVIDIA GeForce RTX 4090" {
		t.Fatalf("Type = %q, want 2x ...", got.Type)
	}
	if want := uint64(24564+24564) * mib; got.TotalVRAM != want {
		t.Fatalf("TotalVRAM = %d, want %d", got.TotalVRAM, want)
	}
	if want := uint64(20000+24564) * mib; got.FreeVRAM != want {
		t.Fatalf("FreeVRAM = %d, want %d", got.FreeVRAM, want)
	}
	if got.Load != 20 { // mean(10,30)
		t.Fatalf("Load = %d, want 20", got.Load)
	}
}

// TestDetectVendorPriorityAndFirstNonEmpty verifies the detector takes the first
// vendor that reports a device. With nvidia present it must not consult AMD even
// if AMD would also report something.
func TestDetectVendorPriorityAndFirstNonEmpty(t *testing.T) {
	t.Parallel()
	amdCalled := false
	runner := func(_ context.Context, name string, _ ...string) ([]byte, error) {
		switch name {
		case nvidiaSMI:
			return []byte("NVIDIA A100, 40960, 40000, 5\n"), nil
		case amdSMI, rocmSMI:
			amdCalled = true
			return []byte(`{"card0":{"Card series":"MI300","VRAM Total Memory (B)":"1"}}`), nil
		default:
			return nil, exec.ErrNotFound
		}
	}
	d := NewDetector(withGOOS("linux"), WithRunner(runner), WithLogger(quietLogger()))
	got := d.Detect(context.Background())
	if got.Type != "NVIDIA A100" {
		t.Fatalf("Type = %q, want NVIDIA A100 (nvidia has priority)", got.Type)
	}
	if amdCalled {
		t.Fatalf("AMD probe should not run once NVIDIA reported a device")
	}
}

// TestDetectFallsThroughToNextVendor verifies that when the first vendor's tool
// is absent, detection proceeds to the next vendor rather than giving up.
func TestDetectFallsThroughToNextVendor(t *testing.T) {
	t.Parallel()
	d := newTestDetector(t, "linux", map[string]fakeResponse{
		// nvidia-smi absent (not in map -> ErrNotFound); rocm-smi present.
		rocmSMI: {out: `{"card0":{"Card series":"Instinct MI210","VRAM Total Memory (B)":"68702699520","VRAM Total Used Memory (B)":"10747904","GPU use (%)":"7"}}`},
	})
	got := d.Detect(context.Background())
	if got.Type != "Instinct MI210" {
		t.Fatalf("Type = %q, want Instinct MI210 (fell through to AMD)", got.Type)
	}
	if got.TotalVRAM != 68702699520 {
		t.Fatalf("TotalVRAM = %d", got.TotalVRAM)
	}
}

// TestDetectGracefulDegradationOnRuntimeError covers the graceful-degradation
// AC: a probe that errors at runtime (tool present but fails, e.g. timeout) must
// not propagate a fatal — Detect returns a usable Capacity (CPU fallback here,
// since the only vendor errored).
func TestDetectGracefulDegradationOnRuntimeError(t *testing.T) {
	t.Parallel()
	boom := errors.New("nvidia-smi: driver/library version mismatch")
	d := newTestDetector(t, "linux", map[string]fakeResponse{
		nvidiaSMI: {err: boom},
		// amd tools absent.
	})
	got := d.Detect(context.Background())
	if got != (Capacity{Type: CPUType}) {
		t.Fatalf("Detect() = %+v, want CPU fallback on runtime error", got)
	}
}

// TestDetectMalformedOutputDegradesToCPU verifies malformed vendor output is an
// error that degrades to CPU (the only vendor) rather than reporting a phantom
// device or panicking.
func TestDetectMalformedOutputDegradesToCPU(t *testing.T) {
	t.Parallel()
	d := newTestDetector(t, "linux", map[string]fakeResponse{
		nvidiaSMI: {out: "this is not csv, , , , garbage\nNVIDIA, not-a-number, 100\n"},
	})
	got := d.Detect(context.Background())
	if got != (Capacity{Type: CPUType}) {
		t.Fatalf("Detect() = %+v, want CPU fallback on malformed output", got)
	}
}

// TestErrToolNotFound checks the not-found classifier used to treat missing
// vendor tools as "absent" rather than fatal.
func TestErrToolNotFound(t *testing.T) {
	t.Parallel()
	if !errToolNotFound(exec.ErrNotFound) {
		t.Fatalf("exec.ErrNotFound should be classified as tool-not-found")
	}
	if !errToolNotFound(&exec.Error{Name: "nvidia-smi", Err: exec.ErrNotFound}) {
		t.Fatalf("*exec.Error should be classified as tool-not-found")
	}
	if errToolNotFound(errors.New("ran but exited 1")) {
		t.Fatalf("a generic runtime error must not be tool-not-found")
	}
	if errToolNotFound(nil) {
		t.Fatalf("nil must not be tool-not-found")
	}
}

// TestAggregateLoadRoundingAndClamp checks mean-load rounding and the 100 clamp.
func TestAggregateLoadRoundingAndClamp(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		devices []Device
		want    uint32
	}{
		{"single", []Device{{Load: 42}}, 42},
		{"rounds up", []Device{{Load: 10}, {Load: 11}}, 11},   // mean 10.5 -> 11
		{"rounds down", []Device{{Load: 10}, {Load: 13}}, 12}, // mean 11.5 -> 12 (round half up)
		{"clamp", []Device{{Load: 200}}, 100},
		{"empty -> cpu zero", nil, 0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := aggregate(tc.devices)
			if got.Load != tc.want {
				t.Fatalf("aggregate(%v).Load = %d, want %d", tc.devices, got.Load, tc.want)
			}
		})
	}
}

// TestCombineNames covers the type-string construction for single, homogeneous
// multi, heterogeneous multi, and blank-name cases.
func TestCombineNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		devices []Device
		want    string
	}{
		{"single", []Device{{Name: "NVIDIA RTX 4090"}}, "NVIDIA RTX 4090"},
		{"homogeneous", []Device{{Name: "NVIDIA RTX 4090"}, {Name: "NVIDIA RTX 4090"}}, "2x NVIDIA RTX 4090"},
		{
			"heterogeneous",
			[]Device{{Name: "NVIDIA A100"}, {Name: "NVIDIA T4"}},
			"1x NVIDIA A100 + 1x NVIDIA T4",
		},
		{"blank normalized", []Device{{Name: ""}}, "GPU"},
		{"empty -> cpu", nil, CPUType},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := combineNames(tc.devices); got != tc.want {
				t.Fatalf("combineNames(%v) = %q, want %q", tc.devices, got, tc.want)
			}
		})
	}
}

// TestNewDetectorNilSafety verifies the constructor tolerates nil options and
// nil runner/logger overrides without panicking, defaulting sensibly.
func TestNewDetectorNilSafety(t *testing.T) {
	t.Parallel()
	d := NewDetector(WithRunner(nil), WithLogger(nil))
	if d.runner == nil || d.logger == nil || d.goos == "" {
		t.Fatalf("NewDetector left a nil field: runner=%v logger=%v goos=%q", d.runner == nil, d.logger == nil, d.goos)
	}
}
