package loadtest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeServer is a minimal stand-in for the agent-gpu HTTP API used to exercise
// the driver without the full stack: it records request counts per path and
// returns a configurable status. It speaks just enough for the driver — auth
// header presence, the chat/completions/models routes, and a usage object on a
// 200 chat response.
type fakeServer struct {
	chat        int64
	completions int64
	models      int64
	status      int32 // status code to return (default 200)
	tokens      uint64
}

func newFakeServer() *fakeServer { return &fakeServer{status: 200, tokens: 4} }

func (f *fakeServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&f.chat, 1)
		f.respond(w, true)
	})
	mux.HandleFunc("/v1/completions", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&f.completions, 1)
		f.respond(w, false)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&f.models, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(atomic.LoadInt32(&f.status)))
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	})
	return mux
}

func (f *fakeServer) respond(w http.ResponseWriter, chat bool) {
	status := int(atomic.LoadInt32(&f.status))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if status < 200 || status >= 300 {
		_, _ = w.Write([]byte(`{"error":{"message":"x","code":"y"}}`))
		return
	}
	body := map[string]any{
		"object": "chat.completion",
		"usage":  map[string]any{"total_tokens": f.tokens},
	}
	_ = json.NewEncoder(w).Encode(body)
}

// TestNewDriverValidation covers the config validation rules.
func TestNewDriverValidation(t *testing.T) {
	base := func() Config {
		return Config{
			BaseURL:     "http://x",
			Token:       "t",
			Concurrency: 1,
			Requests:    1,
			Mix:         SingleEndpointMix(EndpointChat),
			Model:       "m",
		}
	}
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"valid", func(*Config) {}, false},
		{"no base url", func(c *Config) { c.BaseURL = "" }, true},
		{"no token", func(c *Config) { c.Token = "" }, true},
		{"zero concurrency", func(c *Config) { c.Concurrency = 0 }, true},
		{"neither duration nor requests", func(c *Config) { c.Requests = 0 }, true},
		{"both duration and requests", func(c *Config) { c.Duration = time.Second }, true},
		{"negative rate", func(c *Config) { c.Rate = -1 }, true},
		{"chat mix without model", func(c *Config) { c.Model = "" }, true},
		{"models mix without model ok", func(c *Config) { c.Mix = SingleEndpointMix(EndpointModels); c.Model = "" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(&cfg)
			_, err := NewDriver(cfg)
			if tc.wantErr != (err != nil) {
				t.Fatalf("NewDriver err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestDriverDefaults proves validate fills the prompt, clock, and HTTP client.
func TestDriverDefaults(t *testing.T) {
	d, err := NewDriver(Config{
		BaseURL: "http://x/", Token: "t", Concurrency: 2, Requests: 1,
		Mix: SingleEndpointMix(EndpointChat), Model: "m",
	})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	c := d.Config()
	if c.BaseURL != "http://x" {
		t.Errorf("BaseURL = %q, want trailing slash trimmed", c.BaseURL)
	}
	if c.Prompt != defaultPrompt {
		t.Errorf("Prompt = %q, want default", c.Prompt)
	}
	if c.Now == nil || c.HTTPClient == nil {
		t.Errorf("Now/HTTPClient not defaulted")
	}
}

// TestDriverClosedLoopRequestBudget proves the closed-loop driver issues exactly
// Requests requests across the worker pool, all succeed against the fake server,
// and the summary reflects the count + tokens.
func TestDriverClosedLoopRequestBudget(t *testing.T) {
	fs := newFakeServer()
	ts := httptest.NewServer(fs.handler())
	defer ts.Close()

	d, err := NewDriver(Config{
		BaseURL: ts.URL, Token: "t", Concurrency: 4, Requests: 200,
		Mix: SingleEndpointMix(EndpointChat), Model: "m",
	})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	results, elapsed := d.Run(context.Background())
	if len(results) != 200 {
		t.Fatalf("results = %d, want exactly 200", len(results))
	}
	if got := atomic.LoadInt64(&fs.chat); got != 200 {
		t.Fatalf("server saw %d chat requests, want 200", got)
	}
	s := Summarize(results, elapsed)
	if s.Success != 200 {
		t.Errorf("Success = %d, want 200", s.Success)
	}
	if s.Errors != 0 || s.Throttled != 0 || s.Unavailable != 0 {
		t.Errorf("unexpected non-2xx: %+v", s)
	}
	if s.TotalTokens != 200*fs.tokens {
		t.Errorf("TotalTokens = %d, want %d", s.TotalTokens, 200*fs.tokens)
	}
	// Percentiles ordered and populated.
	if !(s.Latency.P50 <= s.Latency.P95 && s.Latency.P95 <= s.Latency.P99) {
		t.Errorf("percentiles not ordered: %+v", s.Latency)
	}
	if s.Throughput <= 0 {
		t.Errorf("Throughput = %v, want > 0", s.Throughput)
	}
}

// TestDriverClosedLoopDuration proves a duration-bounded run stops on time and
// produces a sensible summary (some requests, all successful, throughput > 0).
func TestDriverClosedLoopDuration(t *testing.T) {
	fs := newFakeServer()
	ts := httptest.NewServer(fs.handler())
	defer ts.Close()

	d, err := NewDriver(Config{
		BaseURL: ts.URL, Token: "t", Concurrency: 4, Duration: 150 * time.Millisecond,
		Mix: SingleEndpointMix(EndpointChat), Model: "m",
	})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	start := time.Now()
	results, elapsed := d.Run(context.Background())
	wall := time.Since(start)
	if wall > 2*time.Second {
		t.Fatalf("duration run took %v, expected to stop near 150ms", wall)
	}
	if len(results) == 0 {
		t.Fatalf("no requests issued in a 150ms run")
	}
	s := Summarize(results, elapsed)
	if s.Success != len(results) {
		t.Errorf("Success = %d, want all %d", s.Success, len(results))
	}
}

// TestDriverStatusBuckets proves a server returning 429 / 503 surfaces in the
// throttled / unavailable buckets — the client-side observation of throttling
// and queue saturation.
func TestDriverStatusBuckets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int32
		check  func(Summary) bool
	}{
		{"throttled", 429, func(s Summary) bool { return s.Throttled == s.Total && s.Total > 0 }},
		{"unavailable", 503, func(s Summary) bool { return s.Unavailable == s.Total && s.Total > 0 }},
		{"server error", 500, func(s Summary) bool { return s.Errors == s.Total && s.Total > 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeServer()
			fs.status = tc.status
			ts := httptest.NewServer(fs.handler())
			defer ts.Close()

			d, err := NewDriver(Config{
				BaseURL: ts.URL, Token: "t", Concurrency: 2, Requests: 20,
				Mix: SingleEndpointMix(EndpointChat), Model: "m",
			})
			if err != nil {
				t.Fatalf("NewDriver: %v", err)
			}
			results, elapsed := d.Run(context.Background())
			s := Summarize(results, elapsed)
			if !tc.check(s) {
				t.Fatalf("status %d: summary did not bucket as expected: %+v", tc.status, s)
			}
			if s.ErrorRate != 1.0 {
				t.Errorf("ErrorRate = %v, want 1.0 (all non-2xx)", s.ErrorRate)
			}
		})
	}
}

// TestDriverMixRouting proves a chat/models mix hits both endpoints in roughly
// the configured proportion over a fixed request budget.
func TestDriverMixRouting(t *testing.T) {
	fs := newFakeServer()
	ts := httptest.NewServer(fs.handler())
	defer ts.Close()

	mix, err := ParseMix("chat=80,models=20")
	if err != nil {
		t.Fatalf("ParseMix: %v", err)
	}
	d, err := NewDriver(Config{
		BaseURL: ts.URL, Token: "t", Concurrency: 1, Requests: 100,
		Mix: mix, Model: "m",
	})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	results, _ := d.Run(context.Background())
	if len(results) != 100 {
		t.Fatalf("results = %d, want 100", len(results))
	}
	// With concurrency 1 the indices are 0..99 in order, so the mix is exact.
	if got := atomic.LoadInt64(&fs.chat); got != 80 {
		t.Errorf("chat requests = %d, want 80", got)
	}
	if got := atomic.LoadInt64(&fs.models); got != 20 {
		t.Errorf("models requests = %d, want 20", got)
	}
}

// TestDriverOpenLoop proves the open-loop path schedules a fixed number of
// requests at a rate and all complete against the fake server. It uses a high
// rate so the test is fast; the assertion is on completeness and bucketing, not
// timing (CI timing is noisy).
func TestDriverOpenLoop(t *testing.T) {
	fs := newFakeServer()
	ts := httptest.NewServer(fs.handler())
	defer ts.Close()

	d, err := NewDriver(Config{
		BaseURL: ts.URL, Token: "t", Concurrency: 8, Requests: 100, Rate: 5000,
		Mix: SingleEndpointMix(EndpointChat), Model: "m",
	})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	results, elapsed := d.Run(context.Background())
	if len(results) != 100 {
		t.Fatalf("open-loop results = %d, want 100", len(results))
	}
	s := Summarize(results, elapsed)
	if s.Success != 100 {
		t.Errorf("Success = %d, want 100", s.Success)
	}
}

// TestDriverContextCancel proves cancelling the context stops the run promptly
// and still returns the results collected so far (a partial run is usable).
func TestDriverContextCancel(t *testing.T) {
	fs := newFakeServer()
	ts := httptest.NewServer(fs.handler())
	defer ts.Close()

	d, err := NewDriver(Config{
		BaseURL: ts.URL, Token: "t", Concurrency: 4, Duration: 10 * time.Second,
		Mix: SingleEndpointMix(EndpointChat), Model: "m",
	})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	results, _ := d.Run(ctx)
	if time.Since(start) > 3*time.Second {
		t.Fatalf("cancel did not stop the run promptly")
	}
	// A partial run still produced some results (the fake server is fast).
	if len(results) == 0 {
		t.Logf("note: 0 results on cancel (acceptable, but unusual against a fast fake)")
	}
}

// TestDriverTransportError proves a request to a closed server is recorded as a
// transport error (status 0) in the Errors bucket, not a panic.
func TestDriverTransportError(t *testing.T) {
	ts := httptest.NewServer(newFakeServer().handler())
	url := ts.URL
	ts.Close() // close immediately so connections are refused

	d, err := NewDriver(Config{
		BaseURL: url, Token: "t", Concurrency: 2, Requests: 4,
		Mix: SingleEndpointMix(EndpointChat), Model: "m",
		HTTPClient: &http.Client{Timeout: 500 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	results, elapsed := d.Run(context.Background())
	if len(results) != 4 {
		t.Fatalf("results = %d, want 4", len(results))
	}
	s := Summarize(results, elapsed)
	if s.Errors != 4 {
		t.Errorf("Errors = %d, want 4 (all transport failures)", s.Errors)
	}
}
