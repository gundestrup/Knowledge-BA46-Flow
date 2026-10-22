package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanFlagsSuspiciousOutputInPassingTests(t *testing.T) {
	stream := `{"Package":"example/pkg","Test":"TestFine","Action":"output","Output":"=== RUN   TestFine\n"}
{"Package":"example/pkg","Test":"TestFine","Action":"output","Output":"2026/10/21 WARNING: could not record job: sql: database is closed\n"}
{"Package":"example/pkg","Test":"TestFine","Action":"pass"}
{"Package":"example/pkg","Test":"TestFine","Action":"output","Output":"panic: runtime error\n"}
{"Package":"example/pkg","Action":"output","Output":"PASS\n"}
`
	findings, _, err := scan(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 2 {
		t.Fatalf("findings=%+v, want 2 suspicious lines", findings)
	}
	if findings[0].where != "example/pkg.TestFine" {
		t.Fatalf("finding attributed to %q", findings[0].where)
	}
}

func TestScanHonorsAllowlist(t *testing.T) {
	stream := `{"Package":"knowledge-ba46-flow/internal/server","Test":"TestClientLogEndpoint","Action":"output","Output":"2026/10/21 client error: intentional\n"}
{"Package":"knowledge-ba46-flow/internal/server","Test":"TestClientLogEndpoint","Action":"pass"}
`
	findings, _, err := scan(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("allowlisted output was flagged: %+v", findings)
	}
}

func TestRunExitCodes(t *testing.T) {
	clean := `{"Package":"p","Test":"TestX","Action":"pass"}
`
	dirty := `{"Package":"p","Test":"TestX","Action":"output","Output":"panic: boom\n"}
{"Package":"p","Test":"TestX","Action":"pass"}
`
	var out strings.Builder
	if code := run(nil, strings.NewReader(clean), &out); code != 0 {
		t.Errorf("clean stdin: code=%d, want 0", code)
	}
	if !strings.Contains(out.String(), "audit clean") {
		t.Errorf("clean run should report clean, got %q", out.String())
	}
	out.Reset()
	if code := run(nil, strings.NewReader(dirty), &out); code != 1 {
		t.Errorf("dirty stdin: code=%d, want 1", code)
	}
	if !strings.Contains(out.String(), "Suspicious output") {
		t.Errorf("dirty run should list findings, got %q", out.String())
	}
	// -file reads a captured stream instead of stdin (the CI path).
	path := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(path, []byte(dirty), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"-file", path}, strings.NewReader(clean), &out); code != 1 {
		t.Errorf("-file with findings: code=%d, want 1", code)
	}
	if code := run([]string{"-file", filepath.Join(t.TempDir(), "absent.json")}, nil, &out); code != 1 {
		t.Errorf("missing -file: code=%d, want 1", code)
	}
	if code := run([]string{"-bogus-flag"}, nil, &out); code != 2 {
		t.Errorf("bad flag: code=%d, want 2", code)
	}
}

func TestScanCountsFailuresAndSkipsNoise(t *testing.T) {
	stream := `{"Package":"p","Test":"TestX","Action":"output","Output":"ordinary output\n"}
{"Package":"p","Test":"TestX","Action":"fail"}
not json at all
{"Package":"p","Action":"output","Output":"ok  \tp\t0.1s\n"}
`
	findings, failures, err := scan(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 || failures != 1 {
		t.Fatalf("findings=%v failures=%d", findings, failures)
	}
}
