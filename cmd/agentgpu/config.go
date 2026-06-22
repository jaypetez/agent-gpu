package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/jaypetez/agent-gpu/internal/apiclient"
)

// runConfigCmd routes the `config` subcommand (#104): a thin client over the
// runtime settings admin API (GET/PUT /v1/admin/config, #92).
//
//	agentgpu config get                       print the effective runtime config
//	agentgpu config set field=value [...]      apply a partial update (live, no restart)
//
// It is HTTP-only — runtime config lives on a running server, and editing it is a
// runtime mutation, so there is no --local mode. The server validates the update
// and rejects an invalid value / unknown key / read-only field with a 400 that the
// CLI surfaces verbatim; `set` echoes the resulting config so the operator sees the
// applied state.
func runConfigCmd(ctx context.Context, out io.Writer, args []string) error {
	if len(args) < 1 {
		return usagef("usage: agentgpu config <get|set> [field=value ...]")
	}
	if isHelpArg(args[0]) {
		return groupHelp(out, configUsage)
	}
	switch args[0] {
	case "get":
		return runConfigGet(ctx, out, args[1:])
	case "set":
		return runConfigSet(ctx, out, args[1:])
	default:
		return usagef("unknown config subcommand %q", args[0])
	}
}

// configUsage is the help text for `agentgpu config`.
const configUsage = `Usage: agentgpu config <get|set> [field=value ...]

Inspect and update the server's runtime-tunable settings (applied LIVE, no
restart). This command requires a running server: a server URL
(--server/$AGENTGPU_HTTP_ADDR) and an admin token (--token/$AGENTGPU_TOKEN).

Commands:
  get                      print the effective settings and the read-only values
  set field=value [...]    apply a partial update; only the named fields change

Boot-only fields (e.g. server_listen) are shown by 'get' but rejected by 'set'.
Durations are written as Go duration strings, e.g. session_ttl=45m.

  agentgpu config get
  agentgpu config set log_level=debug quota_default_rpm=120 session_ttl=45m`

func runConfigGet(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("config get", flag.ContinueOnError)
	cf := registerClientFlags(fs, httpOnlyMode)
	setUsage(fs, "Usage: agentgpu config get")
	if err := parseFlags(fs, out, args); err != nil {
		return err
	}
	c, err := cf.client()
	if err != nil {
		return err
	}
	resp, err := c.GetConfig(ctx)
	if err != nil {
		return err
	}
	printConfig(out, resp)
	return nil
}

func runConfigSet(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("config set", flag.ContinueOnError)
	cf := registerClientFlags(fs, httpOnlyMode)
	setUsage(fs, "Usage: agentgpu config set field=value [field=value ...]")
	// field=value pairs are positional; reorder so a trailing --server/--token after
	// them still parses (the flag package stops at the first non-flag token).
	if err := parseFlags(fs, out, reorderFlagsFirst(args, clientValueFlags())); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return usagef("config set: provide at least one field=value pair (e.g. log_level=debug)")
	}
	patch, err := parseConfigPairs(fs.Args())
	if err != nil {
		return err
	}

	c, err := cf.client()
	if err != nil {
		return err
	}
	resp, err := c.PutConfig(ctx, patch)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "Updated runtime config.")
	printConfig(out, resp)
	return nil
}

// configIntFields is the set of runtime-tunable config keys whose value is a JSON
// number (everything else — log level, durations, overflow policy — is a string).
// parseConfigPairs uses it to coerce a "field=value" token to the right JSON type
// so the server's typed decode accepts it; an unknown field is sent as a string and
// the server rejects it with a clear "unknown config field" message (the CLI does
// no field validation of its own, keeping the server the single source of truth).
var configIntFields = map[string]bool{
	"quota_default_rpm":            true,
	"quota_default_tpm":            true,
	"quota_default_daily_tokens":   true,
	"quota_default_monthly_tokens": true,
	"quota_global_rpm":             true,
	"quota_global_tpm":             true,
	"session_max_turns":            true,
	"session_max_bytes":            true,
	"session_max_context_tokens":   true,
	"session_max_sessions_per_key": true,
}

// parseConfigPairs turns "field=value" tokens into the patch map PutConfig sends.
// A token missing '=' (or with an empty field) is a usage error. A duplicate field
// is a usage error so an ambiguous `set rpm=1 rpm=2` is rejected rather than
// silently keeping one. Values for integer-typed fields (configIntFields) are
// parsed as numbers; all other values are sent as strings (the server validates
// the actual range/enum and the field name).
func parseConfigPairs(pairs []string) (map[string]any, error) {
	patch := make(map[string]any, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, usagef("config set: %q is not a field=value pair", p)
		}
		if _, dup := patch[k]; dup {
			return nil, usagef("config set: field %q specified more than once", k)
		}
		if configIntFields[k] {
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return nil, usagef("config set: %s must be a non-negative integer, got %q", k, v)
			}
			patch[k] = n
			continue
		}
		patch[k] = v
	}
	return patch, nil
}

// printConfig renders a GET/PUT config response: the tunable settings as a
// NAME/VALUE table, then the read-only (boot-only) values flagged as such so the
// operator can see them without consulting flags. Field order is stable
// (alphabetical) for a deterministic, diffable display.
func printConfig(out io.Writer, resp apiclient.ConfigResponse) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SETTING\tVALUE")
	for _, kv := range configSettingRows(resp.Settings) {
		fmt.Fprintf(tw, "%s\t%s\n", kv[0], kv[1])
	}
	_ = tw.Flush()

	if len(resp.ReadOnlyFields) == 0 {
		return
	}
	fmt.Fprintln(out, "\nRead-only (restart to change):")
	ro := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(ro, "SETTING\tVALUE")
	for _, kv := range configReadOnlyRows(resp.ReadOnly) {
		fmt.Fprintf(ro, "%s\t%s\n", kv[0], kv[1])
	}
	_ = ro.Flush()
}

// configSettingRows projects the tunable settings into sorted [key, value] rows for
// the table. Keys match the wire field names (so `config set <key>=...` uses the
// same spelling the table shows).
func configSettingRows(s apiclient.ConfigSettings) [][2]string {
	rows := [][2]string{
		{"log_level", s.LogLevel},
		{"quota_default_rpm", fmtUint(s.QuotaDefaultRPM)},
		{"quota_default_tpm", fmtUint(s.QuotaDefaultTPM)},
		{"quota_default_daily_tokens", fmtUint(s.QuotaDefaultDailyTokens)},
		{"quota_default_monthly_tokens", fmtUint(s.QuotaDefaultMonthlyTokens)},
		{"quota_global_rpm", fmtUint(s.QuotaGlobalRPM)},
		{"quota_global_tpm", fmtUint(s.QuotaGlobalTPM)},
		{"session_ttl", fmtStr(s.SessionTTL)},
		{"session_max_turns", strconv.Itoa(s.SessionMaxTurns)},
		{"session_max_bytes", strconv.Itoa(s.SessionMaxBytes)},
		{"session_max_context_tokens", strconv.Itoa(s.SessionMaxContextTokens)},
		{"session_max_sessions_per_key", strconv.Itoa(s.SessionMaxSessionsPerKey)},
		{"session_overflow_policy", fmtStr(s.SessionOverflowPolicy)},
		{"model_warm_max", fmtStr(s.ModelWarmMax)},
		{"heartbeat_timeout", fmtStr(s.HeartbeatTimeout)},
	}
	sortRows(rows)
	return rows
}

// configReadOnlyRows projects the boot-only settings into sorted [key, value] rows.
func configReadOnlyRows(r apiclient.ConfigReadOnly) [][2]string {
	rows := [][2]string{
		{"server_listen", fmtStr(r.ServerListen)},
		{"server_http_listen", fmtStr(r.ServerHTTPListen)},
		{"server_metrics_listen", fmtStr(r.ServerMetricsListen)},
		{"quota_path", fmtStr(r.QuotaPath)},
		{"session_path", fmtStr(r.SessionPath)},
		{"log_format", fmtStr(r.LogFormat)},
		{"log_output", fmtStr(r.LogOutput)},
	}
	sortRows(rows)
	return rows
}

// sortRows orders [key,value] rows by key for a stable, diffable table.
func sortRows(rows [][2]string) {
	sort.Slice(rows, func(i, j int) bool { return rows[i][0] < rows[j][0] })
}

// fmtUint renders an unsigned config value, showing "0" verbatim (a 0 limit is a
// meaningful "unlimited"/"default" the operator should see explicitly here, unlike
// the quota table where it maps to "unlimited").
func fmtUint(v uint64) string { return strconv.FormatUint(v, 10) }

// fmtStr renders a string config value, showing a dash for the empty value so a
// blank cell is unambiguous in the table.
func fmtStr(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
