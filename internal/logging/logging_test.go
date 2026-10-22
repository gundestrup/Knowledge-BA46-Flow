package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The file tees only carry writes made after Init — asserting through the
// file avoids capturing process stderr.
func TestInitWritesInfoToFileAndGatesDebug(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kbm.log")
	if err := Init(path, "info"); err != nil {
		t.Fatal(err)
	}
	Infof("hello %s", "world")
	Debugf("hidden %s", "detail")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hello world") {
		t.Fatalf("info line missing from log file: %q", data)
	}
	if strings.Contains(string(data), "hidden detail") {
		t.Fatalf("debug line leaked at info level: %q", data)
	}
}

func TestDebugLevelEnablesDebugf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kbm.log")
	if err := Init(path, "debug"); err != nil {
		t.Fatal(err)
	}
	Debugf("visible %s", "detail")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "DEBUG visible detail") {
		t.Fatalf("debug line missing at debug level: %q", data)
	}
}

func TestInitEmptyPathStaysOnStderr(t *testing.T) {
	if err := Init("", "debug"); err != nil {
		t.Fatal(err)
	}
	// No file opened — nothing to assert beyond the accepted call and the
	// debug gate still applying.
	Debugf("stderr only")
}

func TestInitReportsUnwritablePath(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "missing", "kbm.log"), "info"); err == nil {
		t.Fatal("expected an error for a log_file in a nonexistent directory")
	}
}
