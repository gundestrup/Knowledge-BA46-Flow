package version

import "testing"

// Tests run without -ldflags injection, so the variable must keep its "dev"
// default; make/docker builds override it with the VERSION file contents.
func TestVersionDefaultsToDev(t *testing.T) {
	if Version != "dev" {
		t.Fatalf("Version=%q, expected the dev default outside a release build", Version)
	}
}
