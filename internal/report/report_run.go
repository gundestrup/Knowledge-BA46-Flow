package report

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/logging"
)

type RunOptions struct {
	Remote     bool
	Force      bool
	Repository string
	// Progress, when set, receives a short "repo i/N: name" status line as
	// local checks walk the registry — the ops layer maps it to job detail.
	Progress func(detail string)
}

// RunWithOptions refreshes the requested repository data, carries forward
// previously collected remote fields on errors/no-remote runs, and atomically
// persists the assembled snapshot.
func RunWithOptions(ctx context.Context, cfg config.ReportConfig, db *database.DB, options RunOptions) (Snapshot, error) {
	old, err := previousSnapshot(ctx, db)
	if err != nil {
		return Snapshot{}, err
	}
	registry, policy, err := loadSources(cfg)
	if err != nil {
		return Snapshot{}, err
	}
	if options.Repository != "" {
		if _, ok := registry[options.Repository]; !ok {
			return Snapshot{}, fmt.Errorf("unknown repository %q", options.Repository)
		}
	}
	if options.Progress != nil {
		options.Progress("resolving policy channels")
	}
	resolved, pins := resolvePolicyVersions(ctx, newRemoteHTTP(), policy, previousPolicyPins(old), options.Remote, options.Force)
	effective := policy
	applyResolvedVersions(&effective, resolved)
	local, err := collect(ctx, cfg, resolved, options.Progress)
	if err != nil {
		return Snapshot{}, err
	}
	oldRepos := repositoryMap(old)
	referenceRepos := loadComparisonRepositories(cfg.ComparisonPath)
	mergeComparisonReferences(oldRepos, referenceRepos, registry)
	localRepos := repositoryMap(&local)
	snapshot := local
	setReportScope(&snapshot, options, old)
	snapshot.PolicyVersions = pins
	snapshot.PolicyExceptions = policyExceptions(policy)
	seedRemoteCaches(&snapshot, cfg, old, options)
	snapshot.Repos = mergeRepositories(registry, localRepos, oldRepos, options.Repository)
	prepareSnapshotRepos(snapshot.Repos, localRepos, oldRepos, referenceRepos, old, options)

	if err := refreshRemoteSnapshot(ctx, cfg, &snapshot, registry, oldRepos, effective, options); err != nil {
		return Snapshot{}, err
	}
	if options.Progress != nil {
		options.Progress("building fleet summary")
	}
	snapshot.Fleet = buildFleet(snapshot.Repos, effective, snapshot.GemLatest, time.Now())
	snapshot.Fleet.NPM = buildNPMFleet(snapshot.Repos, effective, snapshot.NPMLatest, snapshot.NodeReleases)
	summarizeReport(&snapshot, registry, effective)

	data, err := json.Marshal(snapshot)
	if err != nil {
		return Snapshot{}, fmt.Errorf("encode report snapshot: %w", err)
	}
	if err := db.SaveRepoReportSnapshot(ctx, snapshot.GeneratedAt, data); err != nil {
		return Snapshot{}, fmt.Errorf("save report snapshot: %w", err)
	}
	summary, err := json.Marshal(snapshot.Summary)
	if err != nil {
		return Snapshot{}, fmt.Errorf("encode report summary: %w", err)
	}
	if err := db.SaveRepoReportHistory(ctx, snapshot.GeneratedAt, summary); err != nil {
		return Snapshot{}, fmt.Errorf("save report history: %w", err)
	}
	return snapshot, nil
}

// mergeComparisonReferences folds comparison-file remote data into the old
// repository map — registered repositories only.
func mergeComparisonReferences(oldRepos, referenceRepos map[string]*Repository, registry map[string]repositoryEntry) {
	for name, reference := range referenceRepos {
		if _, registered := registry[name]; registered {
			oldRepos[name] = mergeReferenceRemote(oldRepos[name], reference)
		}
	}
}

// setReportScope marks the snapshot local-only or local-and-remote. Local runs
// keep the previous remote timestamp, falling back to the comparison file's.
func setReportScope(snapshot *Snapshot, options RunOptions, old *Snapshot) {
	snapshot.RemoteEnabled = options.Remote
	if options.Remote {
		snapshot.Scope = "local-and-remote"
		snapshot.RemoteGeneratedAt = snapshot.GeneratedAt
		return
	}
	snapshot.Scope = "local-only"
	if old != nil {
		snapshot.RemoteGeneratedAt = old.RemoteGeneratedAt
	}
	if snapshot.RemoteGeneratedAt == "" && snapshot.Comparison.Available {
		snapshot.RemoteGeneratedAt = snapshot.Comparison.GeneratedAt
	}
}

// policyExceptions surfaces configured check exceptions on the snapshot.
func policyExceptions(policy policyFile) PolicyExceptions {
	if len(policy.Checks.TagReleaseExceptions) == 0 {
		return PolicyExceptions{}
	}
	return PolicyExceptions{TagRelease: policy.Checks.TagReleaseExceptions}
}

// seedRemoteCaches carries the previous remote caches forward, marks them
// stale on local-only runs, and overlays the comparison file's gem data.
func seedRemoteCaches(snapshot *Snapshot, cfg config.ReportConfig, old *Snapshot, options RunOptions) {
	snapshot.GemLatest = copyGemLatest(old)
	snapshot.NPMLatest = copyNPMLatest(old)
	snapshot.NodeReleases = copyNodeReleaseCache(old)
	if !options.Remote {
		markNPMLatestStale(snapshot.NPMLatest)
		if len(snapshot.NodeReleases.ByMajor) > 0 {
			snapshot.NodeReleases.Stale = true
		}
	}
	for name, cached := range loadComparisonGemLatest(cfg.ComparisonPath) {
		if _, exists := snapshot.GemLatest[name]; !exists {
			snapshot.GemLatest[name] = cached
		}
	}
}

// prepareSnapshotRepos reconciles gem changes for locally collected repos,
// carries forward prior remote fields, and marks remote data stale for
// repositories outside the refresh scope.
func prepareSnapshotRepos(repos []Repository, localRepos, oldRepos, referenceRepos map[string]*Repository, old *Snapshot, options RunOptions) {
	for i := range repos {
		repo := &repos[i]
		if localRepos[repo.Name] != nil {
			reconcileGemChanges(repo, old, referenceRepos, oldRepos)
		}
		copyRemoteData(repo, oldRepos[repo.Name])
		if options.Repository != "" && repo.Name != options.Repository {
			continue
		}
		if !options.Remote {
			markRepositoryRemoteStale(repo)
		}
	}
}

// reconcileGemChanges restores a baseline's recorded changes on snapshots that
// predate ReportGemBaselineVersion; otherwise it diffs against the old repo.
func reconcileGemChanges(repo *Repository, old *Snapshot, referenceRepos, oldRepos map[string]*Repository) {
	if old != nil && old.SchemaVersion >= ReportGemBaselineVersion {
		setGemChanges(repo, oldRepos[repo.Name])
		return
	}
	baseline := referenceRepos[repo.Name]
	if baseline == nil {
		baseline = oldRepos[repo.Name]
	}
	if baseline != nil && (baseline.GemChanges.Added != nil || baseline.GemChanges.Removed != nil) {
		repo.GemChanges = baseline.GemChanges
		return
	}
	setGemChanges(repo, baseline)
}

// refreshRemoteSnapshot performs the remote refresh pass: per-repository
// remote data plus the shared gem/npm/Node latest-version caches.
func refreshRemoteSnapshot(
	ctx context.Context,
	cfg config.ReportConfig,
	snapshot *Snapshot,
	registry map[string]repositoryEntry,
	oldRepos map[string]*Repository,
	policy policyFile,
	options RunOptions,
) error {
	if !options.Remote {
		return nil
	}
	if err := refreshRemoteData(ctx, cfg, registry, snapshot, oldRepos, options); err != nil {
		return err
	}
	client := newRemoteHTTP()
	snapshot.GemLatest = refreshGemLatest(ctx, client, fleetGemVersions(snapshot.Repos), snapshot.GemLatest, options.Force)
	snapshot.NPMLatest = refreshNPMLatest(ctx, client, fleetNPMVersions(snapshot.Repos), snapshot.NPMLatest, options.Force)
	if hasExactNodePins(snapshot.Repos) || exactNodePolicy(policy.Versions.Node) {
		snapshot.NodeReleases = refreshNodeReleaseCache(ctx, client, snapshot.NodeReleases, options.Force)
	}
	return nil
}

// previousSnapshot reads the stored snapshot for carry-forward. A torn
// payload is derived state — the run regenerates and overwrites it — so
// corruption warns and degrades to "no previous snapshot" rather than
// blocking every report refresh. Authoritative tables (documents,
// suggestions) still fail closed upstream at database.Open.
func previousSnapshot(ctx context.Context, db *database.DB) (*Snapshot, error) {
	_, data, err := db.RepoReportSnapshot(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		logging.Infof("WARNING: stored report snapshot is corrupt, regenerating: %v", err)
		return nil, nil
	}
	return &snapshot, nil
}

func repositoryMap(snapshot *Snapshot) map[string]*Repository {
	out := map[string]*Repository{}
	if snapshot == nil {
		return out
	}
	for i := range snapshot.Repos {
		repo := snapshot.Repos[i]
		out[repo.Name] = &repo
	}
	return out
}

func mergeRepositories(registry map[string]repositoryEntry, local, old map[string]*Repository, only string) []Repository {
	selected := selectRepositories(registry, local, old, only)
	names := slices.Sorted(maps.Keys(selected))
	out := make([]Repository, 0, len(names))
	for _, name := range names {
		out = append(out, selected[name])
	}
	return out
}

// selectRepositories picks the repository set for the run: every local repo on
// a full run; on a single-repository run the other registered repos from the
// previous snapshot, the target's fresh local data, and any previously unseen
// local repos.
func selectRepositories(registry map[string]repositoryEntry, local, old map[string]*Repository, only string) map[string]Repository {
	selected := map[string]Repository{}
	if only == "" {
		for name, repo := range local {
			selected[name] = *repo
		}
		return selected
	}
	for name, repo := range old {
		if name == only {
			continue
		}
		if _, stillRegistered := registry[name]; stillRegistered {
			selected[name] = *repo
		}
	}
	if repo := local[only]; repo != nil {
		selected[only] = *repo
	}
	for name, repo := range local {
		if name != only && old[name] == nil {
			selected[name] = *repo
		}
	}
	return selected
}

// nonNilMap guards the maps.Clone nil case: callers assign into the returned
// map, so it must never be nil.
func nonNilMap[K comparable, V any](m map[K]V) map[K]V {
	if m == nil {
		return map[K]V{}
	}
	return m
}

func copyGemLatest(old *Snapshot) map[string]GemLatest {
	if old == nil {
		return map[string]GemLatest{}
	}
	return nonNilMap(maps.Clone(old.GemLatest))
}

func copyNPMLatest(old *Snapshot) map[string]NPMLatest {
	if old == nil {
		return map[string]NPMLatest{}
	}
	return nonNilMap(maps.Clone(old.NPMLatest))
}

func copyNodeReleaseCache(old *Snapshot) NodeReleaseCache {
	if old == nil {
		return NodeReleaseCache{ByMajor: map[string]NodeRelease{}}
	}
	cache := old.NodeReleases
	if cache.ByMajor == nil {
		cache.ByMajor = map[string]NodeRelease{}
	}
	return cache
}

func hasExactNodePins(repos []Repository) bool {
	for _, repo := range repos {
		for _, project := range repo.NPMProjects {
			if _, ok := parseNPMVersion(project.NodeVersion); ok {
				return true
			}
		}
	}
	return false
}

func exactNodePolicy(policy string) bool {
	_, ok := parseNPMVersion(normalizeNodePin(policy))
	return ok
}

// previousPolicyPins carries the last run's resolved channels forward so a
// local-only run reuses (and marks stale) rather than losing the policy.
func previousPolicyPins(old *Snapshot) map[string]PolicyPin {
	if old == nil {
		return map[string]PolicyPin{}
	}
	return old.PolicyVersions
}

func setGemChanges(repo *Repository, old *Repository) {
	if old == nil {
		repo.GemChanges = GemChanges{Added: []string{}, Removed: []string{}}
		return
	}
	added, removed := []string{}, []string{}
	for gem := range repo.Gems {
		if _, ok := old.Gems[gem]; !ok {
			added = append(added, gem)
		}
	}
	for gem := range old.Gems {
		if _, ok := repo.Gems[gem]; !ok {
			removed = append(removed, gem)
		}
	}
	repo.GemChanges = GemChanges{Added: uniqueSorted(added), Removed: uniqueSorted(removed)}
}

func mergeReferenceRemote(current, reference *Repository) *Repository {
	if current == nil {
		clone := *reference
		return &clone
	}
	merged := *current
	if merged.Gems == nil {
		merged.Gems = reference.Gems
	}
	if reference.GitHub != nil {
		merged.GitHub = mergeGitHubReference(merged.GitHub, reference.GitHub)
	}
	if reference.Forgejo != nil {
		merged.Forgejo = mergeForgejoReference(merged.Forgejo, reference.Forgejo)
	}
	if merged.CodeFactor == nil {
		merged.CodeFactor = reference.CodeFactor
		merged.CodeFactorStale = reference.CodeFactor != nil
	}
	if merged.Codecov == nil {
		merged.Codecov = reference.Codecov
		merged.CodecovStale = reference.Codecov != nil
	}
	if merged.SonarCloud == nil {
		merged.SonarCloud = reference.SonarCloud
		merged.SonarFailures = reference.SonarFailures
		merged.SonarURL = reference.SonarURL
		merged.SonarKind = reference.SonarKind
		merged.SonarStale = reference.SonarCloud != nil
	}
	if merged.SemgrepFindings == nil && reference.SemgrepFindings != nil {
		stale := *reference.SemgrepFindings
		stale.Stale = true
		merged.SemgrepFindings = &stale
	}
	return &merged
}

func mergeGitHubReference(current, reference *GitHubInfo) *GitHubInfo {
	if current == nil {
		return carryGitHubStale(nil, reference)
	}
	merged := cloneGitHubInfo(current)
	stale := append([]string{}, merged.StaleFields...)
	carryRemoteField(&merged.Topics, reference.Topics, "topics", &stale)
	carryRemoteField(&merged.Private, reference.Private, "private", &stale)
	carryRemoteField(&merged.OpenPRs, reference.OpenPRs, "open_prs", &stale)
	carryRemoteField(&merged.PRTitles, reference.PRTitles, "pr_titles", &stale)
	carryRemoteField(&merged.PRs, reference.PRs, "prs", &stale)
	carryRemoteField(&merged.CI, reference.CI, "ci", &stale)
	carryRemoteField(&merged.TagsWithoutRelease, reference.TagsWithoutRelease, "tags_without_release", &stale)
	carryRemoteField(&merged.ReleasesWithoutTag, reference.ReleasesWithoutTag, "releases_without_tag", &stale)
	carryRemoteField(&merged.CodeScanning, reference.CodeScanning, "code_scanning", &stale)
	carryRemoteField(&merged.Dependabot, reference.Dependabot, "dependabot", &stale)
	carryRemoteField(&merged.DefaultBranch, reference.DefaultBranch, "default_branch", &stale)
	merged.StaleFields = uniqueSorted(stale)
	return merged
}

func mergeForgejoReference(current, reference *ForgejoInfo) *ForgejoInfo {
	if current == nil {
		return carryForgejoStale(nil, reference)
	}
	merged := cloneForgejoInfo(current)
	stale := append([]string{}, merged.StaleFields...)
	carryRemoteField(&merged.OpenPRs, reference.OpenPRs, "open_prs", &stale)
	carryRemoteField(&merged.PRTitles, reference.PRTitles, "pr_titles", &stale)
	carryRemoteField(&merged.PRs, reference.PRs, "prs", &stale)
	carryRemoteField(&merged.ActionRuns, reference.ActionRuns, "action_runs", &stale)
	carryRemoteField(&merged.TagsWithoutRelease, reference.TagsWithoutRelease, "tags_without_release", &stale)
	carryRemoteField(&merged.ReleasesWithoutTag, reference.ReleasesWithoutTag, "releases_without_tag", &stale)
	merged.StaleFields = uniqueSorted(stale)
	return merged
}
