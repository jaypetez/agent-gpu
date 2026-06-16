package gpu

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// AMD GPUs are detected via ROCm's command-line tooling. The newer amd-smi is
// preferred when present; the older rocm-smi is the fallback. Both can emit JSON
// with VRAM already in bytes, which we parse defensively because the exact key
// names and shapes vary across ROCm versions.
const (
	amdSMI  = "amd-smi"
	rocmSMI = "rocm-smi"
)

// detectAMD probes for AMD GPUs, preferring amd-smi and falling back to
// rocm-smi. Either tool being absent surfaces as a tool-not-found error; if the
// preferred tool is missing we transparently try the fallback. A present tool
// that reports no usable device yields an empty slice (not an error) so the
// Detector moves on cleanly.
func (d *Detector) detectAMD(ctx context.Context) ([]Device, error) {
	// Prefer amd-smi (the newer unified tool).
	out, err := d.runner(ctx, amdSMI, "static", "--json")
	if err == nil {
		if devices, perr := parseAmdSMI(out); perr == nil && len(devices) > 0 {
			return devices, nil
		}
		// amd-smi present but its output was unusable; fall through to rocm-smi.
	} else if !errToolNotFound(err) {
		// amd-smi present but errored at runtime; still try rocm-smi rather than
		// giving up on AMD entirely.
		d.logger.Debug("amd-smi failed; trying rocm-smi", "err", err)
	}

	// Fall back to rocm-smi. VRAM fields from --showmeminfo are in bytes.
	rout, rerr := d.runner(ctx, rocmSMI,
		"--showproductname", "--showmeminfo", "vram", "--showuse", "--json")
	if rerr != nil {
		// If neither tool produced anything and the only error we have is the
		// rocm-smi error, surface it (tool-not-found is treated as vendor-absent
		// upstream).
		return nil, rerr
	}
	return parseRocmSMI(rout)
}

// parseRocmSMI parses the JSON emitted by
//
//	rocm-smi --showproductname --showmeminfo vram --showuse --json
//
// The output is an object keyed by card id ("card0", "card1", …); each value is
// an object whose keys carry units in their names, e.g.:
//
//	{
//	  "card0": {
//	    "Card series": "Instinct MI210",
//	    "VRAM Total Memory (B)": "68702699520",
//	    "VRAM Total Used Memory (B)": "10747904",
//	    "GPU use (%)": "0"
//	  }
//	}
//
// VRAM values are already in bytes. Because key spellings vary across ROCm
// versions, fields are matched by case-insensitive substring rather than exact
// equality, and unknown keys are ignored. Free VRAM is derived as total − used.
// Cards are returned sorted by id for deterministic aggregation. Output that is
// not a JSON object of card entries is an error.
func parseRocmSMI(out []byte) ([]Device, error) {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, fmt.Errorf("gpu: empty rocm-smi output")
	}
	var raw map[string]map[string]string
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return nil, fmt.Errorf("gpu: parse rocm-smi json: %w", err)
	}

	// Iterate cards in id order so multi-GPU aggregation is stable.
	ids := make([]string, 0, len(raw))
	for id := range raw {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var devices []Device
	for _, id := range ids {
		// Only consider card* entries; rocm-smi sometimes includes a top-level
		// "system" object we must skip.
		if !strings.HasPrefix(strings.ToLower(id), "card") {
			continue
		}
		entry := raw[id]
		dev := Device{Name: rocmName(entry)}

		total, haveTotal := rocmUintBySubstr(entry, "vram total memory")
		used, haveUsed := rocmUintBySubstr(entry, "vram total used memory")
		if haveTotal {
			dev.TotalVRAM = total
			if haveUsed && used <= total {
				dev.FreeVRAM = total - used
			}
		}
		if util, ok := rocmUintBySubstr(entry, "gpu use"); ok {
			if util > 100 {
				util = 100
			}
			dev.Load = uint32(util)
		}
		// Skip entries that exposed neither a name nor any VRAM — they carry no
		// usable capacity signal.
		if dev.Name == "" && !haveTotal {
			continue
		}
		devices = append(devices, dev)
	}
	return devices, nil
}

// rocmName extracts a product name from a rocm-smi card entry, preferring the
// marketing series, then model, then any "card ..." name field. Returns "" when
// none is present (the caller substitutes a generic name during aggregation).
func rocmName(entry map[string]string) string {
	for _, want := range []string{"card series", "card model", "card vendor", "gpu name", "product name"} {
		if v, ok := rocmStrBySubstr(entry, want); ok {
			return v
		}
	}
	return ""
}

// rocmStrBySubstr returns the first value whose key contains substr
// (case-insensitive) and is non-empty.
func rocmStrBySubstr(entry map[string]string, substr string) (string, bool) {
	substr = strings.ToLower(substr)
	for k, v := range entry {
		if strings.Contains(strings.ToLower(k), substr) {
			if v = strings.TrimSpace(v); v != "" {
				return v, true
			}
		}
	}
	return "", false
}

// rocmUintBySubstr returns the unsigned integer value whose key contains substr
// (case-insensitive). Values may carry stray formatting; only a clean unsigned
// integer is accepted.
func rocmUintBySubstr(entry map[string]string, substr string) (uint64, bool) {
	v, ok := rocmStrBySubstr(entry, substr)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// amdGPU mirrors the subset of `amd-smi static --json` we consume. amd-smi emits
// a JSON array, one object per GPU. The schema varies across versions, so the
// fields here are best-effort and parsing tolerates missing ones.
type amdGPU struct {
	// asic carries the marketing name on newer amd-smi.
	ASIC struct {
		MarketName string `json:"market_name"`
	} `json:"asic"`
	// vram holds total memory; the size may be a scalar (MB) or a nested object
	// depending on version, so it is decoded leniently via amdVRAM.
	VRAM amdVRAM `json:"vram"`
}

// amdVRAM leniently decodes amd-smi's vram block, which across versions is
// either {"size": {"value": N, "unit": "MB"}} or {"total": {"value": N,
// "unit": "MB"}} or a bare {"size": N}. It records the value and (lowercased)
// unit so parseAmdSMI can normalize to bytes.
type amdVRAM struct {
	value uint64
	unit  string
}

// UnmarshalJSON decodes the lenient vram block.
func (v *amdVRAM) UnmarshalJSON(data []byte) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		// Not an object (e.g. null) — leave zero.
		return nil
	}
	for _, key := range []string{"total", "size", "total_vram"} {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		if val, unit, ok := amdDecodeQuantity(raw); ok {
			v.value, v.unit = val, unit
			return nil
		}
	}
	return nil
}

// amdDecodeQuantity decodes either a bare number or a {"value":N,"unit":"MB"}
// object into a value and unit. A bare number has an empty unit.
func amdDecodeQuantity(raw json.RawMessage) (uint64, string, bool) {
	var n uint64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, "", true
	}
	var q struct {
		Value uint64 `json:"value"`
		Unit  string `json:"unit"`
	}
	if err := json.Unmarshal(raw, &q); err == nil && q.Value > 0 {
		return q.Value, strings.ToLower(strings.TrimSpace(q.Unit)), true
	}
	return 0, "", false
}

// parseAmdSMI parses `amd-smi static --json` (a JSON array, one object per GPU).
// VRAM is normalized to bytes from the reported unit (MB/MiB default to MiB when
// the unit is absent, which matches amd-smi's historical MB-as-MiB reporting).
// amd-smi static does not include free VRAM or utilization, so those stay zero
// here; the rocm-smi path supplies them when available. Non-array or empty
// output yields no devices (the caller then falls back to rocm-smi).
func parseAmdSMI(out []byte) ([]Device, error) {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, fmt.Errorf("gpu: empty amd-smi output")
	}
	var gpus []amdGPU
	if err := json.Unmarshal([]byte(trimmed), &gpus); err != nil {
		return nil, fmt.Errorf("gpu: parse amd-smi json: %w", err)
	}
	var devices []Device
	for _, g := range gpus {
		dev := Device{
			Name:      strings.TrimSpace(g.ASIC.MarketName),
			TotalVRAM: amdVRAMToBytes(g.VRAM),
		}
		if dev.Name == "" && dev.TotalVRAM == 0 {
			continue
		}
		devices = append(devices, dev)
	}
	return devices, nil
}

// amdVRAMToBytes normalizes an amd-smi vram quantity to bytes. Recognized units:
// gb/gib, mb/mib, kb/kib, b. An absent unit is treated as MiB (amd-smi has
// historically reported VRAM in MB that are really MiB).
func amdVRAMToBytes(v amdVRAM) uint64 {
	if v.value == 0 {
		return 0
	}
	switch v.unit {
	case "gb", "gib":
		return v.value * 1024 * 1024 * 1024
	case "kb", "kib":
		return v.value * 1024
	case "b", "bytes":
		return v.value
	case "mb", "mib", "":
		return v.value * mib
	default:
		// Unknown unit: assume MiB rather than dropping the signal.
		return v.value * mib
	}
}
