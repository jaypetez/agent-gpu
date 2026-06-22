package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
)

// runTelemetryCmd implements the `telemetry` subcommand (#104): the dashboard
// telemetry summary over the admin API (GET /v1/admin/telemetry) — request
// rate/latency, throttle counts, fleet health, live sessions, and uptime — in one
// read over the server's in-process collectors.
//
//	agentgpu telemetry
//
// HTTP-only — telemetry is computed on a running server.
func runTelemetryCmd(ctx context.Context, out io.Writer, args []string) error {
	if len(args) >= 1 && isHelpArg(args[0]) {
		return groupHelp(out, telemetryUsage)
	}
	fs := flag.NewFlagSet("telemetry", flag.ContinueOnError)
	cf := registerClientFlags(fs, httpOnlyMode)
	setUsage(fs, "Usage: agentgpu telemetry")
	if err := parseFlags(fs, out, args); err != nil {
		return err
	}
	c, err := cf.client()
	if err != nil {
		return err
	}
	tel, err := c.Telemetry(ctx)
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "Uptime\t%s\n", fmtUptime(tel.UptimeSeconds))
	fmt.Fprintf(tw, "Requests\t%d\n", tel.Requests.Count)
	fmt.Fprintf(tw, "Latency mean/max\t%dms / %dms\n", tel.Requests.Latency.MeanMs, tel.Requests.Latency.MaxMs)
	fmt.Fprintf(tw, "Throttled global/key\t%d / %d\n", tel.Throttles.Global, tel.Throttles.Key)
	fmt.Fprintf(tw, "Workers\t%d\n", tel.Fleet.WorkerCount)
	fmt.Fprintf(tw, "Queue depth\t%d\n", tel.Fleet.Queue.Total)
	fmt.Fprintf(tw, "Queue wait mean/max\t%dms / %dms\n", tel.Fleet.WaitTime.MeanMs, tel.Fleet.WaitTime.MaxMs)
	fmt.Fprintf(tw, "Sessions active\t%d\n", tel.Sessions.Active)
	fmt.Fprintf(tw, "Affinity hits/misses/rebinds\t%d / %d / %d\n",
		tel.Affinity.Hits, tel.Affinity.Misses, tel.Affinity.Rebinds)
	if err := tw.Flush(); err != nil {
		return err
	}

	if len(tel.Fleet.ByStatus) > 0 {
		fmt.Fprintln(out, "\nWorkers by status:")
		st := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		statuses := make([]string, 0, len(tel.Fleet.ByStatus))
		for s := range tel.Fleet.ByStatus {
			statuses = append(statuses, s)
		}
		sort.Strings(statuses)
		for _, s := range statuses {
			fmt.Fprintf(st, "%s\t%d\n", s, tel.Fleet.ByStatus[s])
		}
		_ = st.Flush()
	}
	return nil
}

// telemetryUsage is the help text for `agentgpu telemetry`.
const telemetryUsage = `Usage: agentgpu telemetry

Show the dashboard telemetry summary of a RUNNING server (a server URL via
--server/$AGENTGPU_HTTP_ADDR and an admin token via --token/$AGENTGPU_TOKEN):
request rate and latency, throttle counts, fleet health, live session count, and
process uptime — in one read.`
