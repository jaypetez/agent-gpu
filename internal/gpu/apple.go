package gpu

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Apple GPUs are detected via system_profiler's display data, with host memory
// read via sysctl for the unified-memory case.
const (
	systemProfiler = "system_profiler"
	sysctl         = "sysctl"
)

// detectApple probes for an Apple GPU via system_profiler. On Apple Silicon the
// GPU shares the system's unified memory, so there is no discrete VRAM field; we
// report the host's physical memory (hw.memsize) as both total and free so the
// Mac stays schedulable. On Intel Macs with a discrete/integrated GPU the VRAM
// is parsed from the reported "N GB"/"N MB" string. A missing system_profiler
// (non-macOS, though this path is GOOS-gated to darwin) surfaces as
// tool-not-found.
func (d *Detector) detectApple(ctx context.Context) ([]Device, error) {
	out, err := d.runner(ctx, systemProfiler, "SPDisplaysDataType", "-json")
	if err != nil {
		return nil, err
	}
	devices, needMem, err := parseSystemProfiler(out)
	if err != nil {
		return nil, err
	}
	// For any device lacking a discrete VRAM figure (Apple Silicon unified
	// memory), fill total/free from hw.memsize so the worker still advertises
	// usable capacity. We read memsize once and apply it to every such device.
	if needMem {
		if memBytes := d.appleUnifiedMemory(ctx); memBytes > 0 {
			for i := range devices {
				if devices[i].TotalVRAM == 0 {
					devices[i].TotalVRAM = memBytes
					devices[i].FreeVRAM = memBytes
				}
			}
		}
	}
	return devices, nil
}

// appleUnifiedMemory reads the host's physical memory via `sysctl -n hw.memsize`
// (bytes, as a decimal string). A failure returns 0; the caller then leaves the
// Apple Silicon device's VRAM at zero rather than failing detection.
func (d *Detector) appleUnifiedMemory(ctx context.Context) uint64 {
	out, err := d.runner(ctx, sysctl, "-n", "hw.memsize")
	if err != nil {
		d.logger.Debug("sysctl hw.memsize failed", "err", err)
		return 0
	}
	mem, err := parseMemsize(out)
	if err != nil {
		d.logger.Debug("parse hw.memsize failed", "err", err)
		return 0
	}
	return mem
}

// spDisplays mirrors the subset of `system_profiler SPDisplaysDataType -json` we
// consume: a top-level object with an SPDisplaysDataType array of GPU entries.
type spDisplays struct {
	Displays []spDisplay `json:"SPDisplaysDataType"`
}

// spDisplay is one GPU entry. Field names follow system_profiler's JSON keys.
// sppci_model is the chipset/marketing name on both Apple Silicon and Intel;
// _name is an older fallback. spdisplays_vram / spdisplays_vram_shared carry a
// human VRAM string ("8 GB") on Macs that expose discrete VRAM; Apple Silicon
// omits them (the GPU uses unified memory).
type spDisplay struct {
	Model       string `json:"sppci_model"`
	Name        string `json:"_name"`
	VRAM        string `json:"spdisplays_vram"`
	VRAMShared  string `json:"spdisplays_vram_shared"`
	CoreCount   string `json:"sppci_cores"`
	MetalFamily string `json:"spdisplays_mtlgpufamilysupport"`
}

// parseSystemProfiler parses the SPDisplaysDataType JSON into Devices. It
// returns the devices, a flag indicating whether any device lacked a discrete
// VRAM figure (so the caller fills it from unified memory), and an error only
// when the JSON is unparseable. An empty display list yields no devices and no
// error (the host simply has no Apple GPU entry).
func parseSystemProfiler(out []byte) (devices []Device, needUnifiedMemory bool, err error) {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, false, fmt.Errorf("gpu: empty system_profiler output")
	}
	var sp spDisplays
	if err := json.Unmarshal([]byte(trimmed), &sp); err != nil {
		return nil, false, fmt.Errorf("gpu: parse system_profiler json: %w", err)
	}
	for _, disp := range sp.Displays {
		name := strings.TrimSpace(disp.Model)
		if name == "" {
			name = strings.TrimSpace(disp.Name)
		}
		dev := Device{Name: name}
		// Prefer the dedicated VRAM field, then the shared one.
		vramStr := disp.VRAM
		if strings.TrimSpace(vramStr) == "" {
			vramStr = disp.VRAMShared
		}
		if bytes, ok := parseAppleVRAM(vramStr); ok {
			dev.TotalVRAM = bytes
			dev.FreeVRAM = bytes
		} else {
			// No discrete VRAM (Apple Silicon unified memory): mark for memsize fill.
			needUnifiedMemory = true
		}
		// An entry with neither a name nor VRAM carries no signal; skip it.
		if dev.Name == "" && dev.TotalVRAM == 0 {
			continue
		}
		devices = append(devices, dev)
	}
	return devices, needUnifiedMemory, nil
}

// parseAppleVRAM parses a system_profiler VRAM string such as "8 GB", "1536 MB",
// or "8192 MB" into bytes. The unit is GB/MB (Apple uses binary multiples here,
// so GB == GiB and MB == MiB). It returns ok=false for an empty or unrecognized
// string (the Apple Silicon unified-memory case).
func parseAppleVRAM(s string) (uint64, bool) {
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) != 2 {
		return 0, false
	}
	value, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, false
	}
	switch strings.ToUpper(fields[1]) {
	case "GB", "GIB":
		return value * 1024 * 1024 * 1024, true
	case "MB", "MIB":
		return value * mib, true
	case "KB", "KIB":
		return value * 1024, true
	default:
		return 0, false
	}
}

// parseMemsize parses the decimal byte count emitted by `sysctl -n hw.memsize`.
func parseMemsize(out []byte) (uint64, error) {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return 0, fmt.Errorf("gpu: empty hw.memsize output")
	}
	return strconv.ParseUint(s, 10, 64)
}
