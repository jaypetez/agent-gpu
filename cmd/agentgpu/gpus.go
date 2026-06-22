package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
)

// runGPUsCmd implements the `gpus` subcommand (#104): the aggregated, read-only GPU
// capacity inventory over the admin API (GET /v1/admin/gpus) — the fleet roll-up,
// a by-GPU-type grouping, and a per-worker heatmap. It is a live read over the
// heartbeat snapshot (no probing).
//
//	agentgpu gpus
//
// HTTP-only — the fleet exists only on a running server.
func runGPUsCmd(ctx context.Context, out io.Writer, args []string) error {
	if len(args) >= 1 && isHelpArg(args[0]) {
		return groupHelp(out, gpusUsage)
	}
	fs := flag.NewFlagSet("gpus", flag.ContinueOnError)
	cf := registerClientFlags(fs, httpOnlyMode)
	setUsage(fs, "Usage: agentgpu gpus")
	if err := parseFlags(fs, out, args); err != nil {
		return err
	}
	c, err := cf.client()
	if err != nil {
		return err
	}
	inv, err := c.FleetCapacity(ctx)
	if err != nil {
		return err
	}

	f := inv.Fleet
	fmt.Fprintf(out, "Fleet: %d worker(s)  Free/Total VRAM: %s / %s  Load mean/max: %d/%d\n",
		f.WorkerCount, fmtBytes(f.FreeVRAM), fmtBytes(f.TotalVRAM), f.MeanLoad, f.MaxLoad)

	if len(inv.ByType) > 0 {
		fmt.Fprintln(out, "\nBy GPU type:")
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "GPU TYPE\tWORKERS\tFREE VRAM\tTOTAL VRAM")
		for _, t := range inv.ByType {
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", fmtStr(t.GPUType), t.WorkerCount, fmtBytes(t.FreeVRAM), fmtBytes(t.TotalVRAM))
		}
		_ = tw.Flush()
	}

	if len(inv.Workers) > 0 {
		fmt.Fprintln(out, "\nWorkers:")
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tSTATUS\tACTIVE\tLOAD\tGPU\tFREE VRAM\tTOTAL VRAM")
		for _, w := range inv.Workers {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%s\t%s\n",
				w.ID, w.Status, w.ActiveJobs, w.Load, fmtStr(w.GPUType), fmtBytes(w.FreeVRAM), fmtBytes(w.TotalVRAM))
		}
		_ = tw.Flush()
	}
	return nil
}

// gpusUsage is the help text for `agentgpu gpus`.
const gpusUsage = `Usage: agentgpu gpus

Show the aggregated GPU/fleet capacity of a RUNNING server (a server URL via
--server/$AGENTGPU_HTTP_ADDR and an admin token via --token/$AGENTGPU_TOKEN): the
fleet roll-up, a by-GPU-type grouping, and a per-worker heatmap. It is a live read
over the heartbeat snapshot (no GPU probing).`
