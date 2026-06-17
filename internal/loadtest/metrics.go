// Package loadtest is a dependency-free load-testing harness for an agent-gpu
// deployment. It drives the OpenAI-compatible inference endpoints
// (/v1/chat/completions, /v1/completions) and the discovery endpoint
// (/v1/models) under a configurable concurrency and request mix, times every
// request, and reports throughput, latency percentiles (p50/p95/p99/p99.9), an
// error rate, and a status-code breakdown so throttling (429) and queue
// saturation (503) are directly observable from the client side.
//
// # Scope
//
// This package owns the load DRIVER and the metrics math only; it speaks raw
// net/http so it has full control over per-request timing and status bucketing.
// The cmd layer (cmd/agentgpu loadtest) wires it to a remote deployment or to an
// in-process stack (server + workers + HTTP API) for a reproducible, model-free
// baseline. It is deliberately stdlib-only: the project carries no metrics or
// histogram dependency, and percentiles over a slice of durations need none.
//
// # Open vs closed loop
//
// The driver runs in two modes (see Config.Rate):
//
//   - Closed-loop (Rate == 0): a fixed number of workers each send a request,
//     wait for the response, then immediately send the next. Throughput is an
//     emergent property of latency and concurrency — a slow server simply slows
//     the send rate, which HIDES tail latency under saturation.
//   - Open-loop (Rate > 0): requests are scheduled at a fixed arrival rate
//     independent of how fast the server responds. Latency is measured from the
//     INTENDED send time, not the actual one, so a request that waited for a free
//     worker slot before even being sent still counts that wait. This is the
//     coordinated-omission-aware measurement: under saturation the open-loop tail
//     reflects the true client-observed latency, where closed-loop would not.
//
// docs/load-testing.md explains the trade-off and when to use each.
package loadtest

import (
	"math"
	"sort"
	"time"
)

// Result is the outcome of a single request the driver issued: how long it took
// (measured from its intended send time — see the package open-loop note) and
// the bucket its HTTP status fell into. Latency is recorded for every completed
// request regardless of status, so a fast 429/503 rejection contributes to the
// latency distribution just like a 200 (under saturation a server may reject
// quickly, and hiding those from the distribution would flatter the tail).
type Result struct {
	// Latency is the wall-clock time from the request's intended send time to the
	// response headers + body being fully read.
	Latency time.Duration
	// Status is the HTTP status code (0 when the request never got a response,
	// e.g. a transport error — bucketed as StatusError).
	Status int
	// Tokens is the total_tokens reported in the response usage object, when the
	// endpoint returned one (chat/completions). Zero otherwise; summed across
	// successful requests to derive a tokens/sec figure.
	Tokens uint64
	// Err is the transport-level error, if any (connection refused, timeout). A
	// non-2xx HTTP response is NOT an error here — it is a real response with a
	// status code; Err is reserved for "no response at all".
	Err error
}

// StatusBucket is one row of the status-code breakdown: 2xx (success), 429
// (rate-limited / throttled), 503 (queue saturation / unavailable), or other
// (any other status, plus transport errors). The buckets are how a remote
// client observes throttling and queueing without admin access.
type StatusBucket int

const (
	// StatusOK counts 2xx responses (a completed inference).
	StatusOK StatusBucket = iota
	// StatusThrottled counts HTTP 429 (global or per-key rate limit).
	StatusThrottled
	// StatusUnavailable counts HTTP 503 (bounded queue full / shutting down).
	StatusUnavailable
	// StatusError counts every other outcome: any non-2xx status not 429/503, and
	// transport errors (no response).
	StatusError
)

// bucketFor maps an HTTP status code to its StatusBucket. A status of 0 (no
// response — transport error) buckets as StatusError.
func bucketFor(status int) StatusBucket {
	switch {
	case status >= 200 && status < 300:
		return StatusOK
	case status == 429:
		return StatusThrottled
	case status == 503:
		return StatusUnavailable
	default:
		return StatusError
	}
}

// Summary is the aggregated outcome of a whole run: counts, throughput, the
// latency-percentile distribution, the error rate, and the status breakdown. It
// is the value the report renders and the --json flag emits, so it is the stable
// comparison record for baselines.
type Summary struct {
	// Total is the number of requests that completed (a response or a transport
	// error). Throughput and rates are computed over this count.
	Total int
	// Success is the number of 2xx responses.
	Success int
	// Throttled / Unavailable / Errors are the non-OK buckets.
	Throttled   int
	Unavailable int
	Errors      int

	// Elapsed is the wall-clock duration of the run (first send to last
	// completion), the denominator for throughput.
	Elapsed time.Duration
	// Throughput is completed requests per second over Elapsed.
	Throughput float64
	// SuccessThroughput is 2xx responses per second over Elapsed (the useful work
	// rate; under saturation it diverges from Throughput as rejections climb).
	SuccessThroughput float64
	// TokensPerSec is total reported tokens per second over Elapsed (0 when the
	// exercised endpoint reports no usage, e.g. /v1/models).
	TokensPerSec float64
	// TotalTokens is the sum of reported tokens across successful requests.
	TotalTokens uint64

	// ErrorRate is the fraction of completed requests that were not 2xx
	// (throttled + unavailable + errors) / total, in [0,1].
	ErrorRate float64

	// Latency holds the percentile distribution over EVERY completed request.
	Latency Percentiles
	// SuccessLatency holds the same distribution over 2xx requests only, so the
	// latency of useful work is not distorted by fast rejections under saturation.
	SuccessLatency Percentiles
}

// Percentiles is a latency distribution: min, the standard tail quantiles, and
// max, plus the mean and the sample count. All durations are exact (computed by
// indexing a sorted slice — no interpolation), so the same input always yields
// the same numbers. P50 <= P95 <= P99 <= P999 always holds for n >= 1.
type Percentiles struct {
	Count int
	Min   time.Duration
	P50   time.Duration
	P90   time.Duration
	P95   time.Duration
	P99   time.Duration
	P999  time.Duration // p99.9
	Max   time.Duration
	Mean  time.Duration
}

// percentile returns the value at quantile q (in [0,1]) of sorted, using the
// nearest-rank method: index = round(q * (n-1)). sorted must be sorted ascending
// and non-empty. q is clamped to [0,1]. This is the textbook
// index-into-sorted-slice percentile; it needs no interpolation and is exact and
// reproducible for a given sample, which is what makes the unit tests assert
// concrete values.
func percentile(sorted []time.Duration, q float64) time.Duration {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if q < 0 {
		q = 0
	}
	if q > 1 {
		q = 1
	}
	// Nearest-rank on a 0-based slice: q=0 -> first, q=1 -> last.
	idx := int(math.Round(q * float64(n-1)))
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx]
}

// computePercentiles builds a Percentiles from latencies. It copies and sorts
// the input (leaving the caller's slice untouched), then derives min/max/mean
// and the quantiles by indexing the sorted copy. An empty input yields a
// zero-value Percentiles (Count 0), so callers need not special-case "no
// samples".
func computePercentiles(latencies []time.Duration) Percentiles {
	n := len(latencies)
	if n == 0 {
		return Percentiles{}
	}
	sorted := make([]time.Duration, n)
	copy(sorted, latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var sum time.Duration
	for _, d := range sorted {
		sum += d
	}

	return Percentiles{
		Count: n,
		Min:   sorted[0],
		P50:   percentile(sorted, 0.50),
		P90:   percentile(sorted, 0.90),
		P95:   percentile(sorted, 0.95),
		P99:   percentile(sorted, 0.99),
		P999:  percentile(sorted, 0.999),
		Max:   sorted[n-1],
		Mean:  time.Duration(int64(sum) / int64(n)),
	}
}

// Summarize aggregates a slice of per-request Results plus the run's wall-clock
// elapsed time into a Summary: it buckets statuses, computes throughput and the
// error rate over the total, and builds the latency distributions (over all
// requests and over successful ones). A zero or negative elapsed is treated as
// "no measurable duration" so the rate fields are 0 rather than +Inf/NaN. It is
// pure (no I/O), so it is unit-tested with exact known inputs.
func Summarize(results []Result, elapsed time.Duration) Summary {
	s := Summary{Total: len(results), Elapsed: elapsed}

	allLat := make([]time.Duration, 0, len(results))
	okLat := make([]time.Duration, 0, len(results))
	for _, r := range results {
		allLat = append(allLat, r.Latency)
		switch bucketFor(r.Status) {
		case StatusOK:
			s.Success++
			s.TotalTokens += r.Tokens
			okLat = append(okLat, r.Latency)
		case StatusThrottled:
			s.Throttled++
		case StatusUnavailable:
			s.Unavailable++
		default:
			s.Errors++
		}
	}

	if elapsed > 0 {
		secs := elapsed.Seconds()
		s.Throughput = float64(s.Total) / secs
		s.SuccessThroughput = float64(s.Success) / secs
		s.TokensPerSec = float64(s.TotalTokens) / secs
	}
	if s.Total > 0 {
		s.ErrorRate = float64(s.Total-s.Success) / float64(s.Total)
	}

	s.Latency = computePercentiles(allLat)
	s.SuccessLatency = computePercentiles(okLat)
	return s
}
