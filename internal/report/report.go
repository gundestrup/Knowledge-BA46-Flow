// Package report creates read-only local and remote health snapshots for
// repositories registered in an external YAML file.
package report

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
)

type Snapshot struct {
	SchemaVersion     int                  `json:"schema_version"`
	Configured        bool                 `json:"configured"`
	Scope             string               `json:"scope"`
	RemoteEnabled     bool                 `json:"remote_enabled"`
	RemoteGeneratedAt string               `json:"remote_generated_at,omitempty"`
	GeneratedAt       string               `json:"generated_at,omitempty"`
	PolicyRuby        string               `json:"policy_ruby,omitempty"`
	PolicyVersions    map[string]PolicyPin `json:"policy_versions,omitempty"`
	PolicyMinCoverage *float64             `json:"policy_min_coverage,omitempty"`
	Summary           Summary              `json:"summary"`
	Comparison        Comparison           `json:"comparison"`
	PolicyExceptions  PolicyExceptions     `json:"policy_exceptions,omitempty"`
	GemLatest         map[string]GemLatest `json:"gem_latest,omitempty"`
	NPMLatest         map[string]NPMLatest `json:"npm_latest,omitempty"`
	NodeReleases      NodeReleaseCache     `json:"node_releases"`
	Fleet             FleetSummary         `json:"fleet"`
	Repos             []Repository         `json:"repos"`
}

type Summary struct {
	Repositories            int  `json:"repositories"`
	Available               int  `json:"available"`
	Missing                 int  `json:"missing"`
	Dirty                   int  `json:"dirty"`
	Problems                int  `json:"problems"`
	SonarProjects           int  `json:"sonar_projects"`
	SemgrepProjects         int  `json:"semgrep_projects"`
	SonarReportedProjects   int  `json:"sonar_reported_projects"`
	SonarBlocked            *int `json:"sonar_blocked"`
	SemgrepReportedProjects int  `json:"semgrep_reported_projects"`
	SemgrepBlocked          *int `json:"semgrep_blocked"`
	SemgrepOpenFindings     *int `json:"semgrep_open_findings"`
	OpenPRs                 *int `json:"open_prs"`
	OpenPRRepositories      int  `json:"open_pr_repositories"`
	RemoteStaleRepos        int  `json:"remote_stale_repositories"`
	NodeProjects            int  `json:"node_projects"`
	NPMPackages             int  `json:"npm_packages"`
	NPMCheckedPackages      int  `json:"npm_checked_packages"`
	NPMOutdated             *int `json:"npm_outdated"`
	NodeOutdated            *int `json:"node_outdated"`
}

type Repository struct {
	Name            string                       `json:"name"`
	URL             string                       `json:"url,omitempty"`
	WebURL          string                       `json:"web_url,omitempty"`
	Path            string                       `json:"path"`
	ReportedAt      string                       `json:"reported_at,omitempty"`
	PathExists      bool                         `json:"path_exists"`
	Tags            map[string][]string          `json:"tags"`
	HeadSHA         string                       `json:"head_sha,omitempty"`
	StatusSig       string                       `json:"status_sig,omitempty"`
	Dirty           bool                         `json:"dirty"`
	Sync            SyncInfo                     `json:"sync"`
	RubyIssues      []string                     `json:"ruby_issues"`
	ToolchainPins   []ToolchainPin               `json:"toolchain_pins,omitempty"`
	ToolchainIssues []string                     `json:"toolchain_issues"`
	Scanners        []string                     `json:"scanners"`
	SemgrepLicense  string                       `json:"semgrep_license,omitempty"`
	SemgrepFindings *SemgrepFindings             `json:"semgrep_findings,omitempty"`
	GitHub          *GitHubInfo                  `json:"github,omitempty"`
	Forgejo         *ForgejoInfo                 `json:"forgejo,omitempty"`
	CodeFactor      *string                      `json:"codefactor,omitempty"`
	CodeFactorStale bool                         `json:"codefactor_stale,omitempty"`
	Codecov         *float64                     `json:"codecov,omitempty"`
	CodecovStale    bool                         `json:"codecov_stale,omitempty"`
	SonarCloud      *string                      `json:"sonarcloud,omitempty"`
	SonarFailures   []string                     `json:"sonarcloud_failures,omitempty"`
	SonarURL        string                       `json:"sonarcloud_url,omitempty"`
	SonarKind       string                       `json:"sonarcloud_kind,omitempty"`
	SonarStale      bool                         `json:"sonar_stale,omitempty"`
	Gems            map[string]string            `json:"gems,omitempty"`
	GemEdges        map[string][]GemEdge         `json:"gem_edges,omitempty"`
	GemConflicts    []string                     `json:"gem_conflicts,omitempty"`
	LockfileMissing bool                         `json:"lockfile_missing,omitempty"`
	GemspecDeps     map[string]GemspecDependency `json:"gemspec_deps,omitempty"`
	GemsWatch       map[string]string            `json:"gems_watch,omitempty"`
	GemNotes        []string                     `json:"gem_notes,omitempty"`
	GemChanges      GemChanges                   `json:"gem_changes,omitempty"`
	NodeIssues      []string                     `json:"node_issues"`
	NPMProjects     []NPMProject                 `json:"npm_projects"`
	ProcfileOrphan  bool                         `json:"procfile_orphan"`
	Error           string                       `json:"error,omitempty"`
	Problems        []string                     `json:"problems"`
}

type SyncInfo struct {
	Marker     string `json:"marker,omitempty"`
	SyncedAt   string `json:"synced_at,omitempty"`
	RepoCommit string `json:"repo_commit,omitempty"`
	KBCommit   string `json:"kb_commit,omitempty"`
	Drift      *int   `json:"drift"`
}

// Collect reads the configured registry and policy and inspects each listed
// repository without changing its files or Git metadata. Policy channels
// stay unresolved here — they match leniently rather than flagging
// repositories; resolved values only exist inside RunWithOptions.
func Collect(ctx context.Context, cfg config.ReportConfig) (Snapshot, error) {
	return collect(ctx, cfg, nil)
}

// collect is Collect with an optional resolved-versions overlay: RunWithOptions
// resolves policy channels once and applies the effective values everywhere.
func collect(ctx context.Context, cfg config.ReportConfig, resolved map[string]string) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	registry, policy, err := loadSources(cfg)
	if err != nil {
		return Snapshot{}, err
	}
	applyResolvedVersions(&policy, resolved)
	registryPath, err := filepath.Abs(cfg.RegistryPath)
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve report registry: %w", err)
	}
	registryDir := filepath.Dir(registryPath)
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	slices.Sort(names)

	snapshot := Snapshot{
		SchemaVersion:     ReportSchemaVersion,
		Configured:        true,
		Scope:             "local-only",
		GeneratedAt:       time.Now().UTC().Format(time.RFC3339),
		PolicyRuby:        policy.Versions.Ruby,
		PolicyMinCoverage: policy.Checks.MinCoverage,
		Comparison:        loadComparison(cfg.ComparisonPath),
		PolicyExceptions:  PolicyExceptions{TagRelease: policy.Checks.TagReleaseExceptions},
		GemLatest:         map[string]GemLatest{},
		NPMLatest:         map[string]NPMLatest{},
		NodeReleases:      NodeReleaseCache{ByMajor: map[string]NodeRelease{}},
		Fleet:             emptyFleetSummary(),
		Repos:             make([]Repository, 0, len(names)),
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		repo := collectRepository(ctx, registryDir, name, registry[name], policy)
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		snapshot.Repos = append(snapshot.Repos, repo)
		snapshot.Repos[len(snapshot.Repos)-1].ReportedAt = snapshot.GeneratedAt
		snapshot.Summary.Repositories++
		if repo.PathExists {
			snapshot.Summary.Available++
		} else {
			snapshot.Summary.Missing++
		}
		if repo.Dirty {
			snapshot.Summary.Dirty++
		}
		snapshot.Summary.Problems += len(repo.Problems)
		if slices.Contains(repo.Scanners, "sonar") {
			snapshot.Summary.SonarProjects++
		}
		if slices.Contains(repo.Scanners, "semgrep") {
			snapshot.Summary.SemgrepProjects++
		}
		snapshot.Summary.NodeProjects += len(repo.NPMProjects)
		for _, project := range repo.NPMProjects {
			snapshot.Summary.NPMPackages += len(project.Packages)
		}
	}
	return snapshot, nil
}

// Run collects a snapshot and atomically replaces the previously persisted
// report. It never writes to any registered repository.
func Run(ctx context.Context, cfg config.ReportConfig, db *database.DB) (Snapshot, error) {
	return RunWithOptions(ctx, cfg, db, RunOptions{Remote: cfg.WantsRemote()})
}

func collectRepository(ctx context.Context, registryDir, name string, entry repositoryEntry, policy policyFile) Repository {
	displayURL, webURL := repositoryURLs(entry.URL)
	repo := Repository{
		Name:            name,
		URL:             displayURL,
		WebURL:          webURL,
		Path:            entry.Path,
		Tags:            entry.Tags,
		RubyIssues:      []string{},
		ToolchainIssues: []string{},
		Scanners:        []string{},
		Problems:        []string{},
		Sync: SyncInfo{
			SyncedAt:   entry.SyncedAt,
			RepoCommit: entry.RepoCommit,
			KBCommit:   entry.KBCommit,
			Drift:      nil,
		},
	}
	if repo.Tags == nil {
		repo.Tags = map[string][]string{}
	}
	if strings.TrimSpace(entry.Path) == "" {
		repo.Error = "path is not configured"
		repo.Problems = append(repo.Problems, repo.Error)
		return repo
	}
	dir := entry.Path
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(registryDir, dir)
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		repo.Error = "path could not be resolved"
		repo.Problems = append(repo.Problems, repo.Error)
		return repo
	}
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		repo.Error = "path missing"
		repo.Problems = append(repo.Problems, repo.Error)
		return repo
	}
	if err != nil {
		repo.Error = "path could not be inspected"
		repo.Problems = append(repo.Problems, repo.Error)
		return repo
	}
	if !info.IsDir() {
		repo.Error = "path is not a directory"
		repo.Problems = append(repo.Problems, repo.Error)
		return repo
	}
	repo.PathExists = true
	repo.Sync.Marker = markerStatus(dir)

	if head, err := git(ctx, dir, "rev-parse", "--short", "HEAD"); err == nil && head != "" {
		repo.HeadSHA = head
		if entry.RepoCommit != "" {
			if count, err := git(ctx, dir, "rev-list", "--count", "--end-of-options", entry.RepoCommit+"..HEAD"); err == nil {
				if n, err := strconv.Atoi(count); err == nil {
					repo.Sync.Drift = &n
				}
			}
		}
	} else {
		repo.Error = "Git metadata unavailable"
	}
	if status, err := git(ctx, dir, "status", "--porcelain", "--untracked-files=normal"); err == nil {
		repo.Dirty = status != ""
		sum := sha256.Sum256([]byte(status))
		repo.StatusSig = hex.EncodeToString(sum[:])[:12]
	} else if repo.Error == "" {
		repo.Error = "Git metadata unavailable"
	}

	issues, err := rubyVersionIssues(dir, policy.Versions.Ruby, repo.Tags, policy.Checks.RequireRubyVersionFile)
	if err != nil {
		repo.RubyIssues = append(repo.RubyIssues, "could not inspect Ruby version files")
	} else {
		repo.RubyIssues = issues
	}
	repo.Scanners = scannerConfigs(dir)
	repo.SemgrepLicense = semgrepLicense(dir)
	repo.ProcfileOrphan = procfileOrphan(dir)
	applyGemPolicy(&repo, collectGemInventory(dir), policy, nil)
	repo.NPMProjects, repo.NodeIssues = collectNPMProjects(dir, policy)
	repo.ToolchainPins, repo.ToolchainIssues = toolchainIssues(dir, policy)
	repo.Problems = deriveProblems(repo, entry.URL, policy.Checks.TagReleaseExceptions[name], policy.Checks.MinCoverage)
	return repo
}

func repositoryURLs(raw string) (display, link string) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return raw, ""
	}
	hasCredentials := u.User != nil
	u.User = nil
	u.Scheme = strings.ToLower(u.Scheme)
	u.RawQuery = ""
	u.Fragment = ""
	display = u.String()
	if !hasCredentials && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		u.Path = strings.TrimSuffix(u.Path, ".git")
		u.RawPath = ""
		link = u.String()
	}
	return display, link
}

func deriveProblems(repo Repository, url string, exceptions TagReleaseException, minCoverage *float64) []string {
	problems := repoIssues(repo)
	problems = append(problems, npmProblems(repo)...)
	problems = append(problems, syncProblems(repo)...)
	problems = append(problems, gemProblems(repo)...)
	problems = append(problems, localProblems(repo)...)
	problems = append(problems, remoteProblems(repo, exceptions)...)
	return append(problems, scannerProblems(repo, url, minCoverage)...)
}

// repoIssues collects repo-level collection errors and language issues.
func repoIssues(repo Repository) []string {
	problems := []string{}
	if repo.Error != "" {
		problems = append(problems, repo.Error)
	}
	for _, issue := range repo.RubyIssues {
		problems = append(problems, "ruby: "+issue)
	}
	for _, issue := range repo.NodeIssues {
		problems = append(problems, "node: "+issue)
	}
	problems = append(problems, repo.ToolchainIssues...)
	return problems
}

// syncProblems collects knowledge-sync marker state issues.
func syncProblems(repo Repository) []string {
	problems := []string{}
	if repo.Sync.Marker == "missing" {
		problems = append(problems, "kb marker missing")
	}
	if repo.Sync.Marker == "unreadable" {
		problems = append(problems, "kb marker unreadable")
	}
	return problems
}

// localProblems collects local repo hygiene issues: lockfile and Procfile
// state.
func localProblems(repo Repository) []string {
	problems := []string{}
	if repo.LockfileMissing {
		problems = append(problems, "Gemfile without committed lockfile")
	}
	if repo.ProcfileOrphan {
		problems = append(problems, "Procfile.dev orphaned — bin/dev does not run it")
	}
	return problems
}

// npmProblems collects per-project lockfile gaps and watched-package hits.
func npmProblems(repo Repository) []string {
	problems := []string{}
	for _, project := range repo.NPMProjects {
		if project.LockfileMissing {
			problems = append(problems, fmt.Sprintf("package.json without npm lockfile (%s)", project.Path))
		}
		for _, name := range slices.Sorted(maps.Keys(project.Watched)) {
			problems = append(problems, fmt.Sprintf("watched npm package: %s — %s", name, project.Watched[name]))
		}
	}
	return problems
}

// gemProblems collects gem conflicts and watched-gem hits.
func gemProblems(repo Repository) []string {
	problems := []string{}
	for _, conflict := range repo.GemConflicts {
		problems = append(problems, "gem conflict: "+conflict)
	}
	for _, gem := range slices.Sorted(maps.Keys(repo.GemsWatch)) {
		problems = append(problems, fmt.Sprintf("watched gem: %s — %s", gem, repo.GemsWatch[gem]))
	}
	return problems
}

// remoteProblems collects forge-sourced issues: topics, tag/release drift,
// code-scanning and dependabot alerts.
func remoteProblems(repo Repository, exceptions TagReleaseException) []string {
	problems := []string{}
	if github := repo.GitHub; github != nil {
		if github.Topics != nil && len(*github.Topics) == 0 {
			problems = append(problems, "no topics")
		}
		if len(unexcepted(github.TagsWithoutRelease, exceptions.TagsWithoutRelease)) > 0 ||
			len(unexcepted(github.ReleasesWithoutTag, exceptions.ReleasesWithoutTag)) > 0 {
			problems = append(problems, "tag/release mismatch")
		}
		if github.CodeScanning != nil && *github.CodeScanning > 0 {
			problems = append(problems, fmt.Sprintf("%d code-scanning alerts", *github.CodeScanning))
		}
		if github.Dependabot != nil && *github.Dependabot > 0 {
			problems = append(problems, fmt.Sprintf("%d dependabot alerts", *github.Dependabot))
		}
	}
	if forgejo := repo.Forgejo; forgejo != nil &&
		len(unexcepted(forgejo.TagsWithoutRelease, exceptions.TagsWithoutRelease)) > 0 {
		problems = append(problems, "tag/release mismatch")
	}
	return problems
}

// scannerProblems collects findings from the quality scanners (CodeFactor,
// Codecov, Sonar, Semgrep) plus the Semgrep license classification.
func scannerProblems(repo Repository, url string, minCoverage *float64) []string {
	problems := []string{}
	if repo.CodeFactor != nil && codeFactorNeedsAttention(*repo.CodeFactor) {
		problems = append(problems, "codefactor "+*repo.CodeFactor)
	}
	if minCoverage != nil && repo.Codecov != nil && *repo.Codecov < *minCoverage {
		problems = append(problems,
			fmt.Sprintf("codecov coverage %.0f%% below %.0f%%", *repo.Codecov, *minCoverage))
	}
	if repo.SonarCloud != nil && *repo.SonarCloud != "OK" {
		problems = append(problems, "sonar gate "+*repo.SonarCloud+
			failureSuffix(repo.SonarFailures))
	}
	if findings := repo.SemgrepFindings; findings != nil {
		if findings.Project == nil && repo.SemgrepLicense == "cloud-ci" {
			problems = append(problems, "semgrep ci configured but no platform project found")
		} else if findings.Open != nil && *findings.Open > 0 {
			problems = append(problems, semgrepProblem(*findings))
		}
	}
	// Without a remote visibility check, treat every project as private or
	// unknown rather than silently waiving the Semgrep license warning.
	address, hasAddress := splitRepoRemote(url)
	private := (hasAddress && isForgejoHost(address.Host)) ||
		repo.GitHub == nil || repo.GitHub.Private == nil || *repo.GitHub.Private
	if private && (repo.SemgrepLicense == "registry" || repo.SemgrepLicense == "cloud-ci") {
		problems = append(problems, "semgrep non-CE rules on private or unknown repo ("+repo.SemgrepLicense+")")
	}
	return problems
}

func unexcepted(actual *[]string, exceptions []string) []string {
	if actual == nil || slices.Contains(exceptions, "*") {
		return nil
	}
	excluded := make(map[string]bool, len(exceptions))
	for _, exception := range exceptions {
		excluded[exception] = true
	}
	var out []string
	for _, value := range *actual {
		if !excluded[value] {
			out = append(out, value)
		}
	}
	return out
}

func codeFactorNeedsAttention(grade string) bool {
	grades := []string{"A+", "A", "A-", "B+", "B", "B-", "C+", "C", "C-", "D+", "D", "D-", "E", "F"}
	rank := func(value string) int {
		for i, grade := range grades {
			if value == grade {
				return i
			}
		}
		return -1
	}
	actual, threshold := rank(grade), rank("B-")
	return actual >= 0 && actual > threshold
}

func failureSuffix(failures []string) string {
	if len(failures) == 0 {
		return ""
	}
	return " (" + strings.Join(failures, "; ") + ")"
}

func semgrepProblem(findings SemgrepFindings) string {
	severityOrder := map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3}
	severities := make([]string, 0, len(findings.Severities))
	for severity := range findings.Severities {
		severities = append(severities, severity)
	}
	slices.SortFunc(severities, func(a, b string) int {
		return severityOrder[a] - severityOrder[b]
	})
	counts := make([]string, 0, len(severities))
	for _, severity := range severities {
		counts = append(counts, fmt.Sprintf("%s:%d", severity, findings.Severities[severity]))
	}
	message := fmt.Sprintf("%d open semgrep finding(s)", *findings.Open)
	if len(counts) > 0 {
		message += " — " + strings.Join(counts, " ")
	}
	return message
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	commandArgs := append([]string{"--no-optional-locks", "-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", commandArgs...)
	cmd.Env = withoutOptionalGitLocks(os.Environ())
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func withoutOptionalGitLocks(env []string) []string {
	out := make([]string, 0, len(env)+1)
	found := false
	for _, value := range env {
		if strings.HasPrefix(value, "GIT_OPTIONAL_LOCKS=") {
			if !found {
				out = append(out, "GIT_OPTIONAL_LOCKS=0")
				found = true
			}
			continue
		}
		out = append(out, value)
	}
	if !found {
		out = append(out, "GIT_OPTIONAL_LOCKS=0")
	}
	return out
}
