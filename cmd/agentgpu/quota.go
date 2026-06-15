package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/jaypetez/agent-gpu/internal/config"
	"github.com/jaypetez/agent-gpu/internal/quota"
	"github.com/jaypetez/agent-gpu/internal/store"
)

// runKeyQuota routes the `key quota` subcommand:
//
//	agentgpu key quota <id>                  show usage vs limits
//	agentgpu key quota set <id> [--rpm N] [--tpm N] [--daily-tokens N] [--monthly-tokens N]
//
// Inspection reads the live counter checkpoint (no server required); `set`
// persists the per-key Limits override. The admin HTTP equivalents are #4.
// Secrets are never printed.
func runKeyQuota(ctx context.Context, out io.Writer, args []string) error {
	if len(args) >= 1 && args[0] == "set" {
		return runKeyQuotaSet(ctx, out, args[1:])
	}
	return runKeyQuotaShow(ctx, out, args)
}

// effectiveLimits resolves the limits shown/applied for a key: its per-key
// override if set, else the global defaults from QuotaConfig.
func effectiveLimits(key store.APIKey, defaults store.Limits) store.Limits {
	if key.Limits != nil {
		return *key.Limits
	}
	return defaults
}

func runKeyQuotaShow(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("key quota", flag.ContinueOnError)
	storeFlag := fs.String("store", "", "path to the keys file (default $AGENTGPU_STORE_PATH or ~/.agentgpu/keys.json)")
	quotaFlag := fs.String("quota-path", "", "path to the quota counter checkpoint (default $AGENTGPU_QUOTA_PATH or ~/.agentgpu/quota.json)")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"store": true, "quota-path": true})); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: agentgpu key quota <id>")
	}
	id := fs.Arg(0)

	svc, st, err := openService(*storeFlag)
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
		return fmt.Errorf("key quota: %w", store.ErrNotFound)
	}

	// Load the persisted counters so usage reflects what the server recorded.
	cs := quota.NewMemoryCounterStore()
	quotaPath := config.ResolveQuotaPath(*quotaFlag, nil, nil)
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

func runKeyQuotaSet(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("key quota set", flag.ContinueOnError)
	storeFlag := fs.String("store", "", "path to the keys file (default $AGENTGPU_STORE_PATH or ~/.agentgpu/keys.json)")
	rpm := fs.Uint64("rpm", 0, "requests per minute (0 = unlimited)")
	tpm := fs.Uint64("tpm", 0, "tokens per minute (0 = unlimited)")
	daily := fs.Uint64("daily-tokens", 0, "daily token budget (0 = unlimited)")
	monthly := fs.Uint64("monthly-tokens", 0, "monthly token budget (0 = unlimited)")
	clear := fs.Bool("clear", false, "clear the per-key override (revert to global defaults)")
	valueFlags := map[string]bool{
		"store": true, "rpm": true, "tpm": true, "daily-tokens": true, "monthly-tokens": true,
	}
	if err := fs.Parse(reorderFlagsFirst(args, valueFlags)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: agentgpu key quota set <id> [--rpm N] [--tpm N] [--daily-tokens N] [--monthly-tokens N] [--clear]")
	}
	id := fs.Arg(0)

	svc, st, err := openService(*storeFlag)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	var limits *store.Limits
	if !*clear {
		limits = &store.Limits{
			RPM:           *rpm,
			TPM:           *tpm,
			DailyTokens:   *daily,
			MonthlyTokens: *monthly,
		}
	}
	key, err := svc.SetLimits(ctx, id, limits)
	if err != nil {
		return err
	}

	if key.Limits == nil {
		fmt.Fprintf(out, "Cleared quota override for key %s; now using global defaults.\n", key.ID)
		return nil
	}
	fmt.Fprintf(out, "Updated quota for key %s\n", key.ID)
	fmt.Fprintf(out, "RPM: %s  TPM: %s  Daily: %s  Monthly: %s\n",
		fmtLimit(key.Limits.RPM), fmtLimit(key.Limits.TPM),
		fmtLimit(key.Limits.DailyTokens), fmtLimit(key.Limits.MonthlyTokens))
	return nil
}

// fmtLimit renders a limit value, showing "unlimited" for the zero value.
func fmtLimit(v uint64) string {
	if v == 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%d", v)
}
