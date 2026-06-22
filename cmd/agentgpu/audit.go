package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/jaypetez/agent-gpu/internal/apiclient"
)

// runAuditCmd implements the `audit` subcommand (#104): a filtered, cursor-followed
// query over the admin audit log (#90, GET /v1/admin/audit).
//
//	agentgpu audit [--actor id] [--op name] [--target id] [--since when] [--until when]
//
// It is HTTP-only — the audit log lives on a running server. The filter fields are
// ANDed server-side; --since/--until accept either an RFC3339 timestamp or a
// relative duration ago (e.g. "24h"). The entries never carry secret material.
func runAuditCmd(ctx context.Context, out io.Writer, args []string) error {
	if len(args) >= 1 && isHelpArg(args[0]) {
		return groupHelp(out, auditUsage)
	}
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	cf := registerClientFlags(fs, httpOnlyMode)
	actor := fs.String("actor", "", "filter to entries by this actor key id")
	op := fs.String("op", "", "filter to this operation name (e.g. key.create)")
	target := fs.String("target", "", "filter to entries acting on this resource id")
	since := fs.String("since", "", "only entries at/after this time (RFC3339 or a duration ago like 24h)")
	until := fs.String("until", "", "only entries before this time (RFC3339 or a duration ago like 1h)")
	setUsage(fs, "Usage: agentgpu audit [--actor id] [--op name] [--target id] [--since when] [--until when]")
	if err := parseFlags(fs, out, args); err != nil {
		return err
	}

	filter := apiclient.AuditFilter{Actor: *actor, Op: *op, Target: *target}
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
	entries, err := c.ListAudit(ctx, filter)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintln(out, "No audit entries.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tACTOR\tOP\tTARGET\tOUTCOME\tREQUEST")
	for _, e := range entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			fmtTime(e.Time), fmtStr(e.Actor), fmtStr(e.Op), fmtStr(e.Target), fmtStr(e.Outcome), fmtStr(e.RequestID))
	}
	return tw.Flush()
}

// auditUsage is the help text for `agentgpu audit`.
const auditUsage = `Usage: agentgpu audit [filters]

Query the admin audit log of a RUNNING server (a server URL via
--server/$AGENTGPU_HTTP_ADDR and an admin token via --token/$AGENTGPU_TOKEN),
newest first. Filters are ANDed; --since/--until take an RFC3339 timestamp or a
relative duration ago (e.g. 24h).

Filters:
  --actor id    entries by this actor key id
  --op name     this operation name (e.g. key.create, config.update)
  --target id   entries acting on this resource id
  --since when  only entries at/after this time
  --until when  only entries before this time

  agentgpu audit --op key.create --since 24h`

// parseWhen interprets a --since/--until value as either an RFC3339 timestamp or a
// relative Go duration ago (e.g. "24h" → now-24h). An empty value yields the zero
// time (the bound is omitted). A malformed value is a usage error naming the flag.
func parseWhen(flagName, v string, now time.Time) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		return now.Add(-d), nil
	}
	return time.Time{}, usagef("--%s: %q is not an RFC3339 time or a duration (e.g. 24h)", flagName, v)
}
