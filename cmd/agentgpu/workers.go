package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"
)

// runWorkersCmd routes the `workers` subcommand (#104): a thin client over the
// per-worker control admin API (#93).
//
//	agentgpu workers list                     snapshot the fleet
//	agentgpu workers detail <id>              one worker's full detail
//	agentgpu workers drain <id> [--deadline d]   stop new jobs (optional forced deadline)
//	agentgpu workers pull <id> <model>        pull a model onto the worker
//	agentgpu workers unload <id> <model>      unload a model from the worker
//
// It is HTTP-only — the fleet exists only on a running server — so there is no
// --local mode. All actions go through /v1/admin/workers/*; the CLI adds no logic
// of its own beyond formatting.
func runWorkersCmd(ctx context.Context, out io.Writer, args []string) error {
	if len(args) < 1 {
		return usagef("usage: agentgpu workers <list|detail|drain|pull|unload> [args]")
	}
	if isHelpArg(args[0]) {
		return groupHelp(out, workersUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return runWorkersList(ctx, out, rest)
	case "detail":
		return runWorkersDetail(ctx, out, rest)
	case "drain":
		return runWorkersDrain(ctx, out, rest)
	case "pull":
		return runWorkersPull(ctx, out, rest)
	case "unload":
		return runWorkersUnload(ctx, out, rest)
	default:
		return usagef("unknown workers subcommand %q", sub)
	}
}

// workersUsage is the help text for `agentgpu workers`.
const workersUsage = `Usage: agentgpu workers <command> [args]

Inspect and control the worker fleet of a RUNNING server (a server URL via
--server/$AGENTGPU_HTTP_ADDR and an admin token via --token/$AGENTGPU_TOKEN).

Commands:
  list                     snapshot the fleet (id, status, models, VRAM, load)
  detail <id>              one worker's full detail (uptime, draining, capacity)
  drain  <id>              stop sending the worker new jobs; in-flight jobs finish
  pull   <id> <model>      ask the worker to pull a model onto its Ollama
  unload <id> <model>      ask the worker to unload a model, freeing VRAM

Run 'agentgpu workers <command> --help' for that command's flags.`

func runWorkersList(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("workers list", flag.ContinueOnError)
	cf := registerClientFlags(fs, httpOnlyMode)
	setUsage(fs, "Usage: agentgpu workers list")
	if err := parseFlags(fs, out, args); err != nil {
		return err
	}
	c, err := cf.client()
	if err != nil {
		return err
	}
	workers, err := c.ListWorkers(ctx)
	if err != nil {
		return err
	}
	if len(workers) == 0 {
		fmt.Fprintln(out, "No workers connected.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tACTIVE\tLOAD\tGPU\tFREE VRAM\tTOTAL VRAM\tLAST SEEN\tMODELS")
	for _, w := range workers {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\n",
			w.ID, w.Status, w.ActiveJobs, w.Load, fmtStr(w.GPUType),
			fmtBytes(w.FreeVRAM), fmtBytes(w.TotalVRAM), fmtUnix(w.LastSeen), fmtModels(w.Models))
	}
	return tw.Flush()
}

func runWorkersDetail(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("workers detail", flag.ContinueOnError)
	cf := registerClientFlags(fs, httpOnlyMode)
	setUsage(fs, "Usage: agentgpu workers detail <id>")
	if err := parseFlags(fs, out, reorderFlagsFirst(args, clientValueFlags())); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("usage: agentgpu workers detail <id>")
	}
	id := fs.Arg(0)

	c, err := cf.client()
	if err != nil {
		return err
	}
	d, err := c.WorkerDetail(ctx, id)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "ID\t%s\n", d.ID)
	fmt.Fprintf(tw, "Status\t%s\n", d.Status)
	fmt.Fprintf(tw, "Draining\t%t\n", d.Draining)
	fmt.Fprintf(tw, "Active jobs\t%d\n", d.ActiveJobs)
	fmt.Fprintf(tw, "Load\t%d\n", d.Load)
	fmt.Fprintf(tw, "GPU type\t%s\n", fmtStr(d.GPUType))
	fmt.Fprintf(tw, "Free VRAM\t%s\n", fmtBytes(d.FreeVRAM))
	fmt.Fprintf(tw, "Total VRAM\t%s\n", fmtBytes(d.TotalVRAM))
	fmt.Fprintf(tw, "Registered\t%s\n", fmtUnix(d.RegisteredAt))
	fmt.Fprintf(tw, "Uptime\t%s\n", fmtUptime(d.UptimeSeconds))
	fmt.Fprintf(tw, "Last seen\t%s\n", fmtUnix(d.LastSeen))
	fmt.Fprintf(tw, "Models\t%s\n", fmtModels(d.Models))
	return tw.Flush()
}

func runWorkersDrain(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("workers drain", flag.ContinueOnError)
	cf := registerClientFlags(fs, httpOnlyMode)
	deadline := fs.Duration("deadline", 0, "forced-drain deadline (e.g. 30s); 0 is a pure soft drain")
	setUsage(fs, "Usage: agentgpu workers drain <id> [--deadline d]")
	valueFlags := clientValueFlags()
	valueFlags["deadline"] = true
	if err := parseFlags(fs, out, reorderFlagsFirst(args, valueFlags)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("usage: agentgpu workers drain <id> [--deadline d]")
	}
	id := fs.Arg(0)

	c, err := cf.client()
	if err != nil {
		return err
	}
	if err := c.DrainWorker(ctx, id, *deadline); err != nil {
		return err
	}
	if *deadline > 0 {
		fmt.Fprintf(out, "Draining worker %s (forced after %s); in-flight jobs finish first.\n", id, *deadline)
	} else {
		fmt.Fprintf(out, "Draining worker %s; it stops receiving new jobs while in-flight jobs finish.\n", id)
	}
	return nil
}

func runWorkersPull(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("workers pull", flag.ContinueOnError)
	cf := registerClientFlags(fs, httpOnlyMode)
	setUsage(fs, "Usage: agentgpu workers pull <id> <model>")
	if err := parseFlags(fs, out, reorderFlagsFirst(args, clientValueFlags())); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return usagef("usage: agentgpu workers pull <id> <model>")
	}
	id, model := fs.Arg(0), fs.Arg(1)

	c, err := cf.client()
	if err != nil {
		return err
	}
	if err := c.PullModel(ctx, id, model); err != nil {
		return err
	}
	fmt.Fprintf(out, "Requested pull of %q on worker %s; it appears once the pull completes.\n", model, id)
	return nil
}

func runWorkersUnload(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("workers unload", flag.ContinueOnError)
	cf := registerClientFlags(fs, httpOnlyMode)
	setUsage(fs, "Usage: agentgpu workers unload <id> <model>")
	if err := parseFlags(fs, out, reorderFlagsFirst(args, clientValueFlags())); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return usagef("usage: agentgpu workers unload <id> <model>")
	}
	id, model := fs.Arg(0), fs.Arg(1)

	c, err := cf.client()
	if err != nil {
		return err
	}
	if err := c.UnloadModel(ctx, id, model); err != nil {
		return err
	}
	fmt.Fprintf(out, "Requested unload of %q from worker %s.\n", model, id)
	return nil
}

// fmtModels renders a worker's model list for the table, sorted for stable output
// and showing a dash when the worker serves none.
func fmtModels(models []string) string {
	if len(models) == 0 {
		return "-"
	}
	sorted := append([]string(nil), models...)
	sort.Strings(sorted)
	return fmtList(sorted)
}

// fmtBytes renders a byte count as a human-readable binary size (e.g. "40.0 GiB"),
// showing a dash for zero (an unreported/CPU-only worker). It keeps the operator
// tables readable versus raw byte integers.
func fmtBytes(b uint64) string {
	if b == 0 {
		return "-"
	}
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// fmtUptime renders an uptime in seconds as a coarse human duration, showing a dash
// when unknown (zero). It rounds to the second so the value is stable across calls.
func fmtUptime(secs int64) string {
	if secs <= 0 {
		return "-"
	}
	return (time.Duration(secs) * time.Second).String()
}
