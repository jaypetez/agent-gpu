package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

// runModelsCmd routes the `models` subcommand:
//
//	agentgpu models list [--json|--openai]
//
// The model catalog only exists on a running server (it is the per-key,
// Online-only, permission-filtered view of the fleet), so this command is
// HTTP-only: it requires a server URL and an admin token. By default it renders
// an operator table (NAME, DIGEST, WORKERS) from the richer GET /models endpoint;
// --json emits that endpoint's raw JSON and --openai emits the OpenAI-canonical
// GET /v1/models JSON instead.
func runModelsCmd(ctx context.Context, out io.Writer, args []string) error {
	if len(args) < 1 {
		return usagef("usage: agentgpu models list [--json|--openai]")
	}
	if isHelpArg(args[0]) {
		return groupHelp(out, modelsUsage)
	}
	switch args[0] {
	case "list":
		return runModelsList(ctx, out, args[1:])
	default:
		return usagef("unknown models subcommand %q", args[0])
	}
}

// modelsUsage is the help text for `agentgpu models`.
const modelsUsage = `Usage: agentgpu models list [--json|--openai]

List the model catalog the server currently serves (the per-key, Online-only,
permission-filtered view of the fleet). This command requires a running server: a
server URL (--server/$AGENTGPU_HTTP_ADDR) and an admin token (--token/$AGENTGPU_TOKEN).

  list            render an operator table (NAME, DIGEST, WORKERS)
  list --json     emit the raw /models JSON
  list --openai   emit the OpenAI-canonical /v1/models JSON`

func runModelsList(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("models list", flag.ContinueOnError)
	cf := registerClientFlags(fs, httpOnlyMode)
	asJSON := fs.Bool("json", false, "emit the raw /models JSON (digest + per-model worker availability)")
	asOpenAI := fs.Bool("openai", false, "emit the OpenAI-canonical /v1/models JSON")
	setUsage(fs, "Usage: agentgpu models list [--json|--openai]")
	if err := parseFlags(fs, out, args); err != nil {
		return err
	}
	if *asJSON && *asOpenAI {
		return usagef("models list: --json and --openai are mutually exclusive")
	}

	c, err := cf.client()
	if err != nil {
		return err
	}

	// Raw passthrough modes: fetch the endpoint's JSON and re-emit it indented so
	// it pipes cleanly into jq while staying readable on a terminal.
	if *asOpenAI {
		return passthroughJSON(ctx, out, c, "/v1/models")
	}
	if *asJSON {
		return passthroughJSON(ctx, out, c, "/models")
	}

	models, err := c.ListModels(ctx)
	if err != nil {
		return err
	}
	if len(models) == 0 {
		fmt.Fprintln(out, "No models available.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tDIGEST\tWORKERS")
	for _, m := range models {
		workers := append([]string(nil), m.Workers...)
		sort.Strings(workers)
		fmt.Fprintf(tw, "%s\t%s\t%s\n", m.Name, fmtDigest(m.Digest), fmtWorkers(m.WorkerCount, workers))
	}
	return tw.Flush()
}

// passthroughJSON fetches path's JSON via the client and writes it back indented,
// preserving the server's shape verbatim (used by --json/--openai). Decoding to
// any then re-encoding normalizes whitespace without depending on the field set.
func passthroughJSON(ctx context.Context, out io.Writer, c interface {
	Get(ctx context.Context, path string, out any) error
}, path string) error {
	var raw any
	if err := c.Get(ctx, path, &raw); err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(raw)
}

// fmtDigest renders a model digest for the table, showing a dash when empty and
// trimming a long content-addressed digest to a readable prefix.
func fmtDigest(digest string) string {
	if digest == "" {
		return "-"
	}
	// Strip an algorithm prefix (e.g. "sha256:") then keep a short, recognizable
	// prefix; operators correlate by the leading hex, not the full hash.
	d := digest
	if i := strings.IndexByte(d, ':'); i >= 0 && i+1 < len(d) {
		d = d[i+1:]
	}
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// fmtWorkers renders the per-model worker availability: the count plus the ids in
// parentheses, or a dash when no Online worker currently serves the model.
func fmtWorkers(count int, ids []string) string {
	if count == 0 {
		return "-"
	}
	if len(ids) == 0 {
		return fmt.Sprintf("%d", count)
	}
	return fmt.Sprintf("%d (%s)", count, strings.Join(ids, ","))
}
