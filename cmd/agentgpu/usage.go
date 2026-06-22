package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/jaypetez/agent-gpu/internal/apiclient"
)

// runUsageCmd implements the `usage` subcommand (#104): the per-key usage report
// over the admin usage API (#94, GET /v1/admin/usage) — current usage versus
// effective limits per key, plus the fleet-wide throttle summary.
//
//	agentgpu usage [--key id] [--owner label] [--team label]
//
// It is HTTP-only — usage is computed on a running server. The filters are ANDed
// server-side. The summary comes from GetUsage; the per-key rows are
// cursor-followed via ListUsage so the full matching set is shown. No secrets.
func runUsageCmd(ctx context.Context, out io.Writer, args []string) error {
	if len(args) >= 1 && isHelpArg(args[0]) {
		return groupHelp(out, usageCmdUsage)
	}
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	cf := registerClientFlags(fs, httpOnlyMode)
	keyID := fs.String("key", "", "filter to this key id")
	owner := fs.String("owner", "", "filter to keys with this owner label")
	team := fs.String("team", "", "filter to keys with this team label")
	setUsage(fs, "Usage: agentgpu usage [--key id] [--owner label] [--team label]")
	if err := parseFlags(fs, out, args); err != nil {
		return err
	}
	filter := apiclient.UsageFilter{KeyID: *keyID, Owner: *owner, Team: *team}

	c, err := cf.client()
	if err != nil {
		return err
	}
	// The summary (fleet-wide throttle counts + matched-key count) is identical on
	// every page, so take it from a single GetUsage; the rows are cursor-followed via
	// ListUsage so a fleet larger than one page is fully shown.
	report, err := c.GetUsage(ctx, filter)
	if err != nil {
		return err
	}
	rows, err := c.ListUsage(ctx, filter)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Keys: %d  Throttled (global): %d  Throttled (per-key): %d\n",
		report.Summary.KeyCount, report.Summary.GlobalThrottled, report.Summary.KeyThrottled)
	if len(rows) == 0 {
		fmt.Fprintln(out, "No matching keys.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tNAME\tOWNER\tTEAM\tREQ/MIN\tTOK/MIN\tTOK/DAY\tTOK/MONTH\tRPM LIMIT\tDAILY LIMIT")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%s\t%s\n",
			r.KeyID, fmtStr(r.Name), fmtStr(r.Owner), fmtStr(r.Team),
			r.RequestsThisMinute, r.TokensThisMinute, r.TokensToday, r.TokensThisMonth,
			fmtLimit(r.Limits.RPM), fmtLimit(r.Limits.DailyTokens))
	}
	return tw.Flush()
}

// usageCmdUsage is the help text for `agentgpu usage`.
const usageCmdUsage = `Usage: agentgpu usage [--key id] [--owner label] [--team label]

Show the per-key usage report of a RUNNING server (a server URL via
--server/$AGENTGPU_HTTP_ADDR and an admin token via --token/$AGENTGPU_TOKEN):
current usage versus each key's effective limits, with a fleet-wide throttle
summary. Filters are ANDed.

  --key id        only this key id
  --owner label   only keys with this owner label
  --team label    only keys with this team label

  agentgpu usage --team platform`
