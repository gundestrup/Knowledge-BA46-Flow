// Package logging is the app's single log destination: stderr always, plus a
// configured log_file (appended) when set. log_level "debug" turns on
// verbose request/job tracing; the default "info" keeps output quiet.
package logging

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync/atomic"
)

var (
	std   = log.New(os.Stderr, "", log.LstdFlags)
	debug atomic.Bool
)

// Init applies the level and, when path is non-empty, tees output to the
// file (created/appended, mode 0644) while keeping stderr. Re-calling Init
// re-applies settings — repeated in-process command runs in tests stay
// predictable.
func Init(path, level string) error {
	debug.Store(level == "debug")
	if path == "" {
		std.SetOutput(os.Stderr)
		return nil
	}
	// 0600: the log can carry browser stack traces and job detail.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log_file %s: %w", path, err)
	}
	std.SetOutput(io.MultiWriter(os.Stderr, f))
	Infof("logging to %s", path)
	return nil
}

// Debugf logs at debug level — gated by log_level "debug" in config.
func Debugf(format string, args ...any) {
	if debug.Load() {
		std.Printf("DEBUG "+format, args...)
	}
}

// Infof logs unconditionally.
func Infof(format string, args ...any) {
	std.Printf(format, args...)
}
