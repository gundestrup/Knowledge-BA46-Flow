package report

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

const npmLatestTTL = 24 * time.Hour

type npmPackument struct {
	DistTags map[string]string `json:"dist-tags"`
}

func emptyNPMFleetSummary() NPMFleetSummary {
	return NPMFleetSummary{
		NodeVersionDrift: map[string][]string{},
		NodeOutdated:     map[string]NodeOutdated{},
		PackageDrift:     map[string]NPMDrift{},
		PackageDriftOK:   map[string]AllowedNPMDrift{},
		PackageOutdated:  map[string]NPMOutdated{},
		PackageHeld:      map[string]NPMHold{},
	}
}

func markNPMLatestStale(cache map[string]NPMLatest) {
	for name, entry := range cache {
		entry.Stale = true
		cache[name] = entry
	}
}

func npmProjectLabel(repoName string, project NPMProject) string {
	if project.Path == "" || project.Path == "." {
		return repoName
	}
	return repoName + "/" + project.Path
}

func fleetNPMVersions(repos []Repository) map[string]map[string][]string {
	versions := map[string]map[string][]string{}
	for _, repo := range repos {
		for _, project := range repo.NPMProjects {
			label := npmProjectLabel(repo.Name, project)
			for name, version := range project.Packages {
				addFleetLabel(versions, name, version, label)
			}
		}
	}
	sortFleetVersions(versions)
	return versions
}

func refreshNPMLatest(
	ctx context.Context,
	client *remoteHTTP,
	installed map[string]map[string][]string,
	existing map[string]NPMLatest,
	force bool,
) map[string]NPMLatest {
	result := make(map[string]NPMLatest, len(existing)+len(installed))
	for name, cached := range existing {
		result[name] = cached
	}
	now := time.Now().UTC()
	names := make([]string, 0, len(installed))
	for name := range installed {
		cached, ok := existing[name]
		if force || !ok || npmCacheStale(cached, now) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	var group errgroup.Group
	var mu sync.Mutex
	group.SetLimit(min(8, len(names)))
	for _, name := range names {
		if ctx.Err() != nil {
			break
		}
		group.Go(func() error {
			if ctx.Err() != nil {
				return nil
			}
			entry, err := fetchNPMLatest(ctx, client, name, now)
			mu.Lock()
			result[name] = npmLatestOutcome(result[name], entry, err, now)
			mu.Unlock()
			return nil
		})
	}
	_ = group.Wait()
	return result
}

// npmLatestOutcome merges a fetch result into the cache: a successful fetch
// wins, a failed one marks the cached entry stale (or records a stale marker
// when the package was never cached).
func npmLatestOutcome(cached, fetched NPMLatest, fetchErr error, now time.Time) NPMLatest {
	if fetchErr == nil {
		return fetched
	}
	failedAt := now.Format(time.RFC3339)
	if cached.CheckedAt == "" && cached.FailedAt == "" && cached.Version == "" {
		return NPMLatest{FailedAt: failedAt, Stale: true}
	}
	cached.Stale = true
	cached.FailedAt = failedAt
	return cached
}

func npmCacheStale(entry NPMLatest, now time.Time) bool {
	if entry.Stale {
		if entry.FailedAt == "" {
			return true
		}
		failed, err := time.Parse(time.RFC3339, entry.FailedAt)
		if err != nil || now.Sub(failed) >= time.Hour {
			return true
		}
		return false
	}
	if entry.CheckedAt == "" {
		return true
	}
	checked, err := time.Parse(time.RFC3339, entry.CheckedAt)
	return err != nil || now.Sub(checked) > npmLatestTTL
}

func fetchNPMLatest(ctx context.Context, client *remoteHTTP, name string, now time.Time) (NPMLatest, error) {
	target := client.npmRegistryBase + "/" + url.PathEscape(name)
	var packument npmPackument
	if err := client.getJSONAccept(ctx, target, requestAuth{}, "application/vnd.npm.install-v1+json", &packument); err != nil {
		return NPMLatest{}, err
	}
	return NPMLatest{
		Version:   packument.DistTags["latest"],
		CheckedAt: now.Format(time.RFC3339),
		Available: packument.DistTags["latest"] != "",
	}, nil
}

func buildNPMFleet(
	repos []Repository,
	policy policyFile,
	latest map[string]NPMLatest,
	nodeReleases NodeReleaseCache,
) NPMFleetSummary {
	fleet := emptyNPMFleetSummary()
	collectNodeOutdated(repos, nodeReleases, &fleet)
	for name, byVersion := range fleetNPMVersions(repos) {
		recordPackageDrift(name, byVersion, policy, &fleet)
		recordPackageOutdated(repos, name, byVersion, latest, policy, &fleet)
	}
	return fleet
}

func collectNodeOutdated(repos []Repository, releases NodeReleaseCache, fleet *NPMFleetSummary) {
	nodeVersions := map[string][]string{}
	for _, repo := range repos {
		for _, project := range repo.NPMProjects {
			if project.NodeVersion == "" {
				continue
			}
			version := normalizeNodePin(project.NodeVersion)
			label := npmProjectLabel(repo.Name, project)
			nodeVersions[version] = appendUnique(nodeVersions[version], label)
			if outdated := nodeOutdatedEntry(version, label, releases); outdated != nil {
				fleet.NodeOutdated[label] = *outdated
			}
		}
	}
	for version := range nodeVersions {
		slices.Sort(nodeVersions[version])
	}
	if len(nodeVersions) > 1 {
		fleet.NodeVersionDrift = nodeVersions
	}
}

func nodeOutdatedEntry(pin, label string, releases NodeReleaseCache) *NodeOutdated {
	pinned, ok := parseNPMVersion(pin)
	if !ok {
		return nil
	}
	release, found := releases.ByMajor[uintString(pinned.major)]
	if !found {
		return nil
	}
	current, ok := parseNPMVersion(release.Version)
	if !ok || compareNPMVersions(pinned, current) >= 0 {
		return nil
	}
	return &NodeOutdated{
		Pinned: pin, Latest: release.Version, LTS: release.LTS,
		Severity: npmDiffSeverity(npmVersionDifference(pinned, current)),
		Project:  label,
	}
}

func recordPackageDrift(name string, byVersion map[string][]string, policy policyFile, fleet *NPMFleetSummary) {
	if len(byVersion) <= 1 {
		return
	}
	if reason, allowed := policy.NPM.DriftOK[name]; allowed {
		fleet.PackageDriftOK[name] = AllowedNPMDrift{Versions: byVersion, Reason: reason}
		return
	}
	fleet.PackageDrift[name] = NPMDrift{
		Versions: byVersion,
		Severity: npmVersionSpreadSeverity(sortedNPMVersionNames(byVersion)),
	}
}

func recordPackageOutdated(
	repos []Repository,
	name string,
	byVersion map[string][]string,
	latest map[string]NPMLatest,
	policy policyFile,
	fleet *NPMFleetSummary,
) {
	cache, known := latest[name]
	if !known || cache.Version == "" {
		return
	}
	currentLatest, ok := parseNPMVersion(cache.Version)
	if !ok {
		return
	}
	newest := latestNPMVersion(sortedNPMVersionNames(byVersion))
	if newest == nil || compareNPMVersions(currentLatest, *newest) <= 0 {
		return
	}
	blocked := npmUpgradeBlockers(repos, name, byVersion, currentLatest)
	if reason, held := policy.NPM.Hold[name]; held {
		fleet.PackageHeld[name] = NPMHold{
			Latest: cache.Version, Versions: byVersion, Reason: reason, BlockedBy: blocked,
		}
		return
	}
	severity := npmDiffSeverity(npmVersionDifference(*newest, currentLatest))
	if len(blocked) > 0 {
		severity = "constrained"
	}
	fleet.PackageOutdated[name] = NPMOutdated{
		Latest: cache.Version, Versions: byVersion, Severity: severity, BlockedBy: blocked,
	}
}

func npmDiffSeverity(difference int) string {
	switch difference {
	case 1:
		return "info"
	case 2:
		return "dim"
	default:
		return "warn"
	}
}

func sortedNPMVersionNames(versions map[string][]string) []string {
	names := slices.Collect(maps.Keys(versions))
	sortVersionStrings(names, parseNPMVersion, compareNPMVersions)
	return names
}

func latestNPMVersion(versions []string) *npmVersion {
	var latest *npmVersion
	for _, raw := range versions {
		version, ok := parseNPMVersion(raw)
		if !ok {
			continue
		}
		if latest == nil || compareNPMVersions(version, *latest) > 0 {
			newest := version
			latest = &newest
		}
	}
	return latest
}

func npmUpgradeBlockers(repos []Repository, packageName string, versions map[string][]string, latest npmVersion) []string {
	usedBy := map[string]bool{}
	for _, projects := range versions {
		for _, project := range projects {
			usedBy[project] = true
		}
	}
	latestVersion := npmVersionString(latest)
	blocked := []string{}
	for _, repo := range repos {
		for _, project := range repo.NPMProjects {
			blocked = append(blocked,
				npmProjectBlockers(repo.Name, project, packageName, latestVersion, usedBy)...)
		}
	}
	slices.Sort(blocked)
	return blocked
}

func npmProjectBlockers(repoName string, project NPMProject, packageName, latestVersion string, usedBy map[string]bool) []string {
	label := npmProjectLabel(repoName, project)
	if !usedBy[label] {
		return nil
	}
	var blocked []string
	for _, edge := range project.Edges[packageName] {
		if !npmRangeSatisfies(edge.Req, latestVersion) {
			blocked = append(blocked, fmt.Sprintf("%s %s (%s)", edge.By, edge.Req, label))
		}
	}
	return blocked
}
