package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Exit codes, kept small and distinct so a script wrapping this example can
// branch on the outcome.
const (
	exitOK        = 0   // success
	exitConfig    = 1   // bad/missing configuration (e.g. no API key)
	exitModel     = 2   // requested model is not available to this key
	exitRequest   = 3   // the API call failed (auth, quota, server error, …)
	exitInterrupt = 130 // interrupted (Ctrl-C); 128 + SIGINT, the shell convention
)

// chatTimeout bounds a single NON-streaming chat request. A generation can run
// for a while, so this is generous; it is applied via a context deadline rather
// than an http.Client.Timeout so it does NOT also cap the streaming request,
// where a long generation must be allowed to stream to completion.
const chatTimeout = 120 * time.Second

// config is the resolved runtime configuration: environment variables provide
// the defaults and flags override them (flag > env > built-in default).
type config struct {
	baseURL string
	apiKey  string
	model   string
	prompt  string
	stream  bool
}

func main() {
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		// -help / -h is a successful, intentional invocation: flag printed the
		// usage and returns flag.ErrHelp, so exit 0. Any other parse error is a
		// usage mistake (flag already printed it) and exits with the config code.
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(exitOK)
		}
		os.Exit(exitConfig)
	}
	os.Exit(run(cfg))
}

// parseConfig resolves configuration from environment defaults overridden by
// flags. The API key is required; everything else has a sensible default.
func parseConfig(args []string) (config, error) {
	fs := flag.NewFlagSet("go-client", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usage)
		fs.PrintDefaults()
	}

	cfg := config{}
	fs.StringVar(&cfg.baseURL, "base-url", envOr("AGENTGPU_BASE_URL", "http://localhost:8080/v1"),
		"agent-gpu OpenAI API base URL (the .../v1 prefix); env AGENTGPU_BASE_URL")
	fs.StringVar(&cfg.apiKey, "api-key", os.Getenv("AGENTGPU_API_KEY"),
		"API key token (agpu_<keyid>_<secret>); env AGENTGPU_API_KEY (REQUIRED)")
	fs.StringVar(&cfg.model, "model", envOr("AGENTGPU_MODEL", "qwen2:0.5b"),
		"model to use; env AGENTGPU_MODEL")
	fs.StringVar(&cfg.prompt, "prompt", "Say hello in one short sentence.",
		"user prompt to send")
	fs.BoolVar(&cfg.stream, "stream", false,
		"stream the response token-by-token (SSE) instead of waiting for the full reply")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// run executes the example flow and returns a process exit code. Errors are
// printed to stderr so stdout carries only the model's output.
func run(cfg config) int {
	if strings.TrimSpace(cfg.apiKey) == "" {
		fmt.Fprintln(os.Stderr, "error: an API key is required; set AGENTGPU_API_KEY or pass -api-key")
		fmt.Fprintln(os.Stderr, "       tokens look like agpu_<keyid>_<secret>; mint one with `agentgpu key create --role user`.")
		return exitConfig
	}

	// No overall http.Client.Timeout: a short one would cut off a long streaming
	// generation. Non-streaming calls are bounded by a context deadline instead.
	client := NewClient(cfg.baseURL, cfg.apiKey, &http.Client{})

	// Cancel in-flight work on Ctrl-C so a streaming generation can be stopped
	// cleanly without a deadline that would otherwise truncate a slow reply.
	ctx, stop := signalContext(context.Background())
	defer stop()

	// 1. Discover the catalog and confirm the target model is available. This
	//    avoids a confusing failure and shows users how to find a usable model.
	if code := ensureModelAvailable(ctx, client, cfg.model); code != exitOK {
		return code
	}

	// 2. Run the chat request.
	messages := []Message{{Role: "user", Content: cfg.prompt}}
	if cfg.stream {
		return runStream(ctx, client, cfg.model, messages)
	}
	return runChat(ctx, client, cfg.model, messages)
}

// ensureModelAvailable lists the key's catalog and verifies model is in it,
// printing a helpful hint (and the available models) when it is not.
func ensureModelAvailable(ctx context.Context, client *Client, model string) int {
	listCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	models, err := client.ListModels(listCtx)
	if err != nil {
		printAPIError(err)
		return exitRequest
	}
	available := make([]string, 0, len(models))
	for _, m := range models {
		if m.ID == model {
			return exitOK
		}
		available = append(available, m.ID)
	}

	fmt.Fprintf(os.Stderr, "error: model %q is not available to this key\n", model)
	if len(available) == 0 {
		fmt.Fprintln(os.Stderr, "       no models are currently available: ensure a worker is online and has pulled a model")
		fmt.Fprintln(os.Stderr, "       (e.g. `ollama pull qwen2:0.5b`), and that this key is permitted to use it.")
	} else {
		fmt.Fprintf(os.Stderr, "       available models: %s\n", strings.Join(available, ", "))
		fmt.Fprintf(os.Stderr, "       pick one with -model, or pull %q on a worker.\n", model)
	}
	return exitModel
}

// runChat performs a buffered (non-streaming) chat completion and prints the
// assistant reply followed by a usage summary.
func runChat(ctx context.Context, client *Client, model string, messages []Message) int {
	reqCtx, cancel := context.WithTimeout(ctx, chatTimeout)
	defer cancel()

	resp, err := client.Chat(reqCtx, ChatRequest{Model: model, Messages: messages})
	if err != nil {
		printAPIError(err)
		return exitRequest
	}
	if len(resp.Choices) == 0 {
		fmt.Fprintln(os.Stderr, "error: the server returned no choices")
		return exitRequest
	}
	fmt.Println(resp.Choices[0].Message.Content)
	fmt.Fprintf(os.Stderr, "\n[usage] prompt=%d completion=%d total=%d\n",
		resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
	return exitOK
}

// runStream performs a streaming chat completion, printing content deltas to
// stdout as they arrive (no newline between them, flushing implicitly via the
// unbuffered os.Stdout) and a trailing newline at the end. A mid-stream error
// frame is surfaced as a failure.
func runStream(ctx context.Context, client *Client, model string, messages []Message) int {
	wrote := false
	err := client.ChatStream(ctx, ChatRequest{Model: model, Messages: messages}, func(delta string) {
		fmt.Print(delta)
		wrote = true
	})
	if wrote {
		fmt.Println() // terminate the streamed line
	}
	if err != nil {
		// Distinguish a clean cancel (Ctrl-C) from a real failure.
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "interrupted")
			return exitInterruptCode(ctx)
		}
		printAPIError(err)
		return exitRequest
	}
	return exitOK
}

// printAPIError prints a server error in the agent-gpu contract form, adding the
// Retry-After hint when the server supplied one (on a 429, sometimes a 503).
// Non-API errors (e.g. a dial failure) are printed verbatim.
func printAPIError(err error) {
	if apiErr, ok := asAPIError(err); ok {
		fmt.Fprintf(os.Stderr, "error: %s\n", apiErr.Error())
		if apiErr.RetryAfter > 0 {
			fmt.Fprintf(os.Stderr, "       retry after %d seconds\n", apiErr.RetryAfter)
		}
		return
	}
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
}

// exitInterruptCode returns the conventional 130 for an interrupted run, or the
// request exit code if the context was cancelled for another reason.
func exitInterruptCode(ctx context.Context) int {
	if ctx.Err() == context.Canceled {
		return exitInterrupt
	}
	return exitRequest
}

// envOr returns the value of env var key, or def when it is unset or empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

const usage = `go-client — an example agent-gpu OpenAI-compatible API client.

It lists the models your key may use, then runs a chat completion (buffered, or
streamed with -stream) against the agent-gpu server.

Usage:
  go-client [flags]

Examples:
  AGENTGPU_API_KEY=agpu_… go run . -prompt "Explain goroutines in one sentence."
  AGENTGPU_API_KEY=agpu_… go run . -stream -prompt "Write a haiku about GPUs."

Flags:
`
