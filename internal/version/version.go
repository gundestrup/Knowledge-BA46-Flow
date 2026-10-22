// Package version contains build metadata for the KBM-flow CLI.
package version

// Version is injected from the root VERSION file by Makefile and Docker builds.
// Direct `go run` and unconfigured builds report "dev".
var Version = "dev"
