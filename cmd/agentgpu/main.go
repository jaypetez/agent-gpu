// Command agentgpu is the unified single binary for agent-gpu. It exposes the
// server and worker processes plus the operator CLI as subcommands:
//
//	agentgpu server start
//	agentgpu worker start --server host:port
//	agentgpu key create --name <name>            # manage a running server
//	agentgpu key create --name <name> --local --role admin   # offline bootstrap
//	agentgpu quota set <id> --rpm N
//	agentgpu models list
//
// The key/quota/models commands act against a RUNNING server over its public
// HTTP admin API by default (so a revoke or quota change takes effect
// immediately); --local switches key/quota to the on-disk store for offline
// bootstrap. See mode.go for the selection rules and exitCode below for the exit
// codes.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jaypetez/agent-gpu/internal/apiclient"
	"github.com/jaypetez/agent-gpu/internal/version"
)

// Exit codes. Distinct codes let scripts branch on the failure class; they are
// documented in the top-level usage() so the contract is discoverable.
const (
	exitOK       = 0 // success (also a --help request: help is not an error)
	exitError    = 1 // general runtime error not covered by a more specific code
	exitUsage    = 2 // bad invocation: unknown subcommand, missing/invalid flags
	exitAuth     = 3 // authentication/authorization failure (HTTP 401/403)
	exitNotFound = 4 // the target resource does not exist (HTTP 404)
	exitNetwork  = 5 // could not reach the server / transport failure
)

// usageError marks an error as a misuse of the CLI (unknown subcommand, missing
// argument, invalid flag) so main maps it to exitUsage. Its message is printed
// like any other error; the type is the only signal.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// usagef builds a usageError from a printf-style message.
func usagef(format string, args ...any) error {
	return &usageError{err: fmt.Errorf(format, args...)}
}

func main() {
	err := run(os.Args[1:])
	if err == nil {
		os.Exit(exitOK)
	}
	code := exitCode(err)
	// A help request (flag.ErrHelp) already printed usage to the relevant stream
	// and is a success, not an error — exit 0 silently.
	if code == exitOK {
		os.Exit(exitOK)
	}
	fmt.Fprintln(os.Stderr, "agentgpu:", err)
	os.Exit(code)
}

// exitCode maps an error returned by run to a process exit code. flag.ErrHelp is
// success (a help request); a usageError is exitUsage; the apiclient's typed
// status errors map to their dedicated codes; everything else is exitError. The
// network code is inferred from an apiclient request failure that is not one of
// the typed status classes.
func exitCode(err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, flag.ErrHelp):
		return exitOK
	}
	var ue *usageError
	if errors.As(err, &ue) {
		return exitUsage
	}
	switch {
	case errors.Is(err, apiclient.ErrUnauthorized), errors.Is(err, apiclient.ErrForbidden):
		return exitAuth
	case errors.Is(err, apiclient.ErrNotFound):
		return exitNotFound
	}
	// A transport failure surfaces as a non-typed apiclient error: it is neither a
	// usageError nor one of the status sentinels above, and an *APIError (a real
	// HTTP response) would have matched a sentinel or be a 4xx/5xx server fault.
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) {
		// A decoded HTTP response that is not 401/403/404 (e.g. 400, 5xx) is a
		// server-side fault, not a local misuse: report it as a general error.
		return exitError
	}
	if isNetworkError(err) {
		return exitNetwork
	}
	return exitError
}

func run(args []string) error {
	if len(args) < 1 {
		usage(os.Stderr)
		return usagef("expected a subcommand")
	}

	// Informational commands first: they print to stdout and exit without setting
	// up signal handling, logging, or any subsystem.
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintln(os.Stdout, version.String())
		return nil
	case "-h", "--help", "help":
		// A help request is a success and prints to stdout (so it can be piped),
		// unlike the error-path usage which goes to stderr.
		usage(os.Stdout)
		return nil
	}

	return dispatch(args)
}

// usage prints the top-level help to w. It is written to stdout for an explicit
// help request and to stderr on a usage error, so a help request can be piped
// while errors stay on stderr.
func usage(w io.Writer) {
	fmt.Fprint(w, `agentgpu — distributed inference layer for Ollama

Usage:
  agentgpu server start [--listen host:port] [--http-listen host:port] [flags]
  agentgpu worker start --server host:port [--id worker-id] [flags]
  agentgpu key    <create|list|revoke|rotate|perms|quota> [flags]
  agentgpu quota  <set|show> <id> [flags]
  agentgpu models list [--json|--openai]
  agentgpu loadtest [--mode remote|inproc] [flags]
  agentgpu version

The key, quota, and models commands act against a RUNNING server over its public
HTTP admin API, so changes (revoke, quota updates) take effect immediately. Pass
an admin token via --token or AGENTGPU_TOKEN and the server URL via --server or
AGENTGPU_HTTP_ADDR (default http://127.0.0.1:8080).

  agentgpu key create --name app --token <admin-token>
  agentgpu key revoke <id> --token <admin-token>
  agentgpu quota set <id> --rpm 60 --token <admin-token>
  agentgpu models list --token <admin-token>

Offline bootstrap (no server running): mint the first admin key directly into the
on-disk store with --local, then start the server (it loads the store at boot):

  agentgpu key create --name bootstrap --role admin --local
  # ...start the server, then export AGENTGPU_TOKEN=<that token>...

Note: --local writes the store file; a running server does not see the change
until it is restarted. Use the HTTP mode (a --token) to manage a live server.

Global configuration (flag > environment > default):
  --server / --url   AGENTGPU_HTTP_ADDR   HTTP API base URL (default http://127.0.0.1:8080)
  --token            AGENTGPU_TOKEN       admin Bearer token for the HTTP API
  --store            AGENTGPU_STORE_PATH  on-disk keys file (used with --local)

Server/worker configuration may also be supplied via environment variables:
  AGENTGPU_SERVER_LISTEN, AGENTGPU_HTTP_LISTEN, AGENTGPU_SERVER_ADDR,
  AGENTGPU_WORKER_ID, AGENTGPU_STORE_PATH, AGENTGPU_QUOTA_PATH

Exit codes:
  0  success (including --help)
  1  general runtime error
  2  usage error (unknown command, missing or invalid flags)
  3  authentication or authorization failure (401/403)
  4  resource not found (404)
  5  could not reach the server (network/transport failure)
`)
}
