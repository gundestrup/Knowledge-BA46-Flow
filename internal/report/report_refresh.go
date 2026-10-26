package report

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"

	"golang.org/x/sync/errgroup"

	"knowledge-ba46-flow/internal/config"
)

func refreshRemoteData(
	ctx context.Context,
	cfg config.ReportConfig,
	registry map[string]repositoryEntry,
	snapshot *Snapshot,
	oldRepos map[string]*Repository,
	options RunOptions,
) error {
	if len(snapshot.Repos) == 0 {
		return ctx.Err()
	}
	registryPath, err := filepath.Abs(cfg.RegistryPath)
	if err != nil {
		return fmt.Errorf("resolve report registry: %w", err)
	}
	registryDir := filepath.Dir(registryPath)
	client := newRemoteHTTP()
	token := githubToken(ctx)

	var group errgroup.Group
	group.SetLimit(min(4, len(snapshot.Repos)))
	for index := range snapshot.Repos {
		if err := ctx.Err(); err != nil {
			_ = group.Wait()
			return err
		}
		repo := &snapshot.Repos[index]
		group.Go(func() error {
			refreshRemoteRepositoryForRepo(ctx, client, repo, registryDir, registry, oldRepos, token, options)
			return nil
		})
	}
	_ = group.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	refreshSemgrepData(ctx, client, snapshot, registry)
	return ctx.Err()
}

// refreshRemoteRepositoryForRepo refreshes one repository's remote data unless
// the context is done or the run targets a different repository. Paths are
// resolved against the registry directory.
func refreshRemoteRepositoryForRepo(
	ctx context.Context,
	client *remoteHTTP,
	repo *Repository,
	registryDir string,
	registry map[string]repositoryEntry,
	oldRepos map[string]*Repository,
	token string,
	options RunOptions,
) {
	if ctx.Err() != nil {
		return
	}
	if options.Repository != "" && repo.Name != options.Repository {
		return
	}
	entry := registry[repo.Name]
	previous := oldRepos[repo.Name]
	unchanged := !options.Force && previous != nil &&
		repo.HeadSHA != "" && repo.HeadSHA == previous.HeadSHA &&
		repo.StatusSig != "" && repo.StatusSig == previous.StatusSig
	dir := entry.Path
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(registryDir, dir)
	}
	refreshRemoteRepository(ctx, client, repo, dir, entry, previous, token, unchanged)
}

// refreshSemgrepData refreshes Semgrep findings when a token is configured;
// without a token or on request failure existing findings are marked stale and
// cloud-ci repos without findings record the failure reason.
func refreshSemgrepData(ctx context.Context, client *remoteHTTP, snapshot *Snapshot, registry map[string]repositoryEntry) {
	reason := "semgrep token is not configured"
	if token := semgrepToken(); token != "" {
		data, err := client.semgrepPlatform(ctx, token)
		if err == nil {
			refreshSemgrepFindings(data, snapshot.Repos, registry)
			return
		} else {
			reason = err.Error()
		}
	}
	markSemgrepStale(snapshot.Repos, reason)
}

func markSemgrepStale(repos []Repository, reason string) {
	for i := range repos {
		if repos[i].SemgrepFindings != nil {
			repos[i].SemgrepFindings.Stale = true
			continue
		}
		if repos[i].SemgrepLicense == "cloud-ci" {
			repos[i].SemgrepFindings = &SemgrepFindings{Stale: true, Error: reason}
		}
	}
}

func copyRemoteData(repo *Repository, previous *Repository) {
	if previous == nil {
		return
	}
	repo.GitHub = cloneGitHubInfo(previous.GitHub)
	repo.Forgejo = cloneForgejoInfo(previous.Forgejo)
	repo.CodeFactor = previous.CodeFactor
	repo.CodeFactorStale = previous.CodeFactorStale
	repo.Codecov = previous.Codecov
	repo.CodecovStale = previous.CodecovStale
	repo.SonarCloud = previous.SonarCloud
	repo.SonarFailures = previous.SonarFailures
	repo.SonarURL = previous.SonarURL
	repo.SonarKind = previous.SonarKind
	repo.SonarStale = previous.SonarStale
	if previous.SemgrepFindings != nil {
		findings := *previous.SemgrepFindings
		findings.Severities = maps.Clone(previous.SemgrepFindings.Severities)
		repo.SemgrepFindings = &findings
	}
}

func markRepositoryRemoteStale(repo *Repository) {
	if g := repo.GitHub; g != nil {
		markStaleFields(&g.StaleFields, map[string]bool{
			"topics": g.Topics != nil, "private": g.Private != nil,
			"open_prs": g.OpenPRs != nil, "pr_titles": g.PRTitles != nil,
			"prs": g.PRs != nil, "ci": g.CI != nil,
			"tags_without_release": g.TagsWithoutRelease != nil,
			"releases_without_tag": g.ReleasesWithoutTag != nil,
			"code_scanning":        g.CodeScanning != nil, "dependabot": g.Dependabot != nil,
			"default_branch": g.DefaultBranch != nil,
		})
	}
	if f := repo.Forgejo; f != nil {
		markStaleFields(&f.StaleFields, map[string]bool{
			"open_prs": f.OpenPRs != nil, "pr_titles": f.PRTitles != nil,
			"prs": f.PRs != nil, "action_runs": f.ActionRuns != nil,
			"tags_without_release": f.TagsWithoutRelease != nil,
			"releases_without_tag": f.ReleasesWithoutTag != nil,
		})
	}
	if repo.SemgrepFindings != nil {
		repo.SemgrepFindings.Stale = true
	}
	if repo.CodeFactor != nil {
		repo.CodeFactorStale = true
	}
	if repo.Codecov != nil {
		repo.CodecovStale = true
	}
	if repo.SonarCloud != nil {
		repo.SonarStale = true
	}
}

func summarizeReport(snapshot *Snapshot, registry map[string]repositoryEntry, policy policyFile) {
	summary := Summary{
		Repositories:    len(snapshot.Repos),
		Available:       0,
		Missing:         0,
		Dirty:           0,
		Problems:        0,
		SonarProjects:   0,
		SemgrepProjects: 0,
	}
	counts := summaryCounts{npmChecked: checkedNPMPackages(snapshot)}
	for i := range snapshot.Repos {
		repo := &snapshot.Repos[i]
		entry := registry[repo.Name]
		repo.Problems = deriveProblems(*repo, entry.URL, policy.Checks.TagReleaseExceptions[repo.Name], policy.Checks.MinCoverage)
		counts.accumulate(&summary, repo)
	}
	counts.releaseKnown = nodeReleaseKnown(snapshot, policy)
	counts.apply(&summary, snapshot)
	snapshot.Summary = summary
}

// summaryCounts holds the cross-repository tallies summarized into Summary
// pointers at the end — Sonar/Semgrep blocked counts, open PRs and staleness.
type summaryCounts struct {
	sonarReported, sonarBlocked     int
	semgrepReported, semgrepBlocked int
	semgrepFindings                 int
	openPRs, openPRRepositories     int
	staleRepos, npmChecked          int
	releaseKnown                    bool
}

// checkedNPMPackages counts installed npm packages with a fresh upstream
// latest-version record.
func checkedNPMPackages(snapshot *Snapshot) int {
	checked := 0
	for name := range fleetNPMVersions(snapshot.Repos) {
		if item, ok := snapshot.NPMLatest[name]; ok && item.Available {
			checked++
		}
	}
	return checked
}

// nodeReleaseKnown reports whether any pinned Node version — per project or
// from an exact policy pin — resolves to a known major release.
func nodeReleaseKnown(snapshot *Snapshot, policy policyFile) bool {
	for _, repo := range snapshot.Repos {
		for _, project := range repo.NPMProjects {
			if version, ok := parseNPMVersion(project.NodeVersion); ok {
				if _, found := snapshot.NodeReleases.ByMajor[uintString(version.major)]; found {
					return true
				}
			}
		}
	}
	if exactNodePolicy(policy.Versions.Node) {
		if version, ok := parseNPMVersion(normalizeNodePin(policy.Versions.Node)); ok {
			_, found := snapshot.NodeReleases.ByMajor[uintString(version.major)]
			return found
		}
	}
	return false
}

// accumulate folds one repository's signals into the summary and counts.
func (c *summaryCounts) accumulate(summary *Summary, repo *Repository) {
	if repo.PathExists {
		summary.Available++
	} else {
		summary.Missing++
	}
	if repo.Dirty {
		summary.Dirty++
	}
	summary.Problems += len(repo.Problems)
	if slices.Contains(repo.Scanners, "sonar") {
		summary.SonarProjects++
	}
	if slices.Contains(repo.Scanners, "semgrep") {
		summary.SemgrepProjects++
	}
	for _, project := range repo.NPMProjects {
		summary.NodeProjects++
		summary.NPMPackages += len(project.Packages)
	}
	c.countScannerSignals(repo)
	c.countRemoteStale(repo)
}

// countScannerSignals tallies Sonar/Semgrep reporting state and open PR
// counts for one repository.
func (c *summaryCounts) countScannerSignals(repo *Repository) {
	if repo.SonarCloud != nil {
		c.sonarReported++
		if *repo.SonarCloud != "OK" || len(repo.SonarFailures) > 0 {
			c.sonarBlocked++
		}
	}
	if findings := repo.SemgrepFindings; findings != nil && findings.Open != nil {
		c.semgrepReported++
		c.semgrepFindings += *findings.Open
		if *findings.Open > 0 {
			c.semgrepBlocked++
		}
	}
	if repo.GitHub != nil && repo.GitHub.OpenPRs != nil {
		c.openPRs += *repo.GitHub.OpenPRs
		c.openPRRepositories++
	}
	if repo.Forgejo != nil && repo.Forgejo.OpenPRs != nil {
		c.openPRs += *repo.Forgejo.OpenPRs
		c.openPRRepositories++
	}
}

// countRemoteStale counts repositories whose remote data could not be
// refreshed or carries stale fields.
func (c *summaryCounts) countRemoteStale(repo *Repository) {
	stale := repo.CodeFactorStale || repo.SonarStale ||
		(repo.SemgrepFindings != nil && repo.SemgrepFindings.Stale)
	if repo.GitHub != nil && (repo.GitHub.AuthUnavailable || len(repo.GitHub.StaleFields) > 0) {
		stale = true
	}
	if repo.Forgejo != nil && (repo.Forgejo.AuthUnavailable || len(repo.Forgejo.StaleFields) > 0) {
		stale = true
	}
	if stale {
		c.staleRepos++
	}
}

// apply writes the counted tallies into summary pointer fields — populated
// only when the corresponding signal was actually reported.
func (c *summaryCounts) apply(summary *Summary, snapshot *Snapshot) {
	summary.SonarReportedProjects = c.sonarReported
	if c.sonarReported > 0 {
		summary.SonarBlocked = &c.sonarBlocked
	}
	summary.SemgrepReportedProjects = c.semgrepReported
	if c.semgrepReported > 0 {
		summary.SemgrepBlocked = &c.semgrepBlocked
		summary.SemgrepOpenFindings = &c.semgrepFindings
	}
	summary.OpenPRRepositories = c.openPRRepositories
	if c.openPRRepositories > 0 {
		summary.OpenPRs = &c.openPRs
	}
	summary.NPMCheckedPackages = c.npmChecked
	if c.npmChecked > 0 {
		outdated := len(snapshot.Fleet.NPM.PackageOutdated)
		summary.NPMOutdated = &outdated
	}
	if c.releaseKnown {
		outdated := len(snapshot.Fleet.NPM.NodeOutdated)
		summary.NodeOutdated = &outdated
	}
	summary.RemoteStaleRepos = c.staleRepos
}
