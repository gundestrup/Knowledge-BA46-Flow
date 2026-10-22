package report

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"
)

const gemLatestTTL = 24 * time.Hour

func emptyFleetSummary() FleetSummary {
	return FleetSummary{
		GemDrift:    map[string]GemDrift{},
		GemDriftOK:  map[string]AllowedGemDrift{},
		GemOutdated: map[string]GemOutdated{},
		GemHeld:     map[string]GemHold{},
		GemStale:    map[string]GemStaleness{},
		NPM:         emptyNPMFleetSummary(),
	}
}

func fleetGemVersions(repos []Repository) map[string]map[string][]string {
	versions := map[string]map[string][]string{}
	for _, repo := range repos {
		for gem, version := range repo.Gems {
			addFleetLabel(versions, gem, version, repo.Name)
		}
	}
	sortFleetVersions(versions)
	return versions
}

func buildFleet(repos []Repository, policy policyFile, latest map[string]GemLatest, now time.Time) FleetSummary {
	fleet := emptyFleetSummary()
	versions := fleetGemVersions(repos)
	for gem, byVersion := range versions {
		recordGemDrift(&fleet, policy, gem, byVersion)
		recordGemOutdated(&fleet, repos, latest, policy, gem, byVersion)
	}
	for gem, byVersion := range versions {
		recordGemStaleness(&fleet, latest, policy, now, gem, byVersion)
	}
	return fleet
}

// recordGemDrift files gems installed at more than one version under GemDrift,
// or under GemDriftOK when policy allows the split.
func recordGemDrift(fleet *FleetSummary, policy policyFile, gem string, byVersion map[string][]string) {
	if len(byVersion) <= 1 {
		return
	}
	if reason, allowed := policy.Gems.DriftOK[gem]; allowed {
		fleet.GemDriftOK[gem] = AllowedGemDrift{Versions: byVersion, Reason: reason}
		return
	}
	versionNames := sortedVersionNames(byVersion)
	fleet.GemDrift[gem] = GemDrift{
		Versions: byVersion,
		Severity: gemVersionSpreadSeverity(versionNames),
	}
}

// recordGemOutdated files gems whose newest installed version trails the
// latest upstream release.
func recordGemOutdated(fleet *FleetSummary, repos []Repository, latest map[string]GemLatest, policy policyFile, gem string, byVersion map[string][]string) {
	record, known := latest[gem]
	if !known || record.Version == "" {
		return
	}
	latestVersion, ok := parseGemVersion(record.Version)
	if !ok {
		return
	}
	newest := latestInstalledVersion(sortedVersionNames(byVersion))
	if newest == nil || compareGemVersions(latestVersion, *newest) <= 0 {
		return
	}
	blockedBy := gemUpgradeBlockers(repos, gem, byVersion, latestVersion)
	if reason, held := policy.Gems.Hold[gem]; held {
		fleet.GemHeld[gem] = GemHold{
			Latest:    record.Version,
			Versions:  byVersion,
			Reason:    reason,
			BlockedBy: blockedBy,
		}
		return
	}
	fleet.GemOutdated[gem] = GemOutdated{
		Latest:    record.Version,
		Versions:  byVersion,
		Severity:  gemOutdatedSeverity(*newest, latestVersion, blockedBy),
		BlockedBy: blockedBy,
	}
}

// gemOutdatedSeverity ranks how far the newest installed version lags: pinned
// by a dependency constraint beats everything; patch-level drift is "info",
// major/minor drift "dim" and same-segment bumps "warn".
func gemOutdatedSeverity(newest, latest gemVersion, blockedBy []string) string {
	if len(blockedBy) > 0 {
		return "constrained"
	}
	switch segment := versionDifferenceSegment(newest, latest); {
	case segment == 1:
		return "info"
	case segment >= 2:
		return "dim"
	default:
		return "warn"
	}
}

// recordGemStaleness files gems whose latest upstream release is at least a
// year old, honoring dormant_ok policy reviews.
func recordGemStaleness(fleet *FleetSummary, latest map[string]GemLatest, policy policyFile, now time.Time, gem string, byVersion map[string][]string) {
	record, ok := latest[gem]
	if !ok || record.Released == "" {
		return
	}
	released, err := time.Parse(time.RFC3339, record.Released)
	if err != nil {
		return
	}
	ageMonths := int(math.Round(now.Sub(released).Hours() / (24 * 30)))
	if ageMonths < 12 {
		return
	}
	severity := gemAgeSeverity(ageMonths)
	note := ""
	if reviewed, allowed := policy.Gems.DormantOK[gem]; allowed {
		var stillOK bool
		stillOK, note = dormantReviewStatus(reviewed, now)
		if stillOK {
			severity = "ok"
		}
	}
	fleet.GemStale[gem] = GemStaleness{
		Latest: record.Version, AgeMonths: ageMonths, Versions: byVersion,
		Severity: severity, Note: note,
	}
}

// gemAgeSeverity grades latest-release age: dim under 24 months, info under
// 48, warn beyond.
func gemAgeSeverity(ageMonths int) string {
	if ageMonths >= 48 {
		return "warn"
	}
	if ageMonths >= 24 {
		return "info"
	}
	return "dim"
}

// dormantReviewStatus evaluates a dormant_ok entry: it reports stillOK=false
// with an overdue note when the recorded review has passed its review_after
// window, and stillOK=true with the policy reason otherwise.
func dormantReviewStatus(reviewed DormantGemOK, now time.Time) (stillOK bool, note string) {
	months := reviewed.ReviewAfter
	if months <= 0 {
		months = 6
	}
	reviewedAt, parseErr := time.Parse("2006-01-02", reviewed.Reviewed)
	if parseErr == nil && dateAfter(now, addMonthsClamped(reviewedAt, months)) {
		return false, fmt.Sprintf("dormant_ok review overdue (verified %s)", reviewed.Reviewed)
	}
	return true, reviewed.Reason
}

func sortedVersionNames(versions map[string][]string) []string {
	names := slices.Collect(maps.Keys(versions))
	sortVersionStrings(names, parseGemVersion, compareGemVersions)
	return names
}

func latestInstalledVersion(versions []string) *gemVersion {
	var newest *gemVersion
	for _, raw := range versions {
		version, ok := parseGemVersion(raw)
		if !ok {
			continue
		}
		if newest == nil || compareGemVersions(version, *newest) > 0 {
			current := version
			newest = &current
		}
	}
	return newest
}

func gemUpgradeBlockers(repos []Repository, gem string, versions map[string][]string, latest gemVersion) []string {
	repositoryNames := map[string]bool{}
	for _, names := range versions {
		for _, name := range names {
			repositoryNames[name] = true
		}
	}
	blocked := []string{}
	for _, repo := range repos {
		if !repositoryNames[repo.Name] {
			continue
		}
		for _, edge := range repo.GemEdges[gem] {
			if !gemRequirementSatisfied(edge.Req, latest) {
				blocked = append(blocked, fmt.Sprintf("%s %s (%s)", edge.By, edge.Req, repo.Name))
			}
		}
	}
	return blocked
}

func versionDifferenceSegment(old, latest gemVersion) int {
	length := len(old.segments)
	if len(latest.segments) > length {
		length = len(latest.segments)
	}
	for i := 0; i < length; i++ {
		oldPart, latestPart := 0, 0
		if i < len(old.segments) {
			oldPart = old.segments[i]
		}
		if i < len(latest.segments) {
			latestPart = latest.segments[i]
		}
		if oldPart != latestPart {
			return i
		}
	}
	return 0
}

func addMonthsClamped(date time.Time, months int) time.Time {
	if months == 0 {
		return date
	}
	first := time.Date(date.Year(), date.Month(), 1, date.Hour(), date.Minute(), date.Second(), date.Nanosecond(), date.Location())
	target := first.AddDate(0, months, 0)
	lastDay := time.Date(target.Year(), target.Month()+1, 0, 0, 0, 0, 0, target.Location()).Day()
	day := date.Day()
	if day > lastDay {
		day = lastDay
	}
	return time.Date(target.Year(), target.Month(), day, date.Hour(), date.Minute(), date.Second(), date.Nanosecond(), date.Location())
}

func dateAfter(left, right time.Time) bool {
	leftDate := time.Date(left.Year(), left.Month(), left.Day(), 0, 0, 0, 0, left.Location())
	rightDate := time.Date(right.Year(), right.Month(), right.Day(), 0, 0, 0, 0, right.Location())
	return leftDate.After(rightDate)
}

func gemNames(values []string) string {
	return strings.Join(values, "/")
}
