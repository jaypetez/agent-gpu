package webui

import (
	"strconv"
	"strings"
)

// Tone names a status severity used across the console's status language. Status
// is ALWAYS conveyed by both a color (these tones) and a text label/word, so it
// survives color-blindness and grayscale (issue #100 AC3). These constants are the
// vocabulary the handlers map raw worker/log/telemetry states onto.
const (
	ToneOK     = "ok"
	ToneWarn   = "warn"
	ToneDanger = "danger"
	ToneInfo   = "info"
	ToneIdle   = "idle"
)

// toneText maps a tone to the text-color utility class for a value/label. The
// classes are token-derived (text-ok / text-warn / …), defined from the design
// tokens in app.css, so no raw color ever appears in a template. An unknown tone
// falls back to the muted foreground.
func toneText(tone string) string {
	switch tone {
	case ToneOK:
		return "text-ok"
	case ToneWarn:
		return "text-warn"
	case ToneDanger:
		return "text-danger"
	case ToneInfo:
		return "text-info"
	case ToneIdle:
		return "text-idle"
	default:
		return "text-fg-muted"
	}
}

// toneBadge maps a tone to the status-badge variant class (.badge-ok / …). The
// badge always renders alongside its text label in the template.
func toneBadge(tone string) string {
	switch tone {
	case ToneOK:
		return "badge-ok"
	case ToneWarn:
		return "badge-warn"
	case ToneDanger:
		return "badge-danger"
	default:
		return "badge-idle"
	}
}

// toneBar maps a tone to the fill color of a progress/queue bar.
func toneBar(tone string) string {
	switch tone {
	case ToneOK:
		return "bg-ok"
	case ToneWarn:
		return "bg-warn"
	case ToneDanger:
		return "bg-danger"
	case ToneInfo:
		return "bg-accent"
	default:
		return "bg-idle"
	}
}

// LoadTone maps a coarse worker load (0-100) to a status tone using the heatmap
// thresholds of issue #101 AC2: green (ok) below 60, yellow (warn) 60-85, red
// (danger) above 85. It is exported because the httpapi layer projects fleet
// snapshots into heatmap cells and needs the SAME thresholds the cells render, so
// the band color and the band word never diverge.
func LoadTone(load uint32) string {
	switch {
	case load > 85:
		return ToneDanger
	case load >= 60:
		return ToneWarn
	default:
		return ToneOK
	}
}

// LoadBandWord is the text label that rides ALONGSIDE the heatmap cell's color so
// the band reads in grayscale and to a screen reader (AC2: text labels, not color
// alone). It uses operator vocabulary for utilization: "ok" / "busy" / "hot".
func LoadBandWord(load uint32) string {
	switch {
	case load > 85:
		return "hot"
	case load >= 60:
		return "busy"
	default:
		return "ok"
	}
}

// toastTone maps a tone to the toast variant class (.toast-ok / …), which colors
// the toast's left rule. The toast always carries a text title + message, so the
// tone is reinforcement, never the sole signal (AC3/AC4). An unknown tone falls
// back to the informational variant.
func toastTone(tone string) string {
	switch tone {
	case ToneOK:
		return "toast-ok"
	case ToneWarn:
		return "toast-warn"
	case ToneDanger:
		return "toast-danger"
	default:
		return "toast-info"
	}
}

// heatCell maps a tone to the heatmap cell's fill + text classes. The fill is a
// soft, low-chroma tint of the tone (so a wall of cells is calm, not garish) and
// the foreground is the full-strength tone for the load number — both
// token-derived, so no raw color appears in a template.
func heatCell(tone string) string {
	switch tone {
	case ToneOK:
		return "bg-ok-soft text-ok"
	case ToneWarn:
		return "bg-warn-soft text-warn"
	case ToneDanger:
		return "bg-danger-soft text-danger"
	default:
		return "bg-surface-2 text-fg-muted"
	}
}

// toneWord is the short status word shown beside a KPI value, so the KPI's health
// is stated in words and not only in the value's color (AC3). It deliberately uses
// operator vocabulary: "ok" / "watch" / "alert".
func toneWord(tone string) string {
	switch tone {
	case ToneOK:
		return "ok"
	case ToneWarn:
		return "watch"
	case ToneDanger:
		return "alert"
	case ToneInfo:
		return "live"
	default:
		return "idle"
	}
}

// barWidth returns an inline `width: N%` style for a queue/progress bar. The
// percentage is live data (a fraction of the total), not a design value, so it is
// computed here and emitted as an inline style — it is deliberately NOT a Tailwind
// arbitrary value (which the token-lint test forbids in templates). n is clamped
// to [0,total]; a zero total yields 0% (no divide-by-zero).
func barWidth(n, total int) string {
	if total <= 0 || n <= 0 {
		return "width:0%"
	}
	if n >= total {
		return "width:100%"
	}
	pct := (n * 100) / total
	if pct < 2 {
		// Keep a sliver visible for a tiny-but-nonzero count.
		pct = 2
	}
	return "width:" + strconv.Itoa(pct) + "%"
}

// itoa / itoaU32 format integers for display. Numbers render in the mono face with
// tabular figures (the template applies tnum), so columns of them stay aligned.
func itoa(n int) string { return strconv.Itoa(n) }

func itoaU32(n uint32) string { return strconv.FormatUint(uint64(n), 10) }

func itoaU64(n uint64) string { return strconv.FormatUint(n, 10) }

// logFieldTone maps a log field's first-class-ness to its badge text color class:
// the primary filter fields (request_id/session_id/worker) are tinted with the
// accent so they stand out from incidental attrs (muted). Both are token-derived
// classes, so no raw color appears in a template.
func logFieldTone(primary bool) string {
	if primary {
		return "text-accent"
	}
	return "text-fg-muted"
}

// UsageTone maps a consumption percentage (0-100) to a status tone using the usage
// thresholds of #103: ok below 75%, watch from 75-90%, alert above 90%. It is
// exported because the httpapi layer projects quota snapshots into usage meters and
// needs the SAME thresholds the bars render, so a meter's color and the row's
// warning never diverge.
func UsageTone(pct int) string {
	switch {
	case pct >= 90:
		return ToneDanger
	case pct >= 75:
		return ToneWarn
	default:
		return ToneOK
	}
}

// Sparkpoints renders a polyline `points` attribute for a sparkline over a fixed
// 100x28 viewBox from a series of values (oldest→newest). The x axis is evenly
// spaced across the width; the y axis is inverted (SVG y grows downward) and scaled
// so the series max touches the top padding and a flat/zero series sits on the
// baseline. It is the data-driven geometry of the usage sparkline; the value is an
// SVG attribute string (NOT a Tailwind arbitrary value), so it never trips the
// token-lint guard. Fewer than two points yields an empty string (the caller draws
// no line). The returned string has space-separated "x,y" pairs.
func Sparkpoints(values []uint64) string {
	if len(values) < 2 {
		return ""
	}
	const (
		w   = 100.0
		h   = 28.0
		pad = 2.0
	)
	var max uint64
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	stepX := w / float64(len(values)-1)
	usableH := h - 2*pad
	var b strings.Builder
	for i, v := range values {
		x := float64(i) * stepX
		var y float64
		if max == 0 {
			y = h - pad // flat baseline when every day is zero
		} else {
			y = pad + (1-float64(v)/float64(max))*usableH
		}
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(strconv.FormatFloat(x, 'f', 1, 64))
		b.WriteByte(',')
		b.WriteString(strconv.FormatFloat(y, 'f', 1, 64))
	}
	return b.String()
}
