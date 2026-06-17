package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/jaypetez/agent-gpu/internal/apiclient"
	"github.com/jaypetez/agent-gpu/internal/auth"
	"github.com/jaypetez/agent-gpu/internal/config"
	"github.com/jaypetez/agent-gpu/internal/store"
)

// clientFlags holds the mode-selection flags shared by the key/quota/models
// commands. The same set is registered on each subcommand's FlagSet so the flags
// (and their precedence) are identical everywhere.
//
// Mode selection: by default the command targets a RUNNING server over its HTTP
// admin API (so revoke/quota changes take effect immediately). --local switches
// the key/quota commands to write the on-disk store directly, which is the
// offline bootstrap path used to mint the first admin key before any server runs
// — a running server does NOT observe a --local change until it restarts.
type clientFlags struct {
	server *string // --server / --url: HTTP API base URL
	token  *string // --token: admin Bearer token
	local  *bool   // --local: use the on-disk store instead of the HTTP API
	store  *string // --store: on-disk keys file path (only meaningful with --local)
}

// supportsLocal controls whether a command registers the --local/--store flags.
// The key and quota commands do (bootstrap + offline use); models does not (the
// catalog only exists on a running server).
type supportsLocal bool

const (
	withLocal    supportsLocal = true
	httpOnlyMode supportsLocal = false
)

// registerClientFlags adds the mode-selection flags to fs and returns the holder.
// When local is httpOnlyMode the --local/--store flags are omitted (the command
// has no offline mode).
func registerClientFlags(fs *flag.FlagSet, local supportsLocal) *clientFlags {
	cf := &clientFlags{
		server: fs.String("server", "", "HTTP API base URL of the running server (or $AGENTGPU_HTTP_ADDR, default http://127.0.0.1:8080)"),
		token:  fs.String("token", "", "admin Bearer token for the HTTP API (or $AGENTGPU_TOKEN)"),
	}
	// --url is an alias for --server, matching common CLI ergonomics.
	fs.Var(newAliasValue(cf.server), "url", "alias for --server")
	if local {
		cf.local = fs.Bool("local", false, "manage the on-disk key store directly instead of a running server (offline bootstrap; requires a server restart to take effect)")
		cf.store = fs.String("store", "", "path to the on-disk keys file used with --local (or $AGENTGPU_STORE_PATH, default ~/.agentgpu/keys.json)")
	}
	return cf
}

// isLocal reports whether the command should use the on-disk store path. It is
// false for http-only commands (which never register --local).
func (cf *clientFlags) isLocal() bool {
	return cf.local != nil && *cf.local
}

// client builds an apiclient.Client for HTTP mode, resolving the base URL and
// token with flag > env > default precedence. It returns a usageError when no
// token is configured, because targeting a live admin API without one is a
// guaranteed 401 — the message points the user at the --token path (and, for
// commands that support it, the --local offline path) so the bootstrap
// chicken-and-egg is discoverable.
func (cf *clientFlags) client() (*apiclient.Client, error) {
	base := config.ResolveHTTPAddr(*cf.server, nil)
	token := config.ResolveToken(*cf.token, nil)
	if token == "" {
		msg := fmt.Sprintf("no admin token configured: set --token or $%s to manage a running server", config.EnvToken)
		// Only commands that register --local can fall back to the on-disk store, so
		// only those mention it (e.g. `models list` is HTTP-only).
		if cf.local != nil {
			msg += ", or use --local to manage the on-disk store for offline bootstrap"
		}
		return nil, &usageError{err: errors.New(msg)}
	}
	return apiclient.New(base, token), nil
}

// localService opens the on-disk store and returns an auth.Service over it (the
// offline path). It is only reachable when --local is set. The caller must Close
// the returned store.
func (cf *clientFlags) localService() (*auth.Service, store.Store, error) {
	storeFlag := ""
	if cf.store != nil {
		storeFlag = *cf.store
	}
	st, err := openStore(storeFlag)
	if err != nil {
		return nil, nil, err
	}
	return auth.NewService(st), st, nil
}

// aliasValue is a flag.Value that writes through to an existing *string target,
// letting two flag names (--server and --url) share one destination. The last of
// the two specified on the command line wins, which is the intuitive behaviour.
type aliasValue struct{ target *string }

func newAliasValue(target *string) *aliasValue { return &aliasValue{target: target} }

func (a *aliasValue) String() string {
	if a.target == nil {
		return ""
	}
	return *a.target
}

func (a *aliasValue) Set(v string) error {
	*a.target = v
	return nil
}

// parseFlags parses args with fs, routing flag/help output to out and normalizing
// the returned error so main can assign the right exit code:
//
//   - help (-h/--help) prints fs's usage to out and returns flag.ErrHelp
//     unchanged, which exitCode maps to a clean exit 0;
//   - any other parse error (unknown flag, bad value) is wrapped as a usageError
//     so it maps to exit 2.
//
// out is the command's own output writer (os.Stdout for a real invocation, a
// buffer under test), so help text can be asserted without mutating the
// process-global os.Stdout. fs.Usage should already be set (via setUsage) before
// calling this.
func parseFlags(fs *flag.FlagSet, out io.Writer, args []string) error {
	// Help output goes to out (it is requested, a success); a genuine parse error
	// also prints usage, which we likewise route to out for consistency since the
	// wrapped error itself is printed to stderr by main.
	fs.SetOutput(out)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return err
		}
		return &usageError{err: err}
	}
	return nil
}

// setUsage installs a usage function on fs that prints a one-line synopsis
// followed by the flag defaults, matching the look of the top-level help. summary
// is the "Usage: ..." line(s); the flag list is appended automatically.
func setUsage(fs *flag.FlagSet, summary string) {
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintln(out, summary)
		if hasFlags(fs) {
			fmt.Fprintln(out, "\nFlags:")
			fs.PrintDefaults()
		}
	}
}

// hasFlags reports whether fs has any registered flags, so setUsage only prints
// the "Flags:" header when there is something to list.
func hasFlags(fs *flag.FlagSet) bool {
	found := false
	fs.VisitAll(func(*flag.Flag) { found = true })
	return found
}

// isHelpArg reports whether s is a help request token. It lets the group routers
// (key/quota/models) treat `agentgpu <group> --help` as a help request for the
// group rather than an unknown subcommand.
func isHelpArg(s string) bool {
	return s == "-h" || s == "--help" || s == "help"
}

// groupHelp prints a group command's usage (e.g. for `agentgpu key --help`) to
// out and returns flag.ErrHelp so the caller propagates a clean exit 0. out is the
// command's own writer (os.Stdout in production, a buffer under test); summary is
// the multi-line usage text for the group.
func groupHelp(out io.Writer, summary string) error {
	fmt.Fprintln(out, summary)
	return flag.ErrHelp
}
