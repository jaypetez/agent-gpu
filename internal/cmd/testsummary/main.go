// Command testsummary turns the `go test -json` event stream on stdin into a
// compact, human-legible pass/fail summary on stdout, and exits non-zero if any
// test failed (or the stream reported a build/setup error). It is the small,
// dependency-free formatter behind `make test-e2e` / `make test-all` (#105): the
// raw JSON is tee'd to an artifact for machines, while this renders the same run
// for a human (and an agent) to read a failure and fix it in one loop iteration.
//
// It is deliberately stdlib-only and reads from stdin so it composes in a pipe:
//
//	go test -json ./... | tee test-results/go-test.json | go run ./internal/cmd/testsummary
//
// With `set -o pipefail` the pipeline's exit status reflects `go test` too, so a
// failure is never masked even if this tool somehow exited 0.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// event is one line of `go test -json` output. Only the fields we summarise are
// decoded; the rest are ignored. See `go doc test2json`.
type event struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
	Output  string `json:"Output"`
}

func main() {
	os.Exit(run(os.Stdin, os.Stdout))
}

func run(in *os.File, out *os.File) int {
	sc := bufio.NewScanner(in)
	// go test -json can emit long output lines (e.g. a panic stack); raise the
	// scanner's token cap so we don't truncate mid-line.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var passed, failed, skipped int
	// failedTests collects "pkg.Test" identifiers, and failedNoTest collects
	// package-level failures with no associated test (build/setup errors), so the
	// summary names exactly what to look at.
	var failedTests []string
	failedNoTest := map[string]bool{}
	// output buffers the per-test output so a failing test can print its own lines
	// (the assertion message), keyed by "pkg\x00Test".
	output := map[string][]string{}
	var nonJSON []string

	for sc.Scan() {
		line := sc.Bytes()
		var ev event
		if err := json.Unmarshal(line, &ev); err != nil {
			// Not a JSON event — typically a leading build error. Keep it so we can
			// surface it (and treat the run as failed).
			text := strings.TrimRight(string(line), "\r\n")
			if strings.TrimSpace(text) != "" {
				nonJSON = append(nonJSON, text)
			}
			continue
		}
		key := ev.Package + "\x00" + ev.Test
		switch ev.Action {
		case "output":
			if ev.Test != "" {
				output[key] = append(output[key], ev.Output)
			}
		case "pass":
			if ev.Test != "" {
				passed++
			}
		case "skip":
			if ev.Test != "" {
				skipped++
			}
		case "fail":
			if ev.Test != "" {
				failed++
				failedTests = append(failedTests, ev.Package+"."+ev.Test)
			} else {
				// A package-level fail with no test means the package failed to
				// build or a TestMain/setup failed — record it.
				failedNoTest[ev.Package] = true
			}
		}
	}

	// Print the output of each failed test so the assertion message is right here.
	if len(failedTests) > 0 {
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "--- FAILED TESTS ---")
		sort.Strings(failedTests)
		for _, ft := range failedTests {
			fmt.Fprintf(out, "FAIL: %s\n", ft)
			// ft is "pkg.Test"; recover the key to print its buffered output.
			i := strings.LastIndex(ft, ".")
			if i > 0 {
				pkg, test := ft[:i], ft[i+1:]
				for _, ln := range output[pkg+"\x00"+test] {
					trimmed := strings.TrimRight(ln, "\n")
					if strings.TrimSpace(trimmed) != "" {
						fmt.Fprintf(out, "    %s\n", strings.TrimSpace(trimmed))
					}
				}
			}
		}
	}

	// Package-level build/setup failures (no test name).
	var pkgFails []string
	for pkg := range failedNoTest {
		pkgFails = append(pkgFails, pkg)
	}
	if len(pkgFails) > 0 {
		sort.Strings(pkgFails)
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "--- PACKAGE FAILURES (build/setup) ---")
		for _, pkg := range pkgFails {
			fmt.Fprintf(out, "FAIL: %s\n", pkg)
		}
	}

	// Leading non-JSON lines (build errors) usually mean the whole run is broken.
	if len(nonJSON) > 0 {
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "--- NON-JSON OUTPUT (likely a build error) ---")
		for _, ln := range nonJSON {
			fmt.Fprintln(out, ln)
		}
	}

	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "go test summary: %d passed, %d failed, %d skipped\n", passed, failed, skipped)

	if failed > 0 || len(pkgFails) > 0 || len(nonJSON) > 0 {
		return 1
	}
	return 0
}
