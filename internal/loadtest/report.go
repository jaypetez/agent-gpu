package loadtest

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
	"unicode/utf8"
)

// RunReport is the full, serializable record of one load run: the configuration
// that produced it, the aggregated summary, and an optional saturation snapshot
// polled from the admin stats endpoint. It is what --json emits, so it is the
// stable artifact a future run is compared against. Field names are snake_case
// for a conventional JSON shape.
type RunReport struct {
	Config     ReportConfig   `json:"config"`
	Summary    ReportSummary  `json:"summary"`
	Saturation *SaturationObs `json:"saturation,omitempty"`
}

// ReportConfig is the human/JSON projection of the run's configuration: enough
// to reproduce and compare it.
type ReportConfig struct {
	Mode        string  `json:"mode"`        // "remote" or "inproc"
	BaseURL     string  `json:"base_url"`    // omitted detail-free; the target
	Concurrency int     `json:"concurrency"` //
	Loop        string  `json:"loop"`        // "closed" or "open"
	Rate        float64 `json:"rate_rps"`    // open-loop arrival rate (0 = closed)
	Mix         string  `json:"mix"`         // e.g. "chat=80,models=20"
	Model       string  `json:"model,omitempty"`
	DurationCfg string  `json:"duration,omitempty"` // configured duration, if any
	RequestsCfg int     `json:"requests,omitempty"` // configured request budget, if any
	// InProc fields are populated only for the in-process mode so a baseline
	// records the stack it ran against.
	Workers       int    `json:"workers,omitempty"`
	QueueMaxDepth int    `json:"queue_max_depth,omitempty"`
	GlobalRPM     uint64 `json:"global_rpm,omitempty"`
	GlobalTPM     uint64 `json:"global_tpm,omitempty"`
}

// ReportSummary is the JSON projection of a Summary with durations expressed in
// milliseconds (floats) so the artifact is language-agnostic and easy to diff.
type ReportSummary struct {
	Total       int     `json:"total"`
	Success     int     `json:"success"`
	Throttled   int     `json:"throttled_429"`
	Unavailable int     `json:"unavailable_503"`
	Errors      int     `json:"errors_other"`
	ElapsedSec  float64 `json:"elapsed_sec"`

	Throughput        float64 `json:"throughput_rps"`
	SuccessThroughput float64 `json:"success_throughput_rps"`
	TokensPerSec      float64 `json:"tokens_per_sec"`
	TotalTokens       uint64  `json:"total_tokens"`
	ErrorRate         float64 `json:"error_rate"`

	Latency        ReportPercentiles `json:"latency_ms"`
	SuccessLatency ReportPercentiles `json:"success_latency_ms"`
}

// ReportPercentiles mirrors Percentiles in milliseconds (floats).
type ReportPercentiles struct {
	Count int     `json:"count"`
	Min   float64 `json:"min"`
	P50   float64 `json:"p50"`
	P90   float64 `json:"p90"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	P999  float64 `json:"p99_9"`
	Max   float64 `json:"max"`
	Mean  float64 `json:"mean"`
}

// SaturationObs is the saturation snapshot polled from GET /v1/admin/stats over
// the run (or read directly in-process): the peak queue depth seen and the
// time-in-queue summary. It is how queueing is made observable beyond the
// client-side 503 count. It is nil when no admin polling was configured.
type SaturationObs struct {
	PeakQueueDepth int    `json:"peak_queue_depth"`
	WaitCount      uint64 `json:"wait_count"`
	WaitMaxMs      uint64 `json:"wait_max_ms"`
	WaitMeanMs     uint64 `json:"wait_mean_ms"`
	Samples        int    `json:"samples"` // number of stats polls taken
}

// msFloat converts a duration to milliseconds as a float for the JSON report.
func msFloat(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// reportPercentiles projects a Percentiles into its millisecond JSON form.
func reportPercentiles(p Percentiles) ReportPercentiles {
	return ReportPercentiles{
		Count: p.Count,
		Min:   msFloat(p.Min),
		P50:   msFloat(p.P50),
		P90:   msFloat(p.P90),
		P95:   msFloat(p.P95),
		P99:   msFloat(p.P99),
		P999:  msFloat(p.P999),
		Max:   msFloat(p.Max),
		Mean:  msFloat(p.Mean),
	}
}

// NewRunReport assembles a RunReport from the run's config projection, the
// aggregated summary, and an optional saturation observation.
func NewRunReport(cfg ReportConfig, s Summary, sat *SaturationObs) RunReport {
	return RunReport{
		Config: cfg,
		Summary: ReportSummary{
			Total:             s.Total,
			Success:           s.Success,
			Throttled:         s.Throttled,
			Unavailable:       s.Unavailable,
			Errors:            s.Errors,
			ElapsedSec:        s.Elapsed.Seconds(),
			Throughput:        s.Throughput,
			SuccessThroughput: s.SuccessThroughput,
			TokensPerSec:      s.TokensPerSec,
			TotalTokens:       s.TotalTokens,
			ErrorRate:         s.ErrorRate,
			Latency:           reportPercentiles(s.Latency),
			SuccessLatency:    reportPercentiles(s.SuccessLatency),
		},
		Saturation: sat,
	}
}

// WriteJSON writes the report as indented JSON to w (the --json output). It is
// the machine-readable artifact for baseline comparison.
func (r RunReport) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteText renders a human-readable report to w: the run configuration, the
// throughput and latency-percentile tables, the error rate, the status-code
// breakdown (how throttling/queueing is observed client-side), and, when polled,
// the saturation snapshot. The format is stable and grep-friendly.
func (r RunReport) WriteText(w io.Writer) {
	c, s := r.Config, r.Summary
	fmt.Fprintln(w, "agent-gpu load test")
	fmt.Fprintln(w, "===================")
	fmt.Fprintf(w, "mode:         %s (%s-loop)\n", c.Mode, c.Loop)
	fmt.Fprintf(w, "target:       %s\n", c.BaseURL)
	fmt.Fprintf(w, "concurrency:  %d\n", c.Concurrency)
	if c.Loop == "open" {
		fmt.Fprintf(w, "target rate:  %.1f req/s\n", c.Rate)
	}
	fmt.Fprintf(w, "mix:          %s\n", c.Mix)
	if c.Model != "" {
		fmt.Fprintf(w, "model:        %s\n", c.Model)
	}
	if c.DurationCfg != "" {
		fmt.Fprintf(w, "duration:     %s\n", c.DurationCfg)
	}
	if c.RequestsCfg > 0 {
		fmt.Fprintf(w, "requests:     %d\n", c.RequestsCfg)
	}
	if c.Workers > 0 {
		fmt.Fprintf(w, "workers:      %d\n", c.Workers)
	}
	if c.QueueMaxDepth > 0 {
		fmt.Fprintf(w, "queue depth:  %d (max)\n", c.QueueMaxDepth)
	}
	if c.GlobalRPM > 0 {
		fmt.Fprintf(w, "global rpm:   %d\n", c.GlobalRPM)
	}
	if c.GlobalTPM > 0 {
		fmt.Fprintf(w, "global tpm:   %d\n", c.GlobalTPM)
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "throughput")
	fmt.Fprintln(w, "----------")
	fmt.Fprintf(w, "elapsed:            %.3fs\n", s.ElapsedSec)
	fmt.Fprintf(w, "requests:           %d\n", s.Total)
	fmt.Fprintf(w, "throughput:         %.1f req/s (all)\n", s.Throughput)
	fmt.Fprintf(w, "success throughput: %.1f req/s (2xx)\n", s.SuccessThroughput)
	if s.TotalTokens > 0 {
		fmt.Fprintf(w, "tokens/sec:         %.1f\n", s.TokensPerSec)
	}
	fmt.Fprintf(w, "error rate:         %.2f%%\n", s.ErrorRate*100)

	fmt.Fprintln(w)
	fmt.Fprintln(w, "status breakdown")
	fmt.Fprintln(w, "----------------")
	fmt.Fprintf(w, "2xx (ok):          %d\n", s.Success)
	fmt.Fprintf(w, "429 (throttled):   %d\n", s.Throttled)
	fmt.Fprintf(w, "503 (unavailable): %d\n", s.Unavailable)
	fmt.Fprintf(w, "other/errors:      %d\n", s.Errors)

	writeLatencyBlock(w, "latency — all requests", s.Latency)
	writeLatencyBlock(w, "latency — successful (2xx) requests", s.SuccessLatency)

	if r.Saturation != nil {
		sat := r.Saturation
		fmt.Fprintln(w)
		fmt.Fprintln(w, "saturation (admin stats)")
		fmt.Fprintln(w, "------------------------")
		fmt.Fprintf(w, "peak queue depth:  %d\n", sat.PeakQueueDepth)
		fmt.Fprintf(w, "queued jobs:       %d\n", sat.WaitCount)
		fmt.Fprintf(w, "queue wait max:    %d ms\n", sat.WaitMaxMs)
		fmt.Fprintf(w, "queue wait mean:   %d ms\n", sat.WaitMeanMs)
		fmt.Fprintf(w, "stats polls:       %d\n", sat.Samples)
	}
}

// writeLatencyBlock renders one labelled percentile table.
func writeLatencyBlock(w io.Writer, label string, p ReportPercentiles) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, label)
	// Underline by RUNE count, not byte count, so a label with a multibyte
	// character (the em dash) gets an underline of the right visible width.
	fmt.Fprintln(w, dashes(utf8.RuneCountInString(label)))
	if p.Count == 0 {
		fmt.Fprintln(w, "(no samples)")
		return
	}
	fmt.Fprintf(w, "samples: %d\n", p.Count)
	fmt.Fprintf(w, "min:  %8.2f ms\n", p.Min)
	fmt.Fprintf(w, "p50:  %8.2f ms\n", p.P50)
	fmt.Fprintf(w, "p90:  %8.2f ms\n", p.P90)
	fmt.Fprintf(w, "p95:  %8.2f ms\n", p.P95)
	fmt.Fprintf(w, "p99:  %8.2f ms\n", p.P99)
	fmt.Fprintf(w, "p99.9:%8.2f ms\n", p.P999)
	fmt.Fprintf(w, "max:  %8.2f ms\n", p.Max)
	fmt.Fprintf(w, "mean: %8.2f ms\n", p.Mean)
}

// dashes returns a string of n dash characters, for underlining a heading.
func dashes(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '-'
	}
	return string(b)
}
