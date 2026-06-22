package httpapi

import (
	"sort"
	"strconv"
	"time"

	"github.com/jaypetez/agent-gpu/internal/httpapi/webui"
	"github.com/jaypetez/agent-gpu/internal/types"
)

// webui_workers_data.go maps the server's live, in-process fleet state onto the
// console's Workers + GPU view-models (#101). Like webui_data.go it reads the SAME
// in-process accessors the JSON admin endpoints use — s.fleet.Fleet() for the
// list, s.fleet.WorkerByID for the detail, and the shared aggregateGPUs reducer
// (also behind GET /v1/admin/gpus) for the heatmap — so the console's numbers
// match the API by construction. It performs no new probing and starts no
// pollers; every value is read on demand for one consistent instant.

// collectWorkerList projects the fleet snapshot into the live worker-list rows,
// sorted by id for a stable render across refreshes. Each row's status is carried
// as a text label beside a tone color (never color alone).
func (s *Server) collectWorkerList() []webui.WorkerListItem {
	fleet := s.fleet.Fleet()
	rows := make([]webui.WorkerListItem, 0, len(fleet))
	for _, w := range fleet {
		rows = append(rows, webui.WorkerListItem{
			ID:         w.ID,
			Status:     w.Status.String(),
			Tone:       workerTone(w.Status),
			ActiveJobs: w.ActiveJobs,
			Load:       w.Load,
			GPUType:    gpuTypeLabel(w.GPUType),
			VRAM:       formatVRAMUsage(w.FreeVRAM, w.TotalVRAM),
			LastSeen:   relativeSince(w.LastSeen),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows
}

// collectWorkerDetail resolves one worker's rich detail projection for the detail
// screen, returning ok=false when no such worker is connected (the handler maps
// that to a 404 page). It mirrors the fields of GET /v1/admin/workers/{id} and
// adds the derived presentation strings the screen renders.
func (s *Server) collectWorkerDetail(id string) (webui.WorkerDetail, bool) {
	w, ok := s.fleet.WorkerByID(id)
	if !ok {
		return webui.WorkerDetail{}, false
	}
	models := make([]string, len(w.Models))
	for i, m := range w.Models {
		models[i] = m.Name
	}
	sort.Strings(models)

	d := webui.WorkerDetail{
		ID:         w.ID,
		Status:     w.Status.String(),
		Tone:       workerTone(w.Status),
		Draining:   w.Status == types.WorkerDraining,
		ActiveJobs: w.ActiveJobs,
		Load:       w.Load,
		LoadTone:   webui.LoadTone(w.Load),
		GPUType:    gpuTypeLabel(w.GPUType),
		TotalVRAM:  formatBytes(w.TotalVRAM),
		FreeVRAM:   formatBytes(w.FreeVRAM),
		UsedPct:    usedVRAMPercent(w.FreeVRAM, w.TotalVRAM),
		LastSeen:   relativeSince(w.LastSeen),
		Models:     models,
		// The logs page itself lands in #103; the affordance link is provided now so
		// an operator reaches a worker's logs within ~3 clicks from the dashboard.
		LogsHref: "/admin/logs?worker=" + w.ID,
	}
	if !w.RegisteredAt.IsZero() {
		if up := w.LastSeen.Sub(w.RegisteredAt); up > 0 {
			d.Uptime = formatDuration(up)
		}
	}
	return d, true
}

// collectHeatmap reduces the fleet snapshot into the GPU utilization heatmap: the
// per-worker cells (each with a load band by the AC2 thresholds, conveyed by color
// AND a text word) plus the fleet roll-up shown above the grid. It folds the same
// aggregateGPUs reducer behind GET /v1/admin/gpus so the console and API agree.
func (s *Server) collectHeatmap() webui.HeatmapData {
	agg := aggregateGPUs(s.fleet.Fleet())
	cells := make([]webui.HeatCell, 0, len(agg.Workers))
	for _, c := range agg.Workers {
		cells = append(cells, webui.HeatCell{
			ID:       c.ID,
			Load:     c.Load,
			Band:     webui.LoadTone(c.Load),
			BandWord: webui.LoadBandWord(c.Load),
			Tone:     webui.LoadTone(c.Load),
			VRAM:     formatVRAMUsage(c.FreeVRAM, c.TotalVRAM),
			Href:     "/admin/workers/" + c.ID,
		})
	}
	return webui.HeatmapData{
		Cells:       cells,
		WorkerCount: agg.Fleet.WorkerCount,
		MeanLoad:    agg.Fleet.MeanLoad,
		MaxLoad:     agg.Fleet.MaxLoad,
		MeanTone:    webui.LoadTone(agg.Fleet.MeanLoad),
		TotalVRAM:   formatBytes(agg.Fleet.TotalVRAM),
		FreeVRAM:    formatBytes(agg.Fleet.FreeVRAM),
	}
}

// gpuTypeLabel renders the worker's reported GPU type for display, substituting a
// readable placeholder for an empty string so a cell/row never shows a blank.
func gpuTypeLabel(t string) string {
	if t == "" {
		return "unknown"
	}
	return t
}

// formatBytes renders a byte count in binary units (KiB/MiB/GiB/TiB) with one
// decimal for the larger units, so a VRAM figure stays one glanceable token. Zero
// renders as "0 B" rather than a fraction.
func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return strconv.FormatUint(b, 10) + " B"
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	val := float64(b) / float64(div)
	return strconv.FormatFloat(val, 'f', 1, 64) + " " + []string{"KiB", "MiB", "GiB", "TiB", "PiB"}[exp]
}

// formatVRAMUsage renders "free / total" VRAM for a worker, e.g. "18.2 / 24.0 GiB".
// A zero total (a CPU-only worker that reports no VRAM) renders a dash so the cell
// reads cleanly rather than "0 B / 0 B".
func formatVRAMUsage(free, total uint64) string {
	if total == 0 {
		return "—"
	}
	return formatBytes(free) + " free / " + formatBytes(total)
}

// usedVRAMPercent computes the integer percentage of VRAM in use (0-100) from the
// free/total figures, guarding a zero total. It backs the detail screen's VRAM
// usage bar.
func usedVRAMPercent(free, total uint64) int {
	if total == 0 || free >= total {
		if total == 0 {
			return 0
		}
		return 0
	}
	used := total - free
	return int((used * 100) / total)
}

// relativeSince renders a coarse "time ago" string for a last-seen timestamp, in
// the operator's glance vocabulary: "just now", "12s ago", "3m ago", "2h ago",
// "5d ago". A zero time renders "never". It is intentionally coarse — the detail
// screen is a status glance, not a precise clock.
func relativeSince(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s ago"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d ago"
	}
}

// formatDuration renders a worker uptime compactly as the two most significant
// units (e.g. "3d 4h", "5h 12m", "2m 30s", "45s"), so the detail screen shows how
// long a worker has been connected without a long precise string.
func formatDuration(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	days := int(d / (24 * time.Hour))
	hours := int((d % (24 * time.Hour)) / time.Hour)
	mins := int((d % time.Hour) / time.Minute)
	secs := int((d % time.Minute) / time.Second)
	switch {
	case days > 0:
		return strconv.Itoa(days) + "d " + strconv.Itoa(hours) + "h"
	case hours > 0:
		return strconv.Itoa(hours) + "h " + strconv.Itoa(mins) + "m"
	case mins > 0:
		return strconv.Itoa(mins) + "m " + strconv.Itoa(secs) + "s"
	default:
		return strconv.Itoa(secs) + "s"
	}
}
