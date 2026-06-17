package loadtest

import (
	"errors"
	"testing"
	"time"
)

// ms is a small helper so the percentile tables below read in milliseconds.
func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// TestPercentileNearestRank pins the nearest-rank index math against a known,
// already-sorted sample so the quantile selection is exact and reproducible. For
// n=10 (indices 0..9): round(q*9) gives p50->idx5, p90->idx8, p95->idx9 (round
// 8.55), p99->idx9, p99.9->idx9.
func TestPercentileNearestRank(t *testing.T) {
	sorted := []time.Duration{ms(1), ms(2), ms(3), ms(4), ms(5), ms(6), ms(7), ms(8), ms(9), ms(10)}
	cases := []struct {
		q    float64
		want time.Duration
	}{
		{0.0, ms(1)},   // first
		{0.50, ms(6)},  // round(0.50*9)=round(4.5)=4 -> wait: math.Round(4.5)=5 (round half away from zero) -> idx5 = ms(6)
		{0.90, ms(9)},  // round(8.1)=8 -> ms(9)
		{0.95, ms(10)}, // round(8.55)=9 -> ms(10)
		{0.99, ms(10)}, // round(8.91)=9 -> ms(10)
		{0.999, ms(10)},
		{1.0, ms(10)}, // last
	}
	for _, tc := range cases {
		if got := percentile(sorted, tc.q); got != tc.want {
			t.Errorf("percentile(q=%g) = %v, want %v", tc.q, got, tc.want)
		}
	}
}

// TestPercentileClampsRange proves q outside [0,1] is clamped rather than
// panicking on an out-of-range index.
func TestPercentileClampsRange(t *testing.T) {
	sorted := []time.Duration{ms(1), ms(2), ms(3)}
	if got := percentile(sorted, -0.5); got != ms(1) {
		t.Errorf("percentile(-0.5) = %v, want first %v", got, ms(1))
	}
	if got := percentile(sorted, 1.5); got != ms(3) {
		t.Errorf("percentile(1.5) = %v, want last %v", got, ms(3))
	}
}

// TestPercentileEmpty proves an empty sample yields a zero duration (no panic).
func TestPercentileEmpty(t *testing.T) {
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("percentile(nil) = %v, want 0", got)
	}
}

// TestComputePercentilesUnsortedInput proves the aggregator sorts its input
// itself (so the driver can append latencies in completion order) and derives
// min/max/mean/quantiles correctly, and that it does NOT mutate the caller's
// slice.
func TestComputePercentilesUnsortedInput(t *testing.T) {
	in := []time.Duration{ms(5), ms(1), ms(3), ms(2), ms(4)}
	orig := append([]time.Duration(nil), in...)

	p := computePercentiles(in)

	if p.Count != 5 {
		t.Errorf("Count = %d, want 5", p.Count)
	}
	if p.Min != ms(1) {
		t.Errorf("Min = %v, want 1ms", p.Min)
	}
	if p.Max != ms(5) {
		t.Errorf("Max = %v, want 5ms", p.Max)
	}
	if p.Mean != ms(3) { // (1+2+3+4+5)/5 = 3
		t.Errorf("Mean = %v, want 3ms", p.Mean)
	}
	// n=5, indices 0..4: p50 -> round(0.5*4)=2 -> ms(3).
	if p.P50 != ms(3) {
		t.Errorf("P50 = %v, want 3ms", p.P50)
	}
	// Ordering invariant.
	if !(p.P50 <= p.P95 && p.P95 <= p.P99 && p.P99 <= p.P999) {
		t.Errorf("percentiles not ordered: p50=%v p95=%v p99=%v p999=%v", p.P50, p.P95, p.P99, p.P999)
	}
	// Caller's slice untouched.
	for i := range orig {
		if in[i] != orig[i] {
			t.Fatalf("computePercentiles mutated input at %d: got %v want %v", i, in[i], orig[i])
		}
	}
}

// TestComputePercentilesEmpty proves an empty input is a zero-value Percentiles.
func TestComputePercentilesEmpty(t *testing.T) {
	p := computePercentiles(nil)
	if p.Count != 0 || p.Min != 0 || p.Max != 0 || p.Mean != 0 {
		t.Errorf("empty percentiles = %+v, want all zero", p)
	}
}

// TestBucketFor pins the status->bucket mapping including the transport-error
// (status 0) case.
func TestBucketFor(t *testing.T) {
	cases := []struct {
		status int
		want   StatusBucket
	}{
		{200, StatusOK},
		{201, StatusOK},
		{299, StatusOK},
		{429, StatusThrottled},
		{503, StatusUnavailable},
		{400, StatusError},
		{401, StatusError},
		{500, StatusError},
		{0, StatusError}, // transport error, no response
	}
	for _, tc := range cases {
		if got := bucketFor(tc.status); got != tc.want {
			t.Errorf("bucketFor(%d) = %d, want %d", tc.status, got, tc.want)
		}
	}
}

// TestSummarizeMixedStatuses drives the full aggregation with a hand-built mix of
// statuses and asserts every count, the throughput, the error rate, the token
// rate, and the latency distributions against exact values.
func TestSummarizeMixedStatuses(t *testing.T) {
	// 10 requests over exactly 2s: 6x 200, 2x 429, 1x 503, 1x 500.
	results := []Result{
		{Latency: ms(10), Status: 200, Tokens: 5},
		{Latency: ms(20), Status: 200, Tokens: 5},
		{Latency: ms(30), Status: 200, Tokens: 5},
		{Latency: ms(40), Status: 200, Tokens: 5},
		{Latency: ms(50), Status: 200, Tokens: 5},
		{Latency: ms(60), Status: 200, Tokens: 5},
		{Latency: ms(1), Status: 429},
		{Latency: ms(2), Status: 429},
		{Latency: ms(3), Status: 503},
		{Latency: ms(4), Status: 500},
	}
	s := Summarize(results, 2*time.Second)

	if s.Total != 10 {
		t.Errorf("Total = %d, want 10", s.Total)
	}
	if s.Success != 6 {
		t.Errorf("Success = %d, want 6", s.Success)
	}
	if s.Throttled != 2 {
		t.Errorf("Throttled = %d, want 2", s.Throttled)
	}
	if s.Unavailable != 1 {
		t.Errorf("Unavailable = %d, want 1", s.Unavailable)
	}
	if s.Errors != 1 {
		t.Errorf("Errors = %d, want 1", s.Errors)
	}
	if s.Throughput != 5.0 { // 10 / 2s
		t.Errorf("Throughput = %v, want 5.0", s.Throughput)
	}
	if s.SuccessThroughput != 3.0 { // 6 / 2s
		t.Errorf("SuccessThroughput = %v, want 3.0", s.SuccessThroughput)
	}
	if s.TotalTokens != 30 { // 6 * 5
		t.Errorf("TotalTokens = %d, want 30", s.TotalTokens)
	}
	if s.TokensPerSec != 15.0 { // 30 / 2s
		t.Errorf("TokensPerSec = %v, want 15.0", s.TokensPerSec)
	}
	if s.ErrorRate != 0.4 { // (10-6)/10
		t.Errorf("ErrorRate = %v, want 0.4", s.ErrorRate)
	}
	// All-request latency distribution includes the fast rejections.
	if s.Latency.Count != 10 {
		t.Errorf("Latency.Count = %d, want 10", s.Latency.Count)
	}
	if s.Latency.Min != ms(1) {
		t.Errorf("Latency.Min = %v, want 1ms", s.Latency.Min)
	}
	if s.Latency.Max != ms(60) {
		t.Errorf("Latency.Max = %v, want 60ms", s.Latency.Max)
	}
	// Success-only latency distribution excludes them: min is the fastest 200.
	if s.SuccessLatency.Count != 6 {
		t.Errorf("SuccessLatency.Count = %d, want 6", s.SuccessLatency.Count)
	}
	if s.SuccessLatency.Min != ms(10) {
		t.Errorf("SuccessLatency.Min = %v, want 10ms", s.SuccessLatency.Min)
	}
	if s.SuccessLatency.Max != ms(60) {
		t.Errorf("SuccessLatency.Max = %v, want 60ms", s.SuccessLatency.Max)
	}
}

// TestSummarizeZeroElapsed proves a zero/negative elapsed produces zero rate
// fields rather than NaN/Inf (division guarded), and still counts statuses.
func TestSummarizeZeroElapsed(t *testing.T) {
	results := []Result{{Latency: ms(5), Status: 200, Tokens: 3}}
	s := Summarize(results, 0)
	if s.Throughput != 0 || s.SuccessThroughput != 0 || s.TokensPerSec != 0 {
		t.Errorf("rates with zero elapsed = %v/%v/%v, want all 0", s.Throughput, s.SuccessThroughput, s.TokensPerSec)
	}
	if s.Success != 1 || s.TotalTokens != 3 {
		t.Errorf("counts with zero elapsed = success %d tokens %d, want 1/3", s.Success, s.TotalTokens)
	}
}

// TestSummarizeEmpty proves an empty run is all-zero with no division by zero.
func TestSummarizeEmpty(t *testing.T) {
	s := Summarize(nil, time.Second)
	if s.Total != 0 || s.ErrorRate != 0 || s.Throughput != 0 {
		t.Errorf("empty summary = %+v, want all zero", s)
	}
	if s.Latency.Count != 0 {
		t.Errorf("empty latency count = %d, want 0", s.Latency.Count)
	}
}

// TestSummarizeTransportErrorBucket proves a transport error (status 0, Err set)
// is counted in the Errors bucket and the error rate, not as success.
func TestSummarizeTransportErrorBucket(t *testing.T) {
	results := []Result{
		{Latency: ms(5), Status: 200},
		{Latency: ms(1), Status: 0, Err: errors.New("connection refused")},
	}
	s := Summarize(results, time.Second)
	if s.Success != 1 {
		t.Errorf("Success = %d, want 1", s.Success)
	}
	if s.Errors != 1 {
		t.Errorf("Errors = %d, want 1 (transport error)", s.Errors)
	}
	if s.ErrorRate != 0.5 {
		t.Errorf("ErrorRate = %v, want 0.5", s.ErrorRate)
	}
}
