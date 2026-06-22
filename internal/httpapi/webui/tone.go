package webui

import (
	"strconv"
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
