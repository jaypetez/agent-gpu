package loadtest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Config configures a load run. The zero value is not usable; build it via the
// cmd layer's flags. Validate enforces the invariants the driver relies on.
type Config struct {
	// BaseURL is the deployment's HTTP API base (e.g. "http://localhost:8080"),
	// without a trailing slash (Validate trims one).
	BaseURL string
	// Token is the Bearer token the load requests authenticate with (a user key
	// permitted for Model). Required.
	Token string

	// Concurrency is the number of in-flight requests. In closed-loop mode it is
	// the number of worker goroutines; in open-loop mode it bounds the number of
	// simultaneously outstanding sends. Must be >= 1.
	Concurrency int

	// Duration bounds the run by wall-clock time. Exactly one of Duration or
	// Requests must be set (> 0); the other must be zero.
	Duration time.Duration
	// Requests bounds the run by a fixed number of requests. See Duration.
	Requests int

	// Rate is the open-loop target arrival rate in requests/sec. Zero selects
	// closed-loop mode (Concurrency workers looping as fast as the server
	// allows). A positive Rate schedules sends at a fixed cadence and measures
	// latency from the intended send time (coordinated-omission-aware).
	Rate float64

	// Mix is the weighted endpoint distribution to issue. Build it from a single
	// endpoint (SingleEndpointMix) or a spec (ParseMix). Must be non-empty.
	Mix Mix

	// Model is the model name chat/completions requests target. Required when the
	// mix includes chat or completions.
	Model string
	// Prompt is the user prompt / completion prompt sent in each inference
	// request. Defaults to a short fixed prompt when empty.
	Prompt string

	// HTTPClient is the client used for requests. Defaults to a client with
	// generous per-request timeout and a connection pool sized for Concurrency.
	HTTPClient *http.Client

	// Now is the injectable clock (defaults to time.Now), so tests measure
	// deterministic latencies. Production uses the wall clock.
	Now func() time.Time
}

// defaultPrompt is the inference prompt used when Config.Prompt is empty. It is
// short and deterministic so the backend's work is dominated by routing/overhead
// rather than the prompt — the harness measures the gateway, not the model.
const defaultPrompt = "ping"

// defaultRequestTimeout bounds a single request so a hung server cannot wedge a
// worker forever; it is generous because a real model can take many seconds.
const defaultRequestTimeout = 60 * time.Second

// validate checks the config invariants and normalizes derived defaults
// (trimming the base URL, defaulting the prompt/clock/client). It returns a
// descriptive error for any misconfiguration so the CLI surfaces a usage error.
func (c *Config) validate() error {
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	if c.BaseURL == "" {
		return fmt.Errorf("base url is required")
	}
	if c.Token == "" {
		return fmt.Errorf("token is required")
	}
	if c.Concurrency < 1 {
		return fmt.Errorf("concurrency must be >= 1")
	}
	if (c.Duration > 0) == (c.Requests > 0) {
		return fmt.Errorf("set exactly one of duration or requests")
	}
	if c.Rate < 0 {
		return fmt.Errorf("rate must be >= 0")
	}
	if len(c.Mix.entries) == 0 {
		return fmt.Errorf("mix must have at least one endpoint")
	}
	needsModel := false
	for _, e := range c.Mix.entries {
		if e.Endpoint == EndpointChat || e.Endpoint == EndpointCompletions {
			needsModel = true
		}
	}
	if needsModel && c.Model == "" {
		return fmt.Errorf("model is required when the mix includes chat or completions")
	}
	if c.Prompt == "" {
		c.Prompt = defaultPrompt
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.HTTPClient == nil {
		c.HTTPClient = defaultHTTPClient(c.Concurrency)
	}
	return nil
}

// defaultHTTPClient builds an *http.Client whose transport pools enough
// connections for the configured concurrency, so the load generator is not
// itself a bottleneck (the stdlib default caps idle conns per host at 2, which
// would serialize a high-concurrency run). It sets a per-request timeout so a
// stuck request fails rather than wedging a worker.
func defaultHTTPClient(concurrency int) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = concurrency * 2
	tr.MaxIdleConnsPerHost = concurrency * 2
	tr.MaxConnsPerHost = 0 // unlimited: the driver bounds concurrency itself
	return &http.Client{Transport: tr, Timeout: defaultRequestTimeout}
}

// Driver issues the configured load against the deployment and collects a
// per-request Result for each. Construct it with NewDriver; Run executes the run
// and returns the raw results, which Summarize aggregates.
type Driver struct {
	cfg Config
}

// NewDriver validates cfg and returns a Driver, or an error if the config is
// invalid. The validated/normalized config is captured, so callers should read
// back the final values (e.g. the defaulted prompt) from the returned Driver via
// Config.
func NewDriver(cfg Config) (*Driver, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Driver{cfg: cfg}, nil
}

// Config returns the driver's normalized configuration (after defaults), so the
// report can render exactly what ran.
func (d *Driver) Config() Config { return d.cfg }

// Run executes the load and returns the per-request results plus the run's
// wall-clock elapsed time. ctx cancellation (e.g. SIGINT) stops the run early
// and returns whatever results were collected so far — a partial run still
// produces a usable summary. Run dispatches to the closed- or open-loop strategy
// based on Config.Rate.
func (d *Driver) Run(ctx context.Context) (results []Result, elapsed time.Duration) {
	start := d.cfg.Now()
	if d.cfg.Rate > 0 {
		results = d.runOpenLoop(ctx)
	} else {
		results = d.runClosedLoop(ctx)
	}
	return results, d.cfg.Now().Sub(start)
}

// collector accumulates Results from many goroutines under a mutex. A mutex
// (rather than a channel drain) keeps the hot path simple and the append cheap;
// contention is negligible relative to a network round-trip.
type collector struct {
	mu      sync.Mutex
	results []Result
}

func (c *collector) add(r Result) {
	c.mu.Lock()
	c.results = append(c.results, r)
	c.mu.Unlock()
}

func (c *collector) take() []Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.results
}

// runClosedLoop runs Concurrency worker goroutines, each issuing requests
// back-to-back (send, await response, repeat) until the deadline or request
// budget is reached, or ctx is cancelled. Throughput is emergent: a slower
// server slows the send rate. A shared atomic counter assigns each request a
// monotonic index so the mix is spread deterministically across the whole run
// regardless of which worker issues a given request.
func (d *Driver) runClosedLoop(ctx context.Context) []Result {
	col := &collector{}
	if d.cfg.Requests > 0 {
		col.results = make([]Result, 0, d.cfg.Requests)
	}

	// runCtx adds the duration deadline (if any) on top of the caller's ctx, so
	// both a SIGINT and the time budget end the run.
	runCtx := ctx
	if d.cfg.Duration > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, d.cfg.Duration)
		defer cancel()
	}

	// next hands out monotonic request indices; budget (when Requests > 0) caps
	// the total. Both are coordinated through a single mutex-guarded counter so a
	// request budget is honored exactly across workers.
	var (
		mu       sync.Mutex
		issued   int
		reqIndex int
	)
	// claim returns the next request index and whether the run should issue it
	// (false when the request budget is exhausted).
	claim := func() (int, bool) {
		mu.Lock()
		defer mu.Unlock()
		if d.cfg.Requests > 0 && issued >= d.cfg.Requests {
			return 0, false
		}
		i := reqIndex
		reqIndex++
		issued++
		return i, true
	}

	var wg sync.WaitGroup
	for w := 0; w < d.cfg.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if runCtx.Err() != nil {
					return
				}
				i, ok := claim()
				if !ok {
					return
				}
				// Closed loop: the intended send time IS now (no schedule to fall
				// behind), so latency is the plain request duration.
				if res, record := d.issue(runCtx, i, d.cfg.Now()); record {
					col.add(res)
				}
			}
		}()
	}
	wg.Wait()
	return col.take()
}

// runOpenLoop schedules requests at the configured fixed arrival rate (Rate
// req/s) independent of how fast the server responds, and measures each
// request's latency from its INTENDED send time. This is the
// coordinated-omission-aware path: if all Concurrency slots are busy when a
// request is due, that request waits for a free slot, and the wait is folded
// into its measured latency (because timing starts at the intended time, not the
// actual send). Under saturation the resulting tail reflects true
// client-observed latency, which closed-loop would mask.
//
// Concurrency bounds the number of simultaneously outstanding requests via a
// semaphore, so a slow server cannot spawn unbounded goroutines. The run ends at
// the duration deadline or after Requests have been scheduled, or on ctx cancel;
// it then waits for outstanding requests to finish so every scheduled request
// contributes a result.
func (d *Driver) runOpenLoop(ctx context.Context) []Result {
	col := &collector{}
	if d.cfg.Requests > 0 {
		col.results = make([]Result, 0, d.cfg.Requests)
	}

	runCtx := ctx
	if d.cfg.Duration > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, d.cfg.Duration)
		defer cancel()
	}

	interval := time.Duration(float64(time.Second) / d.cfg.Rate)
	if interval <= 0 {
		interval = time.Nanosecond
	}

	// sem bounds outstanding requests to Concurrency. A buffered channel is the
	// classic counting semaphore.
	sem := make(chan struct{}, d.cfg.Concurrency)
	var wg sync.WaitGroup

	start := d.cfg.Now()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for i := 0; ; i++ {
		if d.cfg.Requests > 0 && i >= d.cfg.Requests {
			break
		}
		// The intended send time for request i is start + i*interval. Measuring
		// from this (not from when a slot frees) is what makes the latency
		// coordinated-omission-aware.
		intended := start.Add(time.Duration(i) * interval)

		select {
		case <-runCtx.Done():
			wg.Wait()
			return col.take()
		case <-ticker.C:
		}

		// Acquire a concurrency slot. If the server is saturated and all slots are
		// busy, this blocks — and because `intended` was captured before the wait,
		// the slot-wait time is included in the request's measured latency.
		select {
		case <-runCtx.Done():
			wg.Wait()
			return col.take()
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func(idx int, intendedAt time.Time) {
			defer wg.Done()
			defer func() { <-sem }()
			if res, record := d.issue(runCtx, idx, intendedAt); record {
				col.add(res)
			}
		}(i, intended)
	}

	wg.Wait()
	return col.take()
}

// issue performs one request for index i (selecting the endpoint from the mix),
// timing it from intendedAt to the moment the response body is fully read. It
// returns the Result and whether it should be recorded. A transport error yields
// Status 0 with Err set; an HTTP error status (4xx/5xx) is a normal Result with
// that status (it is a real response, not a transport failure). The response
// body is always drained and closed so connections are reused.
//
// The record bool is false only when a transport error coincides with the run
// context being done: that request was aborted because the run ended (the
// deadline fired or the user hit Ctrl-C mid-flight), not because the server
// failed, so counting it would inflate the error rate with shutdown noise. A
// transport error while the run is still live (a genuine connection refused /
// timeout) IS recorded.
func (d *Driver) issue(ctx context.Context, i int, intendedAt time.Time) (Result, bool) {
	ep := d.cfg.Mix.Pick(i)
	req, err := d.buildRequest(ctx, ep)
	if err != nil {
		// A request that cannot even be built (should not happen with a valid
		// config) is recorded as an error with the latency since intendedAt.
		return Result{Latency: d.cfg.Now().Sub(intendedAt), Status: 0, Err: err}, true
	}

	resp, err := d.cfg.HTTPClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			// Run ended mid-request: discard rather than count an aborted request.
			return Result{}, false
		}
		return Result{Latency: d.cfg.Now().Sub(intendedAt), Status: 0, Err: err}, true
	}
	// Read the body so the full round-trip (not just headers) is timed and the
	// connection can be reused. Parse usage tokens from a successful inference
	// response so a tokens/sec figure is available.
	tokens := readTokens(resp, ep)
	latency := d.cfg.Now().Sub(intendedAt)
	return Result{Latency: latency, Status: resp.StatusCode, Tokens: tokens}, true
}

// buildRequest constructs the HTTP request for one endpoint, setting the Bearer
// token and JSON content type for the POST inference endpoints. GET /v1/models
// carries only the auth header.
func (d *Driver) buildRequest(ctx context.Context, ep Endpoint) (*http.Request, error) {
	switch ep {
	case EndpointModels:
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.cfg.BaseURL+"/v1/models", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+d.cfg.Token)
		return req, nil
	case EndpointCompletions:
		body := completionsBody{Model: d.cfg.Model, Prompt: d.cfg.Prompt, Stream: false}
		return d.jsonPost(ctx, "/v1/completions", body)
	case EndpointChat:
		fallthrough
	default:
		body := chatBody{
			Model:    d.cfg.Model,
			Stream:   false,
			Messages: []chatMsg{{Role: "user", Content: d.cfg.Prompt}},
		}
		return d.jsonPost(ctx, "/v1/chat/completions", body)
	}
}

// jsonPost builds a JSON POST request to path with the Bearer token set.
func (d *Driver) jsonPost(ctx context.Context, path string, body any) (*http.Request, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.BaseURL+path, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+d.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// readTokens drains resp.Body (so the connection is reused and the full body is
// timed) and, for a successful inference response, returns the total_tokens from
// the OpenAI usage object. For a non-2xx response or /v1/models it just drains
// and returns 0. It always closes the body.
func readTokens(resp *http.Response, ep Endpoint) uint64 {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || ep == EndpointModels {
		// Drain and discard: we only need the status and the round-trip timing.
		_, _ = io.Copy(io.Discard, resp.Body)
		return 0
	}
	// Cap the read so a misbehaving server cannot make the driver buffer without
	// bound; an inference response's usage object is tiny and lives near the end,
	// but the body is small in the echo baseline and modest for real models.
	var parsed struct {
		Usage struct {
			TotalTokens uint64 `json:"total_tokens"`
		} `json:"usage"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// Drain any remainder past the cap so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = json.Unmarshal(body, &parsed)
	return parsed.Usage.TotalTokens
}

// ---- request body shapes (the subset the endpoints act on) ----

type chatBody struct {
	Model    string    `json:"model"`
	Messages []chatMsg `json:"messages"`
	Stream   bool      `json:"stream"`
}

type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type completionsBody struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}
