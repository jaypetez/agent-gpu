// Package gpu detects local GPU accelerators and reports their capacity
// (vendor/type, total and free VRAM in bytes, and a coarse 0–100 load) so the
// worker can fold real hardware capacity into its heartbeats. The server's
// capacity-aware scheduler (#9) consumes free VRAM and load; total VRAM and the
// type string are observability signals in the fleet view.
//
// Detection is deliberately cgo-free and single-binary friendly: rather than
// linking NVML/CUDA/ROCm/Metal native libraries, it shells out to the vendors'
// own command-line tools (nvidia-smi, rocm-smi/amd-smi, system_profiler) and
// parses their output. This keeps GOOS/GOARCH cross-compilation a one-liner and
// the binary static (see docs/architecture.md "Language & runtime"). The probes
// are gated by runtime.GOOS so each only runs where the tool can exist.
//
// Detection never fails the caller: a host with no GPU, a missing vendor tool,
// or a flaky probe degrades cleanly to the CPU fallback (Type "cpu", zero VRAM).
// This mirrors the worker's backend probe, which logs and continues degraded
// rather than wedging startup. VRAM is always normalized to bytes regardless of
// the units the underlying tool reports.
package gpu

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"
	"sort"
	"strings"
)

// Capacity is the aggregated GPU capacity of a host, the shape the worker copies
// into its heartbeat. All VRAM is in bytes; Load is a coarse 0–100 utilization.
// The zero/CPU-fallback value is Capacity{Type: "cpu"} with zero VRAM and load.
type Capacity struct {
	// Type is a free-form, human-readable accelerator description, e.g.
	// "NVIDIA GeForce RTX 4090", "2x NVIDIA GeForce RTX 4090", "Apple M3 Pro",
	// or "cpu" when no GPU is present. It is observability-only on the server.
	Type string
	// TotalVRAM is the summed total video memory across all detected devices, in
	// bytes. Zero on a CPU-only host.
	TotalVRAM uint64
	// FreeVRAM is the summed currently-free video memory across all detected
	// devices, in bytes. The scheduler routes not-yet-loaded models only to
	// workers reporting FreeVRAM > 0, so a CPU-only host (FreeVRAM 0) is not a
	// candidate for a cold model load — matching the existing fit approximation.
	FreeVRAM uint64
	// Load is the mean device utilization across detected devices, 0–100.
	Load uint32
}

// CPUType is the Type reported when no GPU accelerator is detected.
const CPUType = "cpu"

// cpuFallback is the capacity reported when no vendor reports a device: a
// CPU-only host that still registers and heartbeats, just with no VRAM to offer.
func cpuFallback() Capacity { return Capacity{Type: CPUType} }

// Device is a single detected accelerator, before aggregation into a Capacity.
// Vendor probes parse their tool output into Devices; the Detector aggregates
// them (summing VRAM, averaging load, combining the names). VRAM is in bytes.
type Device struct {
	// Name is the device's marketing/product name, e.g. "NVIDIA GeForce RTX 4090".
	Name string
	// TotalVRAM is the device's total video memory in bytes.
	TotalVRAM uint64
	// FreeVRAM is the device's currently-free video memory in bytes.
	FreeVRAM uint64
	// Load is the device's utilization, 0–100. Probes that cannot read a
	// per-device utilization leave this zero.
	Load uint32
}

// Runner runs an external command and returns its combined standard output. It
// is the unit-test seam for the whole package (analogous to ollama's
// WithHTTPClient): tests inject a Runner that returns captured fixture output so
// detection is exercised without a real GPU or any subprocess. The default
// runner (execRunner) wraps exec.CommandContext(...).Output().
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// execRunner is the default Runner: it resolves name on PATH and runs it,
// returning stdout. A tool that is not installed surfaces as an error wrapping
// exec.ErrNotFound, which the vendor probes treat as "vendor absent" (non-fatal)
// rather than a real failure.
func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// errToolNotFound reports whether err indicates the command itself could not be
// found/started (vs. ran and exited non-zero). A not-found tool means the vendor
// is simply not present on this host, which is expected and non-fatal.
func errToolNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	// exec.Error wraps the lookup failure (e.g. "executable file not found in
	// $PATH"); its Unwrap is exec.ErrNotFound, caught above, but guard the type
	// too for robustness across platforms.
	var execErr *exec.Error
	return errors.As(err, &execErr)
}

// Detector probes the host's GPUs and returns a single aggregated Capacity. It
// is safe for concurrent use; construct it with NewDetector.
type Detector struct {
	runner Runner
	logger *slog.Logger
	// goos overrides runtime.GOOS for tests; empty means use runtime.GOOS.
	goos string
}

// Option configures a Detector.
type Option func(*Detector)

// WithRunner overrides the command runner. A nil runner is ignored. This is the
// primary test seam — inject a Runner that returns fixture output.
func WithRunner(r Runner) Option {
	return func(d *Detector) {
		if r != nil {
			d.runner = r
		}
	}
}

// WithLogger overrides the logger. A nil logger is ignored.
func WithLogger(l *slog.Logger) Option {
	return func(d *Detector) {
		if l != nil {
			d.logger = l
		}
	}
}

// withGOOS overrides the OS gate (test-only).
func withGOOS(goos string) Option {
	return func(d *Detector) { d.goos = goos }
}

// NewDetector constructs a Detector. With no options it uses the real exec-based
// runner, slog.Default(), and the host's runtime.GOOS.
func NewDetector(opts ...Option) *Detector {
	d := &Detector{
		runner: execRunner,
		logger: slog.Default(),
		goos:   runtime.GOOS,
	}
	for _, opt := range opts {
		opt(d)
	}
	if d.runner == nil {
		d.runner = execRunner
	}
	if d.logger == nil {
		d.logger = slog.Default()
	}
	if d.goos == "" {
		d.goos = runtime.GOOS
	}
	return d
}

// probe pairs a vendor name with the function that detects its devices. Detect
// tries them in order and takes the first that reports ≥1 device.
type probe struct {
	vendor  string
	devices func(ctx context.Context) ([]Device, error)
}

// probes returns the vendor probes applicable to this Detector's OS, in priority
// order. NVIDIA is tried first (it is the common discrete-GPU case on the Linux
// and Windows hosts agent-gpu targets), then AMD on Linux, then Apple on macOS.
func (d *Detector) probes() []probe {
	switch d.goos {
	case "linux":
		return []probe{
			{vendor: "nvidia", devices: d.detectNVIDIA},
			{vendor: "amd", devices: d.detectAMD},
		}
	case "windows":
		// nvidia-smi ships with the NVIDIA Windows driver. ROCm tooling is not a
		// supported Windows path here, so AMD is Linux-only.
		return []probe{
			{vendor: "nvidia", devices: d.detectNVIDIA},
		}
	case "darwin":
		return []probe{
			{vendor: "apple", devices: d.detectApple},
		}
	default:
		return nil
	}
}

// Detect probes the host and returns its aggregated GPU Capacity. It tries each
// applicable vendor in order and uses the first that reports at least one
// device, aggregating multiple devices into one Capacity. If no vendor reports a
// device — no GPU, tools absent, or every probe errored — it returns the CPU
// fallback. Detect never returns an error and never panics: any trouble degrades
// to CPU so the caller (the worker heartbeat) always has a usable Capacity.
func (d *Detector) Detect(ctx context.Context) Capacity {
	for _, p := range d.probes() {
		devices, err := p.devices(ctx)
		if err != nil {
			// A probe error is expected on hosts without that vendor (tool missing)
			// and merely logged otherwise; never fatal. Keep trying other vendors.
			if errToolNotFound(err) {
				d.logger.Debug("gpu vendor tool not present", "vendor", p.vendor)
			} else {
				d.logger.Debug("gpu probe failed", "vendor", p.vendor, "err", err)
			}
			continue
		}
		if len(devices) == 0 {
			continue
		}
		cap := aggregate(devices)
		d.logger.Info("detected gpu", "vendor", p.vendor, "type", cap.Type,
			"devices", len(devices), "total_vram_bytes", cap.TotalVRAM, "free_vram_bytes", cap.FreeVRAM)
		return cap
	}
	d.logger.Info("no gpu detected; running in cpu mode")
	return cpuFallback()
}

// aggregate folds one or more devices into a single Capacity: total and free
// VRAM are summed, load is the mean (rounded to nearest, 0–100), and the type is
// a combined human-readable description. An empty slice yields the CPU fallback.
func aggregate(devices []Device) Capacity {
	if len(devices) == 0 {
		return cpuFallback()
	}
	cap := Capacity{Type: combineNames(devices)}
	var loadSum uint64
	for _, dev := range devices {
		cap.TotalVRAM += dev.TotalVRAM
		cap.FreeVRAM += dev.FreeVRAM
		loadSum += uint64(dev.Load)
	}
	// Mean load, rounded to nearest, clamped to 100.
	mean := (loadSum + uint64(len(devices))/2) / uint64(len(devices))
	if mean > 100 {
		mean = 100
	}
	cap.Load = uint32(mean)
	return cap
}

// combineNames builds a human-readable Type from the device names. A single
// device uses its name verbatim. Multiple identical devices collapse to a
// count-prefixed form ("2x NVIDIA GeForce RTX 4090"). A heterogeneous mix lists
// the distinct names, each count-prefixed, joined with " + "
// ("1x NVIDIA A100 + 1x NVIDIA T4"). Blank names are normalized to "GPU".
func combineNames(devices []Device) string {
	if len(devices) == 0 {
		return CPUType
	}
	// Count occurrences, preserving first-seen order for stable output.
	counts := make(map[string]int)
	var order []string
	for _, dev := range devices {
		name := strings.TrimSpace(dev.Name)
		if name == "" {
			name = "GPU"
		}
		if _, seen := counts[name]; !seen {
			order = append(order, name)
		}
		counts[name]++
	}
	if len(order) == 1 {
		name := order[0]
		if counts[name] == 1 {
			return name
		}
		return fmt.Sprintf("%dx %s", counts[name], name)
	}
	// Heterogeneous: sort distinct names for deterministic output, then prefix
	// each with its count.
	sort.Strings(order)
	parts := make([]string, 0, len(order))
	for _, name := range order {
		parts = append(parts, fmt.Sprintf("%dx %s", counts[name], name))
	}
	return strings.Join(parts, " + ")
}
