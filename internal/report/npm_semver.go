package report

import (
	"fmt"
	"strconv"
	"strings"

	npm "github.com/aquasecurity/go-npm-version/pkg"
	"github.com/aquasecurity/go-version/pkg/part"
)

// npmVersion wraps the aquasecurity npm version implementation. Exact pins
// keep major/minor/patch copies for the fleet diff and drift checks.
type npmVersion struct {
	version npm.Version
	major   uint64
	minor   uint64
	patch   uint64
}

type npmPartial struct {
	major     uint64
	minor     uint64
	patch     uint64
	specified int
	wildcard  bool
}

func parseNPMVersion(raw string) (npmVersion, bool) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "v")
	raw = strings.SplitN(raw, "+", 2)[0]
	if raw == "" {
		return npmVersion{}, false
	}
	parsed, err := npm.NewVersion(raw)
	if err != nil {
		return npmVersion{}, false
	}
	major, majorOK := parsed.Major().(part.Uint64)
	minor, minorOK := parsed.Minor().(part.Uint64)
	patch, patchOK := parsed.Patch().(part.Uint64)
	if !majorOK || !minorOK || !patchOK {
		return npmVersion{}, false
	}
	return npmVersion{
		version: parsed,
		major:   uint64(major),
		minor:   uint64(minor),
		patch:   uint64(patch),
	}, true
}

func compareNPMVersions(left, right npmVersion) int {
	return left.version.Compare(right.version)
}

func npmVersionString(version npmVersion) string {
	return version.version.String()
}

func npmRangeSatisfies(expression, candidate string) bool {
	version, ok := parseNPMVersion(candidate)
	if !ok {
		partial, partialOK := parseNPMPartial(candidate)
		if !partialOK {
			return false
		}
		version, ok = parseNPMVersion(
			fmt.Sprintf("%d.%d.%d", partial.major, partial.minor, partial.patch))
		if !ok {
			return false
		}
	}
	for _, alternative := range strings.Split(expression, "||") {
		if npmRangeAlternativeSatisfies(alternative, version) {
			return true
		}
	}
	return false
}

// npmRangeAlternativeSatisfies keeps the previous lenient behaviour: range
// fragments the npm parser cannot understand do not block an upgrade report.
func npmRangeAlternativeSatisfies(expression string, version npmVersion) bool {
	expression = strings.TrimSpace(strings.ReplaceAll(expression, ",", " "))
	if expression == "" || expression == "*" || strings.EqualFold(expression, "x") {
		return true
	}
	if fields := strings.Fields(expression); len(fields) == 3 && fields[1] == "-" {
		lower, lowOK := parseNPMVersion(fields[0])
		upper, highOK := parseNPMVersion(fields[2])
		if !lowOK || !highOK {
			return false
		}
		expression = ">=" + npmVersionString(lower) + " <=" + npmVersionString(upper)
	}
	constraints, err := npm.NewConstraints(expression, npm.WithPreRelease(true))
	if err != nil {
		return true
	}
	return constraints.Check(version.version)
}

// parseNPMPartial validates node pins such as `22` or `22.x` that carry an
// incomplete or wildcard version rather than a full semantic version.
func parseNPMPartial(raw string) (npmPartial, bool) {
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "v")
	raw = strings.SplitN(raw, "+", 2)[0]
	if strings.Contains(raw, "-") {
		if version, ok := parseNPMVersion(raw); ok {
			return npmPartial{
				major: version.major, minor: version.minor, patch: version.patch, specified: 3,
			}, true
		}
		return npmPartial{}, false
	}
	parts := strings.Split(raw, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return npmPartial{}, false
	}
	result := npmPartial{}
	for i, part := range parts {
		if part == "" || strings.EqualFold(part, "x") || part == "*" {
			result.wildcard = true
			break
		}
		if len(part) > 1 && part[0] == '0' {
			return npmPartial{}, false
		}
		number, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return npmPartial{}, false
		}
		result.specified++
		switch i {
		case 0:
			result.major = number
		case 1:
			result.minor = number
		case 2:
			result.patch = number
		}
	}
	if result.specified == 0 && !result.wildcard {
		return npmPartial{}, false
	}
	return result, true
}

func npmVersionSpreadSeverity(versions []string) string {
	series := map[string]bool{}
	for _, raw := range versions {
		version, ok := parseNPMVersion(raw)
		if !ok {
			continue
		}
		series[strconv.FormatUint(version.major, 10)+"."+strconv.FormatUint(version.minor, 10)] = true
	}
	if len(series) > 1 {
		return "warn"
	}
	return "info"
}

func npmVersionDifference(old, latest npmVersion) int {
	switch {
	case old.major != latest.major:
		return 0
	case old.minor != latest.minor:
		return 1
	default:
		return 2
	}
}
