package gpu

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// mib is one mebibyte in bytes. nvidia-smi (with nounits) reports memory in
// MiB, so per-device VRAM is converted to bytes by multiplying by this.
const mib = 1024 * 1024

// nvidiaSMI is the NVIDIA management CLI, shipped with the NVIDIA driver on both
// Linux and Windows. It is resolved on PATH by the Runner.
const nvidiaSMI = "nvidia-smi"

// detectNVIDIA runs nvidia-smi and parses its per-GPU CSV. A missing nvidia-smi
// (no NVIDIA driver) surfaces as a tool-not-found error the Detector treats as
// "vendor absent". The query selects exactly the fields we report — name, total
// and free memory — as headerless, unit-less CSV so parsing is unambiguous.
func (d *Detector) detectNVIDIA(ctx context.Context) ([]Device, error) {
	out, err := d.runner(ctx, nvidiaSMI,
		"--query-gpu=name,memory.total,memory.free,utilization.gpu",
		"--format=csv,noheader,nounits")
	if err != nil {
		return nil, err
	}
	return parseNvidiaSMI(out)
}

// parseNvidiaSMI parses the CSV output of
//
//	nvidia-smi --query-gpu=name,memory.total,memory.free,utilization.gpu \
//	           --format=csv,noheader,nounits
//
// One line per GPU, comma-separated: name, memory.total (MiB), memory.free
// (MiB), utilization.gpu (percent). memory.* are converted from MiB to bytes.
// utilization may be absent or reported as "[N/A]" on some GPUs; it then
// defaults to 0. Blank lines are skipped. A line that has no parseable numeric
// memory is an error (malformed output), so a corrupt result degrades to CPU
// rather than silently reporting a phantom zero-VRAM GPU.
func parseNvidiaSMI(out []byte) ([]Device, error) {
	var devices []Device
	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 3 {
			return nil, fmt.Errorf("gpu: malformed nvidia-smi line %q: want at least name,total,free", line)
		}
		name := strings.TrimSpace(fields[0])
		totalMiB, err := parseUintField(fields[1])
		if err != nil {
			return nil, fmt.Errorf("gpu: nvidia-smi memory.total %q: %w", strings.TrimSpace(fields[1]), err)
		}
		freeMiB, err := parseUintField(fields[2])
		if err != nil {
			return nil, fmt.Errorf("gpu: nvidia-smi memory.free %q: %w", strings.TrimSpace(fields[2]), err)
		}
		dev := Device{
			Name:      name,
			TotalVRAM: totalMiB * mib,
			FreeVRAM:  freeMiB * mib,
		}
		// utilization.gpu is best-effort: absent or "[N/A]" leaves Load at 0.
		if len(fields) >= 4 {
			if util, err := parseUintField(fields[3]); err == nil {
				if util > 100 {
					util = 100
				}
				dev.Load = uint32(util)
			}
		}
		devices = append(devices, dev)
	}
	return devices, nil
}

// parseUintField parses a trimmed unsigned integer from a CSV field. nvidia-smi
// with nounits emits bare integers; this tolerates surrounding whitespace.
func parseUintField(field string) (uint64, error) {
	return strconv.ParseUint(strings.TrimSpace(field), 10, 64)
}
