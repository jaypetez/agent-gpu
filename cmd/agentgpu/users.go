package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/jaypetez/agent-gpu/internal/apiclient"
)

// runUsersCmd implements the `users` subcommand (#104): a client-side,
// label-grouped view over the keys admin API (GET /v1/admin/keys). agent-gpu's
// identity model is per-key labels only — there is NO org/user resource and no
// /v1/admin/users endpoint — so "users" is a presentation aggregation: it lists
// keys, groups them by their owner (or team) label, and reports per-group counts
// and the union of roles. The grouping is done in the CLI; the only server call is
// the existing ListKeys.
//
//	agentgpu users [--by owner|team]
//
// It is HTTP-only — keys live on a running server. No secrets are shown (ListKeys
// returns metadata only).
func runUsersCmd(ctx context.Context, out io.Writer, args []string) error {
	if len(args) >= 1 && isHelpArg(args[0]) {
		return groupHelp(out, usersUsage)
	}
	fs := flag.NewFlagSet("users", flag.ContinueOnError)
	cf := registerClientFlags(fs, httpOnlyMode)
	by := fs.String("by", "owner", "group keys by this label: owner or team")
	setUsage(fs, "Usage: agentgpu users [--by owner|team]")
	if err := parseFlags(fs, out, args); err != nil {
		return err
	}
	dim := strings.ToLower(strings.TrimSpace(*by))
	if dim != "owner" && dim != "team" {
		return usagef("users: --by must be owner or team, got %q", *by)
	}

	c, err := cf.client()
	if err != nil {
		return err
	}
	keys, err := c.ListKeys(ctx)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		fmt.Fprintln(out, "No keys.")
		return nil
	}

	groups := groupKeysByLabel(keys, dim)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "%s\tKEYS\tACTIVE\tROLES\n", strings.ToUpper(dim))
	for _, g := range groups {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n", g.label, g.keys, g.active, fmtList(g.roles))
	}
	return tw.Flush()
}

// usersUsage is the help text for `agentgpu users`.
const usersUsage = `Usage: agentgpu users [--by owner|team]

Group the API keys of a RUNNING server (a server URL via
--server/$AGENTGPU_HTTP_ADDR and an admin token via --token/$AGENTGPU_TOKEN) by
their owner or team label, showing per-group key counts and the union of roles.

agent-gpu's identity model is per-key labels only (there is no separate user/org
resource), so this is a view over the keys, grouped client-side.

  --by owner|team   the label to group by (default owner)

  agentgpu users --by team`

// userGroup is one label-grouped row: the label value, the total and active
// (non-revoked) key counts, and the sorted union of roles seen across the group.
type userGroup struct {
	label  string
	keys   int
	active int
	roles  []string
}

// groupKeysByLabel aggregates keys by the chosen label dimension ("owner" or
// "team"), returning the groups sorted by label. Keys with an empty label for the
// dimension are collected under "(unassigned)" so they are still visible. The roles
// of each group are the deduplicated, sorted union across its keys.
func groupKeysByLabel(keys []apiclient.KeyView, dim string) []userGroup {
	const unassigned = "(unassigned)"
	type agg struct {
		keys   int
		active int
		roles  map[string]struct{}
	}
	byLabel := map[string]*agg{}
	for _, k := range keys {
		label := k.Owner
		if dim == "team" {
			label = k.Team
		}
		if strings.TrimSpace(label) == "" {
			label = unassigned
		}
		a := byLabel[label]
		if a == nil {
			a = &agg{roles: map[string]struct{}{}}
			byLabel[label] = a
		}
		a.keys++
		if !k.Revoked {
			a.active++
		}
		for _, r := range k.Roles {
			a.roles[r] = struct{}{}
		}
	}

	groups := make([]userGroup, 0, len(byLabel))
	for label, a := range byLabel {
		roles := make([]string, 0, len(a.roles))
		for r := range a.roles {
			roles = append(roles, r)
		}
		sort.Strings(roles)
		groups = append(groups, userGroup{label: label, keys: a.keys, active: a.active, roles: roles})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].label < groups[j].label })
	return groups
}
