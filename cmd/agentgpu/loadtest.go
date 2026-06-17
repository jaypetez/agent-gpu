package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/jaypetez/agent-gpu/internal/config"
	"github.com/jaypetez/agent-gpu/internal/loadtest"
)

// loadtestUsage is the help text for `agentgpu loadtest`. The two modes and the
// saturation knobs are documented inline so `--help` is self-sufficient; the full
// how-to-run + how-to-interpret guide is docs/load-testing.md.
const loadtestUsage = `Usage: agentgpu loadtest [--mode remote|inproc] [flags]

Drive a load scenario against an agent-gpu deployment and report throughput,
latency percentiles (p50/p95/p99/p99.9), an error rate, and a status-code
breakdown (200/429/503/other) so throttling and queue saturation are observable.

Modes:
  remote   (default) load a running deployment over its HTTP API. Set --url and a
           user --token (or $AGENTGPU_TOKEN). Optionally pass --admin-token to
           poll GET /v1/admin/stats for queue-depth/wait-time during the run.
  inproc   spin up a full in-process stack (server + echo workers over an
           in-memory transport + HTTP API) and load it. No Ollama/GPU needed, so
           this is the reproducible, model-free baseline path. Use --workers for
           multi-worker routing, --global-rpm/--global-tpm to elicit throttling
           (429), and --think to make client-observed latency climb under load.

Load shape:
  --concurrency N    in-flight requests (closed-loop workers, or open-loop cap)
  --duration D       run for a wall-clock duration (e.g. 30s); OR
  --requests N       run a fixed number of requests (exactly one of the two)
  --rate R           open-loop arrival rate in req/s (0 = closed-loop). Open-loop
                     measures latency from the intended send time, so the tail
                     under saturation is not hidden (coordinated-omission-aware).
  --endpoint E       one of chat|completions|models (default chat); OR
  --mix SPEC         a weighted mix like "chat=80,models=20"
  --model M          model name for chat/completions (default echo-model inproc)
  --prompt P         prompt text for inference requests
  --json             emit the run report as JSON (for baseline comparison)

Examples:
  agentgpu loadtest --mode inproc --workers 2 --concurrency 16 --requests 2000
  agentgpu loadtest --mode inproc --concurrency 16 --requests 2000 --global-rpm 100
  agentgpu loadtest --url http://localhost:8080 --token $AGENTGPU_TOKEN \
    --concurrency 32 --duration 30s --endpoint chat --model llama3 \
    --admin-token $ADMIN --stats-interval 1s`

// runLoadtestCmd routes the `loadtest` subcommand. It parses flags, builds a
// loadtest.Config, runs either the remote or in-process strategy, and prints the
// report (text by default, JSON with --json). out is the command's writer
// (os.Stdout in production); ctx is cancelled on SIGINT/SIGTERM for a clean early
// stop that still prints a partial report.
func runLoadtestCmd(ctx context.Context, logger *slog.Logger, out io.Writer, args []string) error {
	if len(args) >= 1 && isHelpArg(args[0]) {
		return groupHelp(out, loadtestUsage)
	}

	f, err := parseLoadtestFlags(out, args)
	if err != nil {
		return err
	}

	switch f.mode {
	case "inproc":
		return runLoadtestInProc(ctx, logger, out, f)
	case "remote", "":
		return runLoadtestRemote(ctx, out, f)
	default:
		return usagef("loadtest: unknown --mode %q (want remote or inproc)", f.mode)
	}
}

// runLoadtestRemote loads a running deployment over HTTP. It resolves the base
// URL and user token (flag > env), builds the driver, optionally starts an admin
// stats poller, runs the load, and prints the report.
func runLoadtestRemote(ctx context.Context, out io.Writer, f *loadtestFlags) error {
	baseURL := config.ResolveHTTPAddr(f.url, nil)
	token := config.ResolveToken(f.token, nil)
	if token == "" {
		return usagef("loadtest: no token configured: set --token or $%s (a user key permitted for --model)", config.EnvToken)
	}

	mix, model, err := f.resolveMixAndModel("")
	if err != nil {
		return err
	}

	cfg := loadtest.Config{
		BaseURL:     baseURL,
		Token:       token,
		Concurrency: f.concurrency,
		Duration:    f.duration,
		Requests:    f.requests,
		Rate:        f.rate,
		Mix:         mix,
		Model:       model,
		Prompt:      f.prompt,
	}
	driver, err := loadtest.NewDriver(cfg)
	if err != nil {
		return usagef("loadtest: %v", err)
	}

	// Optional saturation polling against the admin stats endpoint: only when an
	// admin token is supplied (the user token cannot read /v1/admin/stats).
	var src loadtest.StatsSource
	if f.adminToken != "" {
		src = &loadtest.HTTPStatsSource{BaseURL: baseURL, Token: f.adminToken}
	}

	progress(out, f.asJSON, "running load test (remote)... press Ctrl-C to stop early")
	results, elapsed, sat := runWithPoller(ctx, driver, src, f.statsInterval)

	report := loadtest.NewRunReport(remoteReportConfig(driver.Config(), "remote"), loadtest.Summarize(results, elapsed), sat)
	return emitReport(out, report, f.asJSON)
}

// runLoadtestInProc spins up the in-process stack, loads it with the same driver,
// reads saturation straight off the live server, and prints the report. This is
// the reproducible baseline path (no external deployment, no Ollama).
func runLoadtestInProc(ctx context.Context, logger *slog.Logger, out io.Writer, f *loadtestFlags) error {
	// The in-process stack mints its own keys, so a token/url is neither needed nor
	// used here. A logger is passed only when -v-style debugging is wanted; default
	// to a quiet stack so the report is not buried in server logs.
	_ = logger

	stack, err := loadtest.StartInProc(ctx, loadtest.InProcConfig{
		Model:         f.model,
		Workers:       f.workers,
		QueueMaxDepth: f.queueMaxDepth,
		GlobalRPM:     f.globalRPM,
		GlobalTPM:     f.globalTPM,
		Think:         f.think,
	})
	if err != nil {
		return fmt.Errorf("loadtest: start in-process stack: %w", err)
	}
	defer stack.Close()

	// Default the model to whatever the stack advertised (echo-model) when the user
	// did not name one, so chat/completions requests target a routable model.
	mix, model, err := f.resolveMixAndModel(loadtestInProcModel(f.model))
	if err != nil {
		return err
	}

	cfg := loadtest.Config{
		BaseURL:     stack.BaseURL,
		Token:       stack.UserToken,
		Concurrency: f.concurrency,
		Duration:    f.duration,
		Requests:    f.requests,
		Rate:        f.rate,
		Mix:         mix,
		Model:       model,
		Prompt:      f.prompt,
	}
	driver, err := loadtest.NewDriver(cfg)
	if err != nil {
		return usagef("loadtest: %v", err)
	}

	// Saturation always observed in-process: read the live server's queue +
	// wait-time accessors directly (no HTTP/admin token needed).
	progress(out, f.asJSON, "running load test (in-process)... press Ctrl-C to stop early")
	results, elapsed, sat := runWithPoller(ctx, driver, stack.StatsSource(), f.statsInterval)

	rc := inProcReportConfig(driver.Config(), f, stack.BaseURL)
	report := loadtest.NewRunReport(rc, loadtest.Summarize(results, elapsed), sat)
	return emitReport(out, report, f.asJSON)
}

// runWithPoller runs the driver and, when src is non-nil, polls it for saturation
// stats over the run in a sibling goroutine. The poller's context is cancelled
// (via defer) the instant the driver returns, so the poller goroutine always
// stops and is awaited before its observation is read — no context leak and no
// race. When src is nil (no admin token / no stats source) it just runs the
// driver and returns a nil observation.
func runWithPoller(ctx context.Context, driver *loadtest.Driver, src loadtest.StatsSource, interval time.Duration) ([]loadtest.Result, time.Duration, *loadtest.SaturationObs) {
	if src == nil {
		results, elapsed := driver.Run(ctx)
		return results, elapsed, nil
	}
	poller := loadtest.NewSaturationPoller(src, interval)
	pctx, pcancel := context.WithCancel(ctx)
	defer pcancel()
	go poller.Run(pctx)

	results, elapsed := driver.Run(ctx)
	pcancel()     // stop the poller now that the run is done
	poller.Wait() // and wait for it before reading the observation
	return results, elapsed, poller.Observation()
}

// emitReport writes the report as JSON or text per asJSON.
func emitReport(out io.Writer, report loadtest.RunReport, asJSON bool) error {
	if asJSON {
		return report.WriteJSON(out)
	}
	fmt.Fprintln(out)
	report.WriteText(out)
	return nil
}

// progress prints a human status line to out, UNLESS asJSON is set — in JSON mode
// out must carry only the machine-readable report so it pipes cleanly into jq, so
// the status line is suppressed.
func progress(out io.Writer, asJSON bool, msg string) {
	if asJSON {
		return
	}
	fmt.Fprintln(out, msg)
}

// loadtestInProcModel returns the model name to use in-process: the user's
// --model if set, else the stack's default echo model name. The empty string
// signals "use the default" to resolveMixAndModel.
func loadtestInProcModel(userModel string) string {
	if userModel != "" {
		return userModel
	}
	return loadtest.DefaultInProcModel
}

// remoteReportConfig projects the driver config into the report header for the
// remote mode.
func remoteReportConfig(c loadtest.Config, mode string) loadtest.ReportConfig {
	rc := loadtest.ReportConfig{
		Mode:        mode,
		BaseURL:     c.BaseURL,
		Concurrency: c.Concurrency,
		Loop:        loopName(c.Rate),
		Rate:        c.Rate,
		Mix:         c.Mix.String(),
		Model:       c.Model,
	}
	if c.Duration > 0 {
		rc.DurationCfg = c.Duration.String()
	}
	if c.Requests > 0 {
		rc.RequestsCfg = c.Requests
	}
	return rc
}

// inProcReportConfig projects the driver config plus the in-process saturation
// knobs into the report header.
func inProcReportConfig(c loadtest.Config, f *loadtestFlags, baseURL string) loadtest.ReportConfig {
	rc := remoteReportConfig(c, "inproc")
	rc.BaseURL = baseURL
	rc.Workers = f.workers
	rc.QueueMaxDepth = f.queueMaxDepth
	rc.GlobalRPM = f.globalRPM
	rc.GlobalTPM = f.globalTPM
	return rc
}

// loopName returns "open" for a positive rate (open-loop arrival schedule) and
// "closed" otherwise (closed-loop workers).
func loopName(rate float64) string {
	if rate > 0 {
		return "open"
	}
	return "closed"
}

// loadtestFlags holds the parsed loadtest flags.
type loadtestFlags struct {
	mode string

	// remote target
	url        string
	token      string
	adminToken string

	// load shape
	concurrency int
	duration    time.Duration
	requests    int
	rate        float64
	endpoint    string
	mix         string
	model       string
	prompt      string

	// in-process knobs
	workers       int
	queueMaxDepth int
	globalRPM     uint64
	globalTPM     uint64
	think         time.Duration

	// observability / output
	statsInterval time.Duration
	asJSON        bool
}

// parseLoadtestFlags parses the loadtest flag set from args, routing help/errors
// to out. It returns the populated loadtestFlags or a usage/help error.
func parseLoadtestFlags(out io.Writer, args []string) (*loadtestFlags, error) {
	fs := flag.NewFlagSet("loadtest", flag.ContinueOnError)
	f := &loadtestFlags{}

	fs.StringVar(&f.mode, "mode", "remote", "load mode: remote (a running deployment) or inproc (in-process stack)")

	fs.StringVar(&f.url, "url", "", "deployment HTTP API base URL (remote mode; or $AGENTGPU_HTTP_ADDR, default http://127.0.0.1:8080)")
	fs.StringVar(&f.token, "token", "", "user Bearer token permitted for --model (remote mode; or $AGENTGPU_TOKEN)")
	fs.StringVar(&f.adminToken, "admin-token", "", "admin Bearer token to poll GET /v1/admin/stats during the run (remote mode, optional)")

	fs.IntVar(&f.concurrency, "concurrency", 16, "number of in-flight requests")
	fs.DurationVar(&f.duration, "duration", 0, "run for this wall-clock duration (e.g. 30s); mutually exclusive with --requests")
	fs.IntVar(&f.requests, "requests", 0, "run a fixed number of requests; mutually exclusive with --duration")
	fs.Float64Var(&f.rate, "rate", 0, "open-loop target arrival rate in req/s (0 = closed-loop)")
	fs.StringVar(&f.endpoint, "endpoint", "chat", "endpoint to exercise: chat|completions|models")
	fs.StringVar(&f.mix, "mix", "", `weighted request mix, e.g. "chat=80,models=20" (overrides --endpoint)`)
	fs.StringVar(&f.model, "model", "", "model name for chat/completions requests")
	fs.StringVar(&f.prompt, "prompt", "", "prompt text for inference requests (default a short fixed prompt)")

	fs.IntVar(&f.workers, "workers", 2, "in-process: number of echo workers to spin up (>=2 exercises routing)")
	fs.IntVar(&f.queueMaxDepth, "queue-max-depth", 0, "in-process: bound the job queue (rejects with 503 only when no worker is runnable; 0 = unbounded)")
	fs.Uint64Var(&f.globalRPM, "global-rpm", 0, "in-process: server-wide requests-per-minute cap; exceeding it returns 429 (0 = unlimited)")
	fs.Uint64Var(&f.globalTPM, "global-tpm", 0, "in-process: server-wide tokens-per-minute cap (0 = unlimited)")
	fs.DurationVar(&f.think, "think", 0, "in-process: artificial per-request backend delay (e.g. 5ms) so client-observed latency climbs under load")

	fs.DurationVar(&f.statsInterval, "stats-interval", time.Second, "interval to poll queue/wait-time stats during the run")
	fs.BoolVar(&f.asJSON, "json", false, "emit the run report as JSON (for baseline comparison)")

	setUsage(fs, loadtestUsage)
	if err := parseFlags(fs, out, args); err != nil {
		return nil, err
	}

	// Default the run bound so a bare `loadtest` invocation does something sensible
	// rather than erroring: a short fixed request budget.
	if f.duration == 0 && f.requests == 0 {
		f.requests = 1000
	}
	return f, nil
}

// resolveMixAndModel turns the --endpoint/--mix flags into a loadtest.Mix and
// resolves the model name, applying defaultModel when --model was not set. It
// errors if both --endpoint and --mix are given, or if the mix spec is invalid.
func (f *loadtestFlags) resolveMixAndModel(defaultModel string) (loadtest.Mix, string, error) {
	model := f.model
	if model == "" {
		model = defaultModel
	}
	if f.mix != "" {
		if f.endpoint != "" && f.endpoint != "chat" {
			// chat is the flag default; treat an explicit non-chat endpoint + a mix
			// as a conflict, but allow the default to be overridden silently by a mix.
			return loadtest.Mix{}, "", usagef("loadtest: pass either --endpoint or --mix, not both")
		}
		m, err := loadtest.ParseMix(f.mix)
		if err != nil {
			return loadtest.Mix{}, "", usagef("loadtest: invalid --mix: %v", err)
		}
		return m, model, nil
	}
	ep := loadtest.Endpoint(f.endpoint)
	if !loadtest.ValidEndpoint(ep) {
		return loadtest.Mix{}, "", usagef("loadtest: invalid --endpoint %q (want chat|completions|models)", f.endpoint)
	}
	return loadtest.SingleEndpointMix(ep), model, nil
}
