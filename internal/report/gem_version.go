package report

import (
	"strconv"
	"strings"

	gem "github.com/aquasecurity/go-gem-version"
)

// gemVersion wraps the aquasecurity RubyGems version implementation and keeps
// the numeric release segments the fleet report needs for diff severity.
type gemVersion struct {
	version  gem.Version
	segments []int
}

func parseGemVersion(raw string) (gemVersion, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return gemVersion{}, false
	}
	parsed, err := gem.NewVersion(raw)
	if err != nil {
		return gemVersion{}, false
	}
	version := gemVersion{version: parsed}
	for _, segment := range strings.Split(parsed.Release().String(), ".") {
		number, err := strconv.Atoi(segment)
		if err != nil {
			break
		}
		version.segments = append(version.segments, number)
	}
	return version, true
}

func compareGemVersions(left, right gemVersion) int {
	return left.version.Compare(right.version)
}

// gemRequirementSatisfied keeps the previous lenient behaviour: requirements
// the parser cannot understand must not block an upgrade report.
func gemRequirementSatisfied(requirement string, version gemVersion) bool {
	if strings.TrimSpace(requirement) == "" {
		return true
	}
	constraints, err := gem.NewConstraints(requirement)
	if err != nil {
		return true
	}
	return constraints.Check(version.version)
}

func gemVersionSpreadSeverity(versions []string) string {
	minors := map[string]bool{}
	for _, version := range versions {
		parts := strings.Split(version, ".")
		if len(parts) > 2 {
			parts = parts[:2]
		}
		minors[strings.Join(parts, ".")] = true
	}
	if len(minors) > 1 {
		return "warn"
	}
	return "info"
}
