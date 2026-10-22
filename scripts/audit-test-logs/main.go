// Command audit-test-logs scans `go test -json` output for errors that pass
// silently: panics, WARNING lines and "error"-style log writes inside
// otherwise-passing tests never surface in normal `go test` output (passing
// packages' output is buffered and discarded). Intended error-path noise is
// allowlisted below — deliberate review, not suppression.
//
// Usage:
//
//	go test -json ./... | go run ./scripts/audit-test-logs
//	go run ./scripts/audit-test-logs -file test-report.json   (CI audits the
//	stream `make coverage-ci` already produces)
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
)

// suspicious matches log lines that indicate something broke inside a test
// that still reported pass.
var suspicious = regexp.MustCompile(`(?i)panic:|WARNING:|client error|client stack|FATAL|DATA RACE|fatal error|goroutine \d+ \[`)

// allowlist covers tests that emit error output on purpose — a test
// asserting a failure path legitimately logs. Entries match the fully
// qualified "pkg.TestName" of the emitting test ("pkg." for package-level
// lines).
var allowlist = []*regexp.Regexp{
	regexp.MustCompile(`TestClientLogEndpoint`),                    // posts a client error on purpose — the relay writes it to stderr by design
	regexp.MustCompile(`TestPreviousSnapshotDegradesOnCorruption`), // asserts the corrupt-snapshot WARNING is emitted — the warning is the feature
}

type event struct {
	Package string `json:"Package"`
	Test    string `json:"Test"`
	Action  string `json:"Action"`
	Output  string `json:"Output"`
}

type finding struct {
	where, line string
}

// scan reads a go test -json stream and returns non-allowlisted suspicious
// output lines plus the count of failed tests.
func scan(in io.Reader) ([]finding, int, error) {
	var findings []finding
	failures := 0
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		var ev event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Action == "fail" && ev.Test != "" {
			failures++
		}
		if ev.Action != "output" || ev.Output == "" || !suspicious.MatchString(ev.Output) {
			continue
		}
		where := ev.Package + "." + ev.Test
		allowed := false
		for _, re := range allowlist {
			if re.MatchString(where) {
				allowed = true
				break
			}
		}
		if !allowed {
			findings = append(findings, finding{where: where, line: ev.Output})
		}
	}
	return findings, failures, sc.Err()
}

func run(args []string, stdin io.Reader, stdout io.Writer) int {
	fs := flag.NewFlagSet("audit-test-logs", flag.ContinueOnError)
	var file string
	fs.StringVar(&file, "file", "", "read a captured go test -json stream instead of stdin")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	in := stdin
	if file != "" {
		f, err := os.Open(file)
		if err != nil {
			fmt.Fprintln(os.Stderr, "audit-test-logs:", err)
			return 1
		}
		defer f.Close()
		in = f
	}

	findings, failures, err := scan(in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "audit-test-logs:", err)
		return 1
	}
	if len(findings) > 0 {
		fmt.Fprintln(stdout, "Suspicious output inside otherwise-passing tests:")
		for _, f := range findings {
			fmt.Fprintf(stdout, "  %s\n    %s\n", f.where, f.line)
		}
		fmt.Fprintf(stdout, "\n%d unexpected log line(s). Intended error-path noise belongs in the allowlist in scripts/audit-test-logs.\n", len(findings))
		return 1
	}
	if failures > 0 {
		fmt.Fprintf(stdout, "Test-log audit clean (%d test failure(s) — see test output).\n", failures)
	} else {
		fmt.Fprintln(stdout, "Test-log audit clean.")
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout))
}
