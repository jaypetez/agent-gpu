package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jaypetez/agent-gpu/internal/apiclient"
)

// runLogsCmd implements the `logs` subcommand (#104): a filtered, cursor-followed
// query over the admin log buffer (#99, GET /v1/admin/logs), newest first. The
// server default returns warnings and errors only unless --level widens it.
//
//	agentgpu logs [--level lvl] [--request-id id] [--session-id id] [--worker id] [--since when] [--until when]
//
// HTTP-only — the log buffer lives on a running server. The entries are already
// redacted by the server at capture, so they never carry secret material.
func runLogsCmd(ctx context.Context, out io.Writer, args []string) error {
	if len(args) >= 1 && isHelpArg(args[0]) {
		return groupHelp(out, logsUsage)
	}
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	cf := registerClientFlags(fs, httpOnlyMode)
	level := fs.String("level", "", "minimum level to return (debug|info|warn|error); default warn")
	requestID := fs.String("request-id", "", "filter to lines with this request_id")
	sessionID := fs.String("session-id", "", "filter to lines with this session_id")
	worker := fs.String("worker", "", "filter to lines with this worker id")
	since := fs.String("since", "", "only lines at/after this time (RFC3339 or a duration ago like 1h)")
	until := fs.String("until", "", "only lines before this time (RFC3339 or a duration ago like 5m)")
	setUsage(fs, "Usage: agentgpu logs [--level lvl] [--request-id id] [--session-id id] [--worker id] [--since when] [--until when]")
	if err := parseFlags(fs, out, args); err != nil {
		return err
	}

	filter := apiclient.LogFilter{Level: *level, RequestID: *requestID, SessionID: *sessionID, Worker: *worker}
	now := time.Now()
	var err error
	if filter.Since, err = parseWhen("since", *since, now); err != nil {
		return err
	}
	if filter.Until, err = parseWhen("until", *until, now); err != nil {
		return err
	}

	c, err := cf.client()
	if err != nil {
		return err
	}
	entries, err := c.Logs(ctx, filter)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintln(out, "No log entries.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tLEVEL\tMESSAGE\tATTRS")
	for _, e := range entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", fmtTime(e.Time), fmtStr(e.Level), fmtStr(e.Message), fmtAttrs(e.Attrs))
	}
	return tw.Flush()
}

// logsUsage is the help text for `agentgpu logs`.
const logsUsage = `Usage: agentgpu logs [filters]

Query the structured log buffer of a RUNNING server (a server URL via
--server/$AGENTGPU_HTTP_ADDR and an admin token via --token/$AGENTGPU_TOKEN),
newest first. Without --level the server returns warnings and errors only.
--since/--until take an RFC3339 timestamp or a relative duration ago (e.g. 1h).

Filters:
  --level lvl        minimum level (debug|info|warn|error)
  --request-id id    only lines with this request_id
  --session-id id    only lines with this session_id
  --worker id        only lines with this worker id
  --since when       only lines at/after this time
  --until when       only lines before this time

  agentgpu logs --level error --since 1h`

// fmtAttrs renders a log entry's structured attributes as a compact, sorted
// key=value list, or a dash when there are none. The attributes are already
// redacted by the server at capture, so this never exposes secrets.
func fmtAttrs(m map[string]any) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, m[k]))
	}
	return strings.Join(parts, " ")
}
