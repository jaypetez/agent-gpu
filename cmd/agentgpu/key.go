package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jaypetez/agent-gpu/internal/apiclient"
	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/config"
	"github.com/jaypetez/agent-gpu/internal/store"
)

// keyUsage is the help text for `agentgpu key` (printed for `key --help` and on a
// missing subcommand). Each subcommand has its own --help with full flag details.
const keyUsage = `Usage: agentgpu key <command> [flags]

Manage API keys. By default these act against a RUNNING server over its HTTP admin
API (changes take effect immediately); --local uses the on-disk store for offline
bootstrap. Pass an admin token via --token/$AGENTGPU_TOKEN for the server path.

Commands:
  create   mint a new key and print its one-time token
  list     list keys (metadata only; never a secret)
  revoke   revoke a key (invalidates it immediately on a running server)
  rotate   replace a key's secret and print the new one-time token
  perms    replace a key's roles and allow/deny model lists
  quota    show or set a key's quota (alias for the top-level 'quota' command)

Run 'agentgpu key <command> --help' for that command's flags.`

// stringList is a flag.Value that accumulates repeated occurrences of a flag
// into a slice, so `--role a --role b` yields ["a", "b"].
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// clientValueFlags is the base set of mode-selection flags that consume a
// following argument (e.g. --token x vs --token=x), used by reorderFlagsFirst so
// a flag's value is never mistaken for the positional <id>. Command-specific
// value flags are merged on top per command.
func clientValueFlags() map[string]bool {
	return map[string]bool{
		"server": true,
		"url":    true,
		"token":  true,
		"store":  true,
	}
}

// reorderFlagsFirst moves flag tokens ahead of positional arguments so that
// `key rotate <id> --token x` parses identically to `key rotate --token x <id>`.
// The Go flag package stops parsing at the first non-flag token, so without this
// a trailing flag after the positional id would be ignored. valueFlags names the
// flags in this set that consume a following argument (e.g. --store x vs
// --store=x), so their value is not mistaken for the positional.
func reorderFlagsFirst(args []string, valueFlags map[string]bool) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) > 0 && a[0] == '-' {
			flags = append(flags, a)
			// If this flag takes a separate value (no '=') consume the next arg.
			name := strings.TrimLeft(a, "-")
			if !strings.Contains(a, "=") && valueFlags[name] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positional = append(positional, a)
	}
	return append(flags, positional...)
}

// runKeyCmd routes the `key` subcommand. It owns the API-key lifecycle CLI:
//
//	agentgpu key create --name <n> [--role r ...] [--allow-model m ...] [--deny-model m ...]
//	agentgpu key list
//	agentgpu key revoke <id>
//	agentgpu key rotate <id>
//	agentgpu key perms <id> [--role r ...] [--allow-model m ...] [--deny-model m ...]
//	agentgpu key quota <id>                              (alias for `quota show`)
//	agentgpu key quota set <id> [--rpm N] ...            (alias for `quota set`)
//
// By default these act against a RUNNING server over its HTTP admin API, so a
// revoke or permission change takes effect immediately. --local switches to the
// on-disk store for offline bootstrap (mint the first admin key before any server
// runs); a running server picks up a --local change only after a restart. The
// plaintext token is printed ONCE by create/rotate and never stored or shown
// again, in both modes.
func runKeyCmd(ctx context.Context, out io.Writer, args []string) error {
	if len(args) < 1 {
		return usagef("usage: agentgpu key <create|list|revoke|rotate|perms|quota> [args]")
	}
	if isHelpArg(args[0]) {
		return groupHelp(out, keyUsage)
	}

	sub := args[0]
	rest := args[1:]

	switch sub {
	case "create":
		return runKeyCreate(ctx, out, rest)
	case "list":
		return runKeyList(ctx, out, rest)
	case "revoke":
		return runKeyRevoke(ctx, out, rest)
	case "rotate":
		return runKeyRotate(ctx, out, rest)
	case "perms":
		return runKeyPerms(ctx, out, rest)
	case "quota":
		// `key quota [set] ...` is the historical spelling; it delegates to the
		// same handlers as the top-level `quota` command.
		return runQuotaCmd(ctx, out, rest)
	default:
		return usagef("unknown key subcommand %q", sub)
	}
}

// openStore resolves the store path (flag > env > default) and opens the
// file-backed key store. The server uses it directly so per-key quota Limits
// persist across restarts and are visible on the dispatch path; the CLI uses it
// for the --local (offline) management path.
func openStore(storeFlag string) (store.Store, error) {
	path := config.ResolveStorePath(storeFlag, nil, nil)
	return store.NewFile(path)
}

func runKeyCreate(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("key create", flag.ContinueOnError)
	cf := registerClientFlags(fs, withLocal)
	name := fs.String("name", "", "human-readable label for the key")
	var roles, allow, deny stringList
	fs.Var(&roles, "role", "grant a role (admin|user|read-only); repeatable")
	fs.Var(&allow, "allow-model", "allow a model by name; repeatable")
	fs.Var(&deny, "deny-model", "deny a model by name (deny wins); repeatable")
	setUsage(fs, "Usage: agentgpu key create --name <name> [--role r ...] [--allow-model m ...] [--deny-model m ...] [--local]")
	if err := parseFlags(fs, out, args); err != nil {
		return err
	}
	if *name == "" {
		return usagef("key create: --name is required")
	}

	if cf.isLocal() {
		return runKeyCreateLocal(ctx, out, cf, *name, roles, allow, deny)
	}
	return runKeyCreateHTTP(ctx, out, cf, *name, roles, allow, deny)
}

func runKeyCreateHTTP(ctx context.Context, out io.Writer, cf *clientFlags, name string, roles, allow, deny stringList) error {
	c, err := cf.client()
	if err != nil {
		return err
	}
	resp, err := c.CreateKey(ctx, apiclient.CreateKeyRequest{
		Name:        name,
		Roles:       roles,
		AllowModels: allow,
		DenyModels:  deny,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Created key %q (id: %s)\n", resp.Name, resp.ID)
	if len(resp.Roles) > 0 || len(resp.AllowModels) > 0 || len(resp.DenyModels) > 0 {
		fmt.Fprintf(out, "Roles: %s  Allow: %s  Deny: %s\n",
			fmtList(resp.Roles), fmtList(resp.AllowModels), fmtList(resp.DenyModels))
	}
	printToken(out, resp.Token)
	return nil
}

func runKeyCreateLocal(ctx context.Context, out io.Writer, cf *clientFlags, name string, roles, allow, deny stringList) error {
	// create is the one mutating local op that is LEGAL on an empty store (it is the
	// bootstrap itself); the guard rejects it only once the store already has keys,
	// at which point a second key must be minted through the running server's API.
	svc, st, err := cf.localBootstrapService(ctx, "key create")
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	perms := auth.Permissions{Roles: roles, AllowModels: allow, DenyModels: deny}
	token, key, err := svc.CreateWithPermissions(ctx, name, perms)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Created key %q (id: %s)\n", key.Name, key.ID)
	if len(key.Roles) > 0 || len(key.AllowModels) > 0 || len(key.DenyModels) > 0 {
		fmt.Fprintf(out, "Roles: %s  Allow: %s  Deny: %s\n",
			fmtList(key.Roles), fmtList(key.AllowModels), fmtList(key.DenyModels))
	}
	printToken(out, token)
	noteLocalRestart(out)
	return nil
}

// runKeyPerms sets (replaces) a key's roles and allow/deny lists. Omitting all
// of --role/--allow-model/--deny-model clears every list, which is an explicit
// way to revoke all access without revoking the key itself.
func runKeyPerms(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("key perms", flag.ContinueOnError)
	cf := registerClientFlags(fs, withLocal)
	var roles, allow, deny stringList
	fs.Var(&roles, "role", "set a role (admin|user|read-only); repeatable")
	fs.Var(&allow, "allow-model", "set an allowed model by name; repeatable")
	fs.Var(&deny, "deny-model", "set a denied model by name (deny wins); repeatable")
	setUsage(fs, "Usage: agentgpu key perms <id> [--role r ...] [--allow-model m ...] [--deny-model m ...] [--local]")
	valueFlags := clientValueFlags()
	valueFlags["role"], valueFlags["allow-model"], valueFlags["deny-model"] = true, true, true
	if err := parseFlags(fs, out, reorderFlagsFirst(args, valueFlags)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("usage: agentgpu key perms <id> [--role r ...] [--allow-model m ...] [--deny-model m ...]")
	}
	id := fs.Arg(0)

	if cf.isLocal() {
		return runKeyPermsLocal(ctx, out, cf, id, roles, allow, deny)
	}
	return runKeyPermsHTTP(ctx, out, cf, id, roles, allow, deny)
}

func runKeyPermsHTTP(ctx context.Context, out io.Writer, cf *clientFlags, id string, roles, allow, deny stringList) error {
	c, err := cf.client()
	if err != nil {
		return err
	}
	key, err := c.SetPermissions(ctx, id, apiclient.PermissionsRequest{
		Roles:       roles,
		AllowModels: allow,
		DenyModels:  deny,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated permissions for key %s\n", key.ID)
	fmt.Fprintf(out, "Roles: %s  Allow: %s  Deny: %s\n",
		fmtList(key.Roles), fmtList(key.AllowModels), fmtList(key.DenyModels))
	return nil
}

func runKeyPermsLocal(ctx context.Context, out io.Writer, cf *clientFlags, id string, roles, allow, deny stringList) error {
	svc, st, err := cf.localBootstrapService(ctx, "key perms")
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	key, err := svc.SetPermissions(ctx, id, auth.Permissions{Roles: roles, AllowModels: allow, DenyModels: deny})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated permissions for key %s\n", key.ID)
	fmt.Fprintf(out, "Roles: %s  Allow: %s  Deny: %s\n",
		fmtList(key.Roles), fmtList(key.AllowModels), fmtList(key.DenyModels))
	noteLocalRestart(out)
	return nil
}

func runKeyList(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("key list", flag.ContinueOnError)
	cf := registerClientFlags(fs, withLocal)
	setUsage(fs, "Usage: agentgpu key list [--local]")
	if err := parseFlags(fs, out, args); err != nil {
		return err
	}

	if cf.isLocal() {
		return runKeyListLocal(ctx, out, cf)
	}
	return runKeyListHTTP(ctx, out, cf)
}

func runKeyListHTTP(ctx context.Context, out io.Writer, cf *clientFlags) error {
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
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tCREATED\tLAST USED\tUSAGE\tREVOKED\tROLES\tALLOW\tDENY")
	for _, k := range keys {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%t\t%s\t%s\t%s\n",
			k.ID, k.Name, fmtUnix(k.Created), fmtUnix(k.LastUsed), k.UsageCount, k.Revoked,
			fmtList(k.Roles), fmtList(k.AllowModels), fmtList(k.DenyModels))
	}
	return tw.Flush()
}

func runKeyListLocal(ctx context.Context, out io.Writer, cf *clientFlags) error {
	svc, st, err := cf.localService()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	keys, err := svc.List(ctx)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		fmt.Fprintln(out, "No keys.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tCREATED\tLAST USED\tUSAGE\tREVOKED\tROLES\tALLOW\tDENY")
	for _, k := range keys {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%t\t%s\t%s\t%s\n",
			k.ID, k.Name, fmtTime(k.CreatedAt), fmtTime(k.LastUsedAt), k.UsageCount, k.Revoked(),
			fmtList(k.Roles), fmtList(k.AllowModels), fmtList(k.DenyModels))
	}
	return tw.Flush()
}

func runKeyRevoke(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("key revoke", flag.ContinueOnError)
	cf := registerClientFlags(fs, withLocal)
	setUsage(fs, "Usage: agentgpu key revoke <id> [--local]")
	if err := parseFlags(fs, out, reorderFlagsFirst(args, clientValueFlags())); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("usage: agentgpu key revoke <id>")
	}
	id := fs.Arg(0)

	if cf.isLocal() {
		return runKeyRevokeLocal(ctx, out, cf, id)
	}
	c, err := cf.client()
	if err != nil {
		return err
	}
	if err := c.RevokeKey(ctx, id); err != nil {
		return err
	}
	fmt.Fprintf(out, "Revoked key %s\n", id)
	return nil
}

func runKeyRevokeLocal(ctx context.Context, out io.Writer, cf *clientFlags, id string) error {
	svc, st, err := cf.localBootstrapService(ctx, "key revoke")
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	if err := svc.Revoke(ctx, id); err != nil {
		return err
	}
	fmt.Fprintf(out, "Revoked key %s\n", id)
	noteLocalRestart(out)
	return nil
}

func runKeyRotate(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("key rotate", flag.ContinueOnError)
	cf := registerClientFlags(fs, withLocal)
	setUsage(fs, "Usage: agentgpu key rotate <id> [--local]")
	if err := parseFlags(fs, out, reorderFlagsFirst(args, clientValueFlags())); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usagef("usage: agentgpu key rotate <id>")
	}
	id := fs.Arg(0)

	if cf.isLocal() {
		return runKeyRotateLocal(ctx, out, cf, id)
	}
	c, err := cf.client()
	if err != nil {
		return err
	}
	resp, err := c.RotateKey(ctx, id)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Rotated key %s; the old token no longer works.\n", id)
	printToken(out, resp.Token)
	return nil
}

func runKeyRotateLocal(ctx context.Context, out io.Writer, cf *clientFlags, id string) error {
	svc, st, err := cf.localBootstrapService(ctx, "key rotate")
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	token, err := svc.Rotate(ctx, id)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Rotated key %s; the old token no longer works.\n", id)
	printToken(out, token)
	noteLocalRestart(out)
	return nil
}

// printToken displays a one-time plaintext token with a save-it-now warning.
func printToken(out io.Writer, token string) {
	fmt.Fprintf(out, "\nToken: %s\n", token)
	fmt.Fprintln(out, "Save it now — it will not be shown again.")
}

// noteLocalRestart reminds the operator that a --local change is written to the
// on-disk store and is not seen by an already-running server until it restarts.
// It is printed only on the offline path, where the caveat applies.
func noteLocalRestart(out io.Writer) {
	fmt.Fprintln(out, "(local store updated; restart a running server for it to take effect)")
}

// fmtTime renders a timestamp, showing a dash for the zero value.
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

// fmtUnix renders a unix-seconds timestamp (the admin API's wire form), showing a
// dash for the zero value. It is the HTTP-mode counterpart of fmtTime.
func fmtUnix(sec int64) string {
	if sec == 0 {
		return "-"
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// fmtList renders a string slice for the list view, showing a dash when empty.
func fmtList(xs []string) string {
	if len(xs) == 0 {
		return "-"
	}
	return strings.Join(xs, ",")
}
