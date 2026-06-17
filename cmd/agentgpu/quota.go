package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/jaypetez/agent-gpu/internal/apiclient"
	"github.com/jaypetez/agent-gpu/internal/config"
	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/store"
)

// runQuotaCmd routes the `quota` subcommand (also reachable as `key quota`):
//
//	agentgpu quota show <id>                 show usage vs limits
//	agentgpu quota <id>                      (shorthand for `quota show <id>`)
//	agentgpu quota set <id> [--rpm N] [--tpm N] [--daily-tokens N] [--monthly-tokens N] [--clear]
//
// By default it acts against a RUNNING server: `set` performs an immediate,
// enforced update via the admin API and `show` reads the server's live usage. The
// --local flag uses the on-disk store/counter checkpoint instead (offline
// inspection and bootstrap); a --local `set` requires a server restart to take
// effect. Secrets are never printed.
func runQuotaCmd(ctx context.Context, out io.Writer, args []string) error {
	if len(args) >= 1 && isHelpArg(args[0]) {
		return groupHelp(out, quotaUsage)
	}
	if len(args) >= 1 && args[0] == "set" {
		return runQuotaSet(ctx, out, args[1:])
	}
	// Accept both `quota show <id>` and the historical `quota <id>` shorthand.
	if len(args) >= 1 && args[0] == "show" {
		return runQuotaShow(ctx, out, args[1:])
	}
	return runQuotaShow(ctx, out, args)
}

// quotaUsage is the help text for `agentgpu quota` (and `key quota`).
const quotaUsage = `Usage: agentgpu quota <set|show> <id> [flags]

Inspect and set per-key quotas. By default this acts against a RUNNING server (an
immediate, enforced update); --local uses the on-disk store/counter checkpoint.

Commands:
  show <id>   show current usage versus the key's effective limits
  set  <id>   set or clear the key's per-key quota override

Run 'agentgpu quota set --help' or 'agentgpu quota show --help' for flags.`

func runQuotaShow(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("quota show", flag.ContinueOnError)
	cf := registerClientFlags(fs, withLocal)
	quotaFlag := fs.String("quota-path", "", "path to the quota counter checkpoint for --local (or $AGENTGPU_QUOTA_PATH, default ~/.agentgpu/quota.json)")
	setUsage(fs, "Usage: agentgpu quota show <id> [--local [--quota-path path]]")
	valueFlags := clientValueFlags()
	valueFlags["quota-path"] = true
	if err := parseFlags(fs, out, reorderFlagsFirst(args, valueFlags)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("usage: agentgpu quota show <id>")
	}
	id := fs.Arg(0)

	if cf.isLocal() {
		return runQuotaShowLocal(ctx, out, cf, id, *quotaFlag)
	}
	return runQuotaShowHTTP(ctx, out, cf, id)
}

func runQuotaShowHTTP(ctx context.Context, out io.Writer, cf *clientFlags, id string) error {
	c, err := cf.client()
	if err != nil {
		return err
	}
	u, err := c.GetQuota(ctx, id)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Quota for key %s\n", u.KeyID)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DIMENSION\tUSAGE\tLIMIT\tRESETS AT")
	fmt.Fprintf(tw, "requests/min\t%d\t%s\t%s\n", u.RequestsThisMinute, fmtLimit(u.Limits.RPM), fmtUnix(u.MinuteResetsAt))
	fmt.Fprintf(tw, "tokens/min\t%d\t%s\t%s\n", u.TokensThisMinute, fmtLimit(u.Limits.TPM), fmtUnix(u.MinuteResetsAt))
	fmt.Fprintf(tw, "tokens/day\t%d\t%s\t%s\n", u.TokensToday, fmtLimit(u.Limits.DailyTokens), fmtUnix(u.DayResetsAt))
	fmt.Fprintf(tw, "tokens/month\t%d\t%s\t%s\n", u.TokensThisMonth, fmtLimit(u.Limits.MonthlyTokens), fmtUnix(u.MonthResetsAt))
	return tw.Flush()
}

func runQuotaShowLocal(ctx context.Context, out io.Writer, cf *clientFlags, id, quotaFlag string) error {
	svc, st, err := cf.localService()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	keys, err := svc.List(ctx)
	if err != nil {
		return err
	}
	var key store.APIKey
	found := false
	for _, k := range keys {
		if k.ID == id {
			key, found = k, true
			break
		}
	}
	if !found {
		return fmt.Errorf("quota show: %w", store.ErrNotFound)
	}

	// Load the persisted counters so usage reflects what the server recorded.
	cs := quota.NewMemoryCounterStore()
	quotaPath := config.ResolveQuotaPath(quotaFlag, nil, nil)
	if err := cs.LoadCheckpoint(quotaPath); err != nil {
		return err
	}
	eng := quota.NewEngine(cs)
	snap, err := eng.UsageForKey(ctx, key)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Quota for key %s (%s)\n", key.ID, key.Name)
	if key.Limits == nil {
		fmt.Fprintln(out, "Limits: global defaults (no per-key override)")
	} else {
		fmt.Fprintln(out, "Limits: per-key override")
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DIMENSION\tUSAGE\tLIMIT\tRESETS AT")
	fmt.Fprintf(tw, "requests/min\t%d\t%s\t%s\n", snap.RequestsThisMinute, fmtLimit(snap.Limits.RPM), fmtTime(snap.MinuteResetsAt))
	fmt.Fprintf(tw, "tokens/min\t%d\t%s\t%s\n", snap.TokensThisMinute, fmtLimit(snap.Limits.TPM), fmtTime(snap.MinuteResetsAt))
	fmt.Fprintf(tw, "tokens/day\t%d\t%s\t%s\n", snap.TokensToday, fmtLimit(snap.Limits.DailyTokens), fmtTime(snap.DayResetsAt))
	fmt.Fprintf(tw, "tokens/month\t%d\t%s\t%s\n", snap.TokensThisMonth, fmtLimit(snap.Limits.MonthlyTokens), fmtTime(snap.MonthResetsAt))
	return tw.Flush()
}

func runQuotaSet(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("quota set", flag.ContinueOnError)
	cf := registerClientFlags(fs, withLocal)
	rpm := fs.Uint64("rpm", 0, "requests per minute (0 = unlimited)")
	tpm := fs.Uint64("tpm", 0, "tokens per minute (0 = unlimited)")
	daily := fs.Uint64("daily-tokens", 0, "daily token budget (0 = unlimited)")
	monthly := fs.Uint64("monthly-tokens", 0, "monthly token budget (0 = unlimited)")
	clear := fs.Bool("clear", false, "clear the per-key override (revert to global defaults)")
	setUsage(fs, "Usage: agentgpu quota set <id> [--rpm N] [--tpm N] [--daily-tokens N] [--monthly-tokens N] [--clear] [--local]")
	valueFlags := clientValueFlags()
	valueFlags["rpm"], valueFlags["tpm"], valueFlags["daily-tokens"], valueFlags["monthly-tokens"] = true, true, true, true
	if err := parseFlags(fs, out, reorderFlagsFirst(args, valueFlags)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("usage: agentgpu quota set <id> [--rpm N] [--tpm N] [--daily-tokens N] [--monthly-tokens N] [--clear]")
	}
	id := fs.Arg(0)

	// Record which numeric dimensions were explicitly provided so set can build a
	// precise update and reject an empty invocation (which would otherwise be an
	// ambiguous "set everything unlimited" vs "clear").
	set := setFlagNames(fs)
	anyNumeric := set["rpm"] || set["tpm"] || set["daily-tokens"] || set["monthly-tokens"]
	if !*clear && !anyNumeric {
		return usagef("quota set: specify at least one of --rpm/--tpm/--daily-tokens/--monthly-tokens, or --clear")
	}
	// --clear and a numeric dimension are contradictory (clear reverts to the
	// global defaults; a dimension sets an override). Rejecting the combination as a
	// usage error is clearer than silently letting --clear win, and is consistent
	// across the HTTP and local paths below.
	if *clear && anyNumeric {
		return usagef("quota set: --clear cannot be combined with --rpm/--tpm/--daily-tokens/--monthly-tokens")
	}

	if cf.isLocal() {
		return runQuotaSetLocal(ctx, out, cf, id, *clear, *rpm, *tpm, *daily, *monthly)
	}
	return runQuotaSetHTTP(ctx, out, cf, id, *clear, set, *rpm, *tpm, *daily, *monthly)
}

func runQuotaSetHTTP(ctx context.Context, out io.Writer, cf *clientFlags, id string, clear bool, set map[string]bool, rpm, tpm, daily, monthly uint64) error {
	c, err := cf.client()
	if err != nil {
		return err
	}
	var req apiclient.QuotaRequest
	// --clear sends an all-nil request, which the server interprets as "clear the
	// per-key override". Otherwise send a pointer for each provided dimension; an
	// omitted dimension defaults to 0 (unlimited) server-side.
	if !clear {
		if set["rpm"] {
			v := rpm
			req.RPM = &v
		}
		if set["tpm"] {
			v := tpm
			req.TPM = &v
		}
		if set["daily-tokens"] {
			v := daily
			req.DailyTokens = &v
		}
		if set["monthly-tokens"] {
			v := monthly
			req.MonthlyTokens = &v
		}
	}
	key, err := c.SetQuota(ctx, id, req)
	if err != nil {
		return err
	}
	printQuotaSetResult(out, key.ID, key.Limits)
	return nil
}

func runQuotaSetLocal(ctx context.Context, out io.Writer, cf *clientFlags, id string, clear bool, rpm, tpm, daily, monthly uint64) error {
	svc, st, err := cf.localService()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	var limits *store.Limits
	if !clear {
		limits = &store.Limits{RPM: rpm, TPM: tpm, DailyTokens: daily, MonthlyTokens: monthly}
	}
	key, err := svc.SetLimits(ctx, id, limits)
	if err != nil {
		return err
	}
	if key.Limits == nil {
		fmt.Fprintf(out, "Cleared quota override for key %s; now using global defaults.\n", key.ID)
		noteLocalRestart(out)
		return nil
	}
	printQuotaSetResult(out, key.ID, &apiclient.Limits{
		RPM:           key.Limits.RPM,
		TPM:           key.Limits.TPM,
		DailyTokens:   key.Limits.DailyTokens,
		MonthlyTokens: key.Limits.MonthlyTokens,
	})
	noteLocalRestart(out)
	return nil
}

// printQuotaSetResult prints the outcome of a quota set: the cleared notice when
// limits is nil, otherwise the new per-key limits. It is shared by both modes so
// the output is identical.
func printQuotaSetResult(out io.Writer, id string, limits *apiclient.Limits) {
	if limits == nil {
		fmt.Fprintf(out, "Cleared quota override for key %s; now using global defaults.\n", id)
		return
	}
	fmt.Fprintf(out, "Updated quota for key %s\n", id)
	fmt.Fprintf(out, "RPM: %s  TPM: %s  Daily: %s  Monthly: %s\n",
		fmtLimit(limits.RPM), fmtLimit(limits.TPM),
		fmtLimit(limits.DailyTokens), fmtLimit(limits.MonthlyTokens))
}

// setFlagNames returns the set of flag names that were explicitly provided on the
// command line (via fs.Visit), so a handler can distinguish "set to 0" from
// "omitted".
func setFlagNames(fs *flag.FlagSet) map[string]bool {
	set := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

// fmtLimit renders a limit value, showing "unlimited" for the zero value.
func fmtLimit(v uint64) string {
	if v == 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%d", v)
}
