package loadtest

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// sampleSummary builds a Summary with known values for the report tests.
func sampleSummary() Summary {
	return Summarize([]Result{
		{Latency: 10 * time.Millisecond, Status: 200, Tokens: 4},
		{Latency: 20 * time.Millisecond, Status: 200, Tokens: 4},
		{Latency: 1 * time.Millisecond, Status: 429},
		{Latency: 2 * time.Millisecond, Status: 503},
	}, 2*time.Second)
}

// TestRunReportWriteJSONRoundTrips proves the JSON report carries the config and
// summary and decodes back to the same numbers (the baseline-comparison artifact).
func TestRunReportWriteJSONRoundTrips(t *testing.T) {
	cfg := ReportConfig{
		Mode: "inproc", BaseURL: "http://x", Concurrency: 8, Loop: "closed",
		Mix: "chat=1", Model: "m", Workers: 2,
	}
	sat := &SaturationObs{PeakQueueDepth: 5, WaitCount: 3, WaitMaxMs: 40, WaitMeanMs: 20, Samples: 7}
	report := NewRunReport(cfg, sampleSummary(), sat)

	var buf bytes.Buffer
	if err := report.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	var got RunReport
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode JSON report: %v", err)
	}
	if got.Config.Mode != "inproc" || got.Config.Workers != 2 {
		t.Errorf("config round-trip wrong: %+v", got.Config)
	}
	if got.Summary.Total != 4 || got.Summary.Success != 2 {
		t.Errorf("summary round-trip wrong: total=%d success=%d", got.Summary.Total, got.Summary.Success)
	}
	if got.Summary.Throttled != 1 || got.Summary.Unavailable != 1 {
		t.Errorf("status buckets wrong: 429=%d 503=%d", got.Summary.Throttled, got.Summary.Unavailable)
	}
	// Latency is in milliseconds in the report.
	if got.Summary.SuccessLatency.Min != 10 || got.Summary.SuccessLatency.Max != 20 {
		t.Errorf("success latency ms wrong: min=%v max=%v", got.Summary.SuccessLatency.Min, got.Summary.SuccessLatency.Max)
	}
	if got.Saturation == nil || got.Saturation.PeakQueueDepth != 5 {
		t.Errorf("saturation round-trip wrong: %+v", got.Saturation)
	}
}

// TestRunReportWriteTextSections proves the text report contains the headline
// blocks and the status breakdown — the human-readable observability output.
func TestRunReportWriteTextSections(t *testing.T) {
	cfg := ReportConfig{
		Mode: "remote", BaseURL: "http://host:8080", Concurrency: 16, Loop: "open",
		Rate: 100, Mix: "chat=80,models=20", Model: "llama3", DurationCfg: "30s",
	}
	report := NewRunReport(cfg, sampleSummary(), &SaturationObs{PeakQueueDepth: 9, Samples: 3})

	var buf bytes.Buffer
	report.WriteText(&buf)
	text := buf.String()

	for _, want := range []string{
		"agent-gpu load test",
		"mode:         remote (open-loop)",
		"target rate:  100.0 req/s",
		"mix:          chat=80,models=20",
		"throughput",
		"status breakdown",
		"2xx (ok):          2",
		"429 (throttled):   1",
		"503 (unavailable): 1",
		"latency — all requests",
		"latency — successful (2xx) requests",
		"saturation (admin stats)",
		"peak queue depth:  9",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text report missing %q\n--- output ---\n%s", want, text)
		}
	}
}

// TestRunReportTextNoSaturation proves the saturation block is omitted when no
// poll was taken (nil observation).
func TestRunReportTextNoSaturation(t *testing.T) {
	cfg := ReportConfig{Mode: "remote", BaseURL: "http://x", Concurrency: 1, Loop: "closed", Mix: "chat=1"}
	report := NewRunReport(cfg, sampleSummary(), nil)
	var buf bytes.Buffer
	report.WriteText(&buf)
	if strings.Contains(buf.String(), "saturation (admin stats)") {
		t.Errorf("saturation block present without an observation")
	}
}

// TestRunReportTextEmptyLatency proves a no-sample latency block renders "(no
// samples)" rather than a bogus zero table.
func TestRunReportTextEmptyLatency(t *testing.T) {
	cfg := ReportConfig{Mode: "remote", BaseURL: "http://x", Concurrency: 1, Loop: "closed", Mix: "chat=1"}
	report := NewRunReport(cfg, Summarize(nil, time.Second), nil)
	var buf bytes.Buffer
	report.WriteText(&buf)
	if !strings.Contains(buf.String(), "(no samples)") {
		t.Errorf("empty latency block did not render '(no samples)':\n%s", buf.String())
	}
}

// TestValidEndpoint covers the exported endpoint validator used by the CLI flag.
func TestValidEndpoint(t *testing.T) {
	for _, ep := range []Endpoint{EndpointChat, EndpointCompletions, EndpointModels} {
		if !ValidEndpoint(ep) {
			t.Errorf("ValidEndpoint(%q) = false, want true", ep)
		}
	}
	if ValidEndpoint(Endpoint("nope")) {
		t.Errorf("ValidEndpoint(nope) = true, want false")
	}
}

// TestMixEntries covers the report-facing Entries accessor and that it returns a
// defensive copy.
func TestMixEntries(t *testing.T) {
	m, _ := ParseMix("chat=80,models=20")
	entries := m.Entries()
	if len(entries) != 2 {
		t.Fatalf("Entries() len = %d, want 2", len(entries))
	}
	// Mutating the returned slice must not affect the Mix.
	entries[0].Weight = 999
	if m.Entries()[0].Weight == 999 {
		t.Errorf("Entries() did not return a defensive copy")
	}
}
