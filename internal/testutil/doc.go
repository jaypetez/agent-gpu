// Package testutil provides shared test fixtures for agent-gpu's unit and
// integration tests: functional-options builders for the core domain types
// ([types.Job], [store.APIKey], [types.Worker], [types.Heartbeat]), a key-minting
// helper over an [auth.Service], and a single configurable fake [worker.Executor].
//
// It exists to retire the fixtures that were copy-pasted across the test suite —
// hand-built jobs, online-worker constructors, key minting, and three near-identical
// fake executors — so a test states only what it cares about and inherits sane
// defaults for everything else.
//
// # Test-only
//
// This package is imported ONLY from *_test.go files. It depends on production
// packages (types, store, auth, authz, worker) and takes *testing.T in its
// minting helpers; production code must never import it, which keeps it off the
// shipped binary and free of import cycles.
//
// White-box internal test files (package server, package httpapi, …) that assert
// on UNEXPORTED types cannot use these builders and are deliberately left alone;
// the builders serve the black-box _test packages that exercise the public surface.
//
// # Builders
//
// Every builder applies its defaults first, then the supplied options in order, so
// later options win. The defaults are chosen so the zero-argument form returns a
// valid, dispatchable value:
//
//	job := testutil.Job()                       // valid: stable ID, Model "llama3", Prompt "hi"
//	chat := testutil.Job(testutil.WithMessages( // a chat job carrying a conversation
//		testutil.UserMessage("hello")))
//	w := testutil.Worker(testutil.WithWorkerModels("llama3")) // an online worker serving llama3
//	key, token := testutil.MintKey(t, svc,      // a real, authenticatable user key
//		testutil.WithRoles(authz.RoleUser), testutil.WithAllowModels("llama3"))
package testutil
