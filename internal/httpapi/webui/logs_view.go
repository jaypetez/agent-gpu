package webui

import (
	"encoding/json"
	"strings"
)

// logs_view.go holds the hand-written view helpers behind the Logs viewer's live
// tail (logs.templ, #103). The tail is a small Alpine island whose logic is supplied
// as an x-data expression string built here, so the template stays declarative and
// the (slightly involved) EventSource handling lives in reviewable Go-authored
// JavaScript. It produces only a JS string — never a CSS class string — so it needs
// no Tailwind content scanning. Alpine evaluates the expression at runtime, which the
// console CSP permits (script-src 'self' 'unsafe-eval'); the EventSource connects to
// the same origin (connect-src 'self'), with the session cookie riding automatically.

// logTailState returns the Alpine x-data expression for the live-tail panel. The
// component:
//
//   - holds the rendered lines, the streaming flag, the EventSource handle, and the
//     base stream URL (the SSE proxy path with the active level/attribute filters);
//   - resume() opens an EventSource to that URL and, on each message, parses the JSON
//     log frame, projects its attrs into discrete {text,primary} field badges,
//     prepends the line (newest first), and caps the list at maxTailLines so the DOM
//     stays bounded;
//   - pause() closes the EventSource (a reconnect on the next resume simply tails new
//     lines — the server cursor is the ring's monotonic position, so nothing is lost);
//   - clear() empties the rendered list.
//
// The level→tone class mapping mirrors levelTone server-side (ERROR→danger,
// WARN→warn, else info) using the SAME token-derived text-* classes the rest of the
// console uses, so the live rows and the buffered rows read identically. maxTailLines
// is inlined so the cap is visible at the call site.
func logTailState(streamURL string) string {
	const maxTailLines = 500
	// json.Marshal the URL so it is safely quoted/escaped inside the JS expression.
	urlLit, err := json.Marshal(streamURL)
	if err != nil {
		urlLit = []byte(`""`)
	}
	// The expression is a single JS object literal. It is kept compact (no template
	// literals) so it embeds cleanly in the HTML attribute. Field badges render as
	// key=value; the primary filter fields get the accent class via the template's
	// x-bind, so here we only flag primary-ness.
	var b strings.Builder
	b.WriteString("{ streaming: false, lines: [], es: null, url: ")
	b.Write(urlLit)
	b.WriteString(", ")
	b.WriteString("primary: { request_id: true, session_id: true, worker: true }, ")
	b.WriteString("buildURL() { /* url is already the filtered stream path */ }, ")
	b.WriteString("toneFor(level) { var l = (level || '').toUpperCase(); if (l.indexOf('ERROR') === 0) return 'text-danger'; if (l.indexOf('WARN') === 0) return 'text-warn'; return 'text-info'; }, ")
	b.WriteString("render(rec) { var fields = []; var attrs = rec.attrs || {}; var keys = Object.keys(attrs).sort(); ")
	b.WriteString("keys.sort(function(a, b){ var pa = this.primary[a] ? 0 : 1; var pb = this.primary[b] ? 0 : 1; if (pa !== pb) return pa - pb; return a < b ? -1 : 1; }.bind(this)); ")
	b.WriteString("for (var i = 0; i < keys.length; i++) { var k = keys[i]; fields.push({ text: k + '=' + attrs[k], primary: !!this.primary[k] }); } ")
	b.WriteString("var t = rec.time ? new Date(rec.time) : null; var ts = t ? t.toISOString().substr(11, 8) : ''; ")
	b.WriteString("return { time: ts, level: rec.level || '', toneClass: this.toneFor(rec.level), message: rec.message || '', fields: fields }; }, ")
	b.WriteString("resume() { if (this.es) return; this.streaming = true; var self = this; this.es = new EventSource(this.url); ")
	b.WriteString("this.es.onmessage = function(ev) { try { var rec = JSON.parse(ev.data); self.lines.unshift(self.render(rec)); if (self.lines.length > ")
	b.WriteString(itoa(maxTailLines))
	b.WriteString(") { self.lines.length = ")
	b.WriteString(itoa(maxTailLines))
	b.WriteString("; } } catch (e) { /* ignore a malformed frame */ } }; ")
	b.WriteString("this.es.onerror = function() { /* the browser auto-reconnects; keep the streaming flag */ }; }, ")
	b.WriteString("pause() { this.streaming = false; if (this.es) { this.es.close(); this.es = null; } }, ")
	b.WriteString("clear() { this.lines = []; } }")
	return b.String()
}
