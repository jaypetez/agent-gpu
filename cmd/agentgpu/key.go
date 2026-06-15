package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/config"
	"github.com/jaypetez/agent-gpu/internal/store"
)

// stringList is a flag.Value that accumulates repeated occurrences of a flag
// into a slice, so `--role a --role b` yields ["a", "b"].
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// reorderFlagsFirst moves flag tokens ahead of positional arguments so that
// `key rotate <id> --store x` parses identically to `key rotate --store x <id>`.
// The Go flag package stops parsing at the first non-flag token, so without
// this a trailing flag after the positional id would be ignored. valueFlags
// names the flags in this set that consume a following argument (e.g. --store x
// vs --store=x), so their value is not mistaken for the positional.
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
//
// Keys are persisted to a JSON store so they survive across invocations; the
// path comes from --store / AGENTGPU_STORE_PATH / the default. The plaintext
// token is printed ONCE by create/rotate and is never stored or shown again.
//
// Role/permission management lives on this CLI until the admin HTTP endpoints
// (#4) exist.
func runKeyCmd(ctx context.Context, out io.Writer, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: agentgpu key <create|list|revoke|rotate|perms> [args]")
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
	default:
		return fmt.Errorf("unknown key subcommand %q", sub)
	}
}

// openService resolves the store path from a flag set's --store value (env and
// default fill the gaps), opens the file-backed store, and returns an auth
// Service plus the store for cleanup.
func openService(storeFlag string) (*auth.Service, store.Store, error) {
	path := config.ResolveStorePath(storeFlag, nil, nil)
	st, err := store.NewFile(path)
	if err != nil {
		return nil, nil, err
	}
	return auth.NewService(st), st, nil
}

func runKeyCreate(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("key create", flag.ContinueOnError)
	name := fs.String("name", "", "human-readable label for the key")
	storeFlag := fs.String("store", "", "path to the keys file (default $AGENTGPU_STORE_PATH or ~/.agentgpu/keys.json)")
	var roles, allow, deny stringList
	fs.Var(&roles, "role", "grant a role (admin|user|read-only); repeatable")
	fs.Var(&allow, "allow-model", "allow a model by name; repeatable")
	fs.Var(&deny, "deny-model", "deny a model by name (deny wins); repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("key create: --name is required")
	}

	svc, st, err := openService(*storeFlag)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	perms := auth.Permissions{Roles: roles, AllowModels: allow, DenyModels: deny}
	token, key, err := svc.CreateWithPermissions(ctx, *name, perms)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Created key %q (id: %s)\n", key.Name, key.ID)
	if len(key.Roles) > 0 || len(key.AllowModels) > 0 || len(key.DenyModels) > 0 {
		fmt.Fprintf(out, "Roles: %s  Allow: %s  Deny: %s\n",
			fmtList(key.Roles), fmtList(key.AllowModels), fmtList(key.DenyModels))
	}
	printToken(out, token)
	return nil
}

// runKeyPerms sets (replaces) a key's roles and allow/deny lists. Omitting all
// of --role/--allow-model/--deny-model clears every list, which is an explicit
// way to revoke all access without revoking the key itself.
func runKeyPerms(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("key perms", flag.ContinueOnError)
	storeFlag := fs.String("store", "", "path to the keys file (default $AGENTGPU_STORE_PATH or ~/.agentgpu/keys.json)")
	var roles, allow, deny stringList
	fs.Var(&roles, "role", "set a role (admin|user|read-only); repeatable")
	fs.Var(&allow, "allow-model", "set an allowed model by name; repeatable")
	fs.Var(&deny, "deny-model", "set a denied model by name (deny wins); repeatable")
	valueFlags := map[string]bool{"store": true, "role": true, "allow-model": true, "deny-model": true}
	if err := fs.Parse(reorderFlagsFirst(args, valueFlags)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: agentgpu key perms <id> [--role r ...] [--allow-model m ...] [--deny-model m ...]")
	}
	id := fs.Arg(0)

	svc, st, err := openService(*storeFlag)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	perms := auth.Permissions{Roles: roles, AllowModels: allow, DenyModels: deny}
	key, err := svc.SetPermissions(ctx, id, perms)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated permissions for key %s\n", key.ID)
	fmt.Fprintf(out, "Roles: %s  Allow: %s  Deny: %s\n",
		fmtList(key.Roles), fmtList(key.AllowModels), fmtList(key.DenyModels))
	return nil
}

func runKeyList(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("key list", flag.ContinueOnError)
	storeFlag := fs.String("store", "", "path to the keys file (default $AGENTGPU_STORE_PATH or ~/.agentgpu/keys.json)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	svc, st, err := openService(*storeFlag)
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
	storeFlag := fs.String("store", "", "path to the keys file (default $AGENTGPU_STORE_PATH or ~/.agentgpu/keys.json)")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"store": true})); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: agentgpu key revoke <id>")
	}
	id := fs.Arg(0)

	svc, st, err := openService(*storeFlag)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	if err := svc.Revoke(ctx, id); err != nil {
		return err
	}
	fmt.Fprintf(out, "Revoked key %s\n", id)
	return nil
}

func runKeyRotate(ctx context.Context, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("key rotate", flag.ContinueOnError)
	storeFlag := fs.String("store", "", "path to the keys file (default $AGENTGPU_STORE_PATH or ~/.agentgpu/keys.json)")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"store": true})); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: agentgpu key rotate <id>")
	}
	id := fs.Arg(0)

	svc, st, err := openService(*storeFlag)
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
	return nil
}

// printToken displays a one-time plaintext token with a save-it-now warning.
func printToken(out io.Writer, token string) {
	fmt.Fprintf(out, "\nToken: %s\n", token)
	fmt.Fprintln(out, "Save it now — it will not be shown again.")
}

// fmtTime renders a timestamp, showing a dash for the zero value.
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

// fmtList renders a string slice for the list view, showing a dash when empty.
func fmtList(xs []string) string {
	if len(xs) == 0 {
		return "-"
	}
	return strings.Join(xs, ",")
}
