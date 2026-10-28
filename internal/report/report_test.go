package report

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
)

func TestCollectLocalRepositoryReport(t *testing.T) {
	root := t.TempDir()
	registryDir := filepath.Join(root, "config")
	repoDir := filepath.Join(root, "sample")
	if err := os.MkdirAll(registryDir, 0755); err != nil {
		t.Fatal(err)
	}
	createGitRepository(t, repoDir)

	writeReportFile(t, repoDir, ".ruby-version", "ruby-3.4.10\n")
	writeReportFile(t, repoDir, "Rakefile", "semgrep scan --config p/security-audit --oss-only\n")
	writeReportFile(t, repoDir, ".semgrep.yml", "rules: []\n")
	writeReportFile(t, repoDir, "Procfile.dev", "web: bin/server\n")
	writeReportFile(t, repoDir, "bin/dev", "#!/bin/sh\necho no process manager\n")
	writeReportFile(t, repoDir, ".github/workflows/ci.yml", "name: test\n")
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "baseline")
	base := strings.TrimSpace(runGit(t, repoDir, "rev-parse", "--short", "HEAD"))

	writeReportFile(t, repoDir, "committed.txt", "second commit\n")
	runGit(t, repoDir, "add", "committed.txt")
	runGit(t, repoDir, "commit", "-m", "one commit after baseline")
	headBefore := strings.TrimSpace(runGit(t, repoDir, "rev-parse", "HEAD"))
	writeReportFile(t, repoDir, "committed.txt", "uncommitted change\n")
	writeReportFile(t, repoDir, "untracked.txt", "untracked\n")
	indexPath := filepath.Join(repoDir, ".git", "index")
	indexBefore, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}

	registry := `---
repos:
  sample:
    url: https://github.com/example/sample.git
    path: ../sample
    synced_at: "2026-10-01"
    repo_commit: ` + base + `
    kb_commit: deadbee
    tags:
      language: [ruby]
      framework: []
      tools: []
  missing:
    path: ../missing
`
	registryPath := filepath.Join(registryDir, "repos.yml")
	writeReportFile(t, registryDir, "repos.yml", registry)
	policyPath := filepath.Join(registryDir, "policy.yml")
	writeReportFile(t, registryDir, "policy.yml", `versions:
  ruby: "3.4.11"
checks:
  require_ruby_version_file: true
`)

	snapshot, err := Collect(context.Background(), config.ReportConfig{
		RegistryPath: registryPath,
		PolicyPath:   policyPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Configured || snapshot.Scope != "local-only" || len(snapshot.Repos) != 2 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	if snapshot.Summary.Repositories != 2 || snapshot.Summary.Available != 1 ||
		snapshot.Summary.Missing != 1 || snapshot.Summary.Dirty != 1 {
		t.Fatalf("summary=%+v", snapshot.Summary)
	}
	if snapshot.Summary.SemgrepProjects != 1 {
		t.Fatalf("Semgrep project count=%d", snapshot.Summary.SemgrepProjects)
	}

	sample := snapshot.Repos[1]
	if sample.Name != "sample" || !sample.PathExists || !sample.Dirty ||
		sample.WebURL != "https://github.com/example/sample" {
		t.Fatalf("sample repo=%+v", sample)
	}
	if sample.HeadSHA == "" || sample.Sync.Drift == nil || *sample.Sync.Drift != 1 {
		t.Fatalf("Git sync status=%+v head=%q", sample.Sync, sample.HeadSHA)
	}
	if sample.Sync.Marker != "missing" || sample.SemgrepLicense != "registry" ||
		!sample.ProcfileOrphan || !contains(sample.Scanners, "gh-actions") ||
		!contains(sample.Scanners, "semgrep") {
		t.Fatalf("local checks=%+v", sample)
	}
	if len(sample.RubyIssues) != 2 || !contains(sample.RubyIssues, ".ruby-version: 'ruby-' prefix (ruby-3.4.10)") ||
		!contains(sample.RubyIssues, ".ruby-version: 3.4.10 does not satisfy 3.4.11") {
		t.Fatalf("Ruby issues=%v", sample.RubyIssues)
	}
	for _, problem := range []string{
		"kb marker missing",
		"semgrep non-CE rules on private or unknown repo (registry)",
		"Procfile.dev orphaned — bin/dev does not run it",
	} {
		if !contains(sample.Problems, problem) {
			t.Errorf("missing problem %q in %v", problem, sample.Problems)
		}
	}

	headAfter := strings.TrimSpace(runGit(t, repoDir, "rev-parse", "HEAD"))
	if headAfter != headBefore {
		t.Fatalf("report changed HEAD from %s to %s", headBefore, headAfter)
	}
	indexAfter, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(indexBefore, indexAfter) {
		t.Fatal("report changed the Git index")
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".engineering-memory.yml")); !os.IsNotExist(err) {
		t.Fatalf("report wrote the sync marker: stat error=%v", err)
	}
}

func TestCollectRejectsInvalidSources(t *testing.T) {
	dir := t.TempDir()
	registryPath := filepath.Join(dir, "repos.yml")
	policyPath := filepath.Join(dir, "policy.yml")
	writeReportFile(t, dir, "repos.yml", "not_repos: {}\n")
	writeReportFile(t, dir, "policy.yml", "versions: [invalid\n")

	if _, err := Collect(context.Background(), config.ReportConfig{RegistryPath: registryPath}); err == nil {
		t.Fatal("registry without repos mapping should fail")
	}
	writeReportFile(t, dir, "repos.yml", "repos: {}\n")
	if _, err := Collect(context.Background(), config.ReportConfig{
		RegistryPath: registryPath,
		PolicyPath:   policyPath,
	}); err == nil {
		t.Fatal("invalid policy YAML should fail")
	}
	if _, err := Collect(context.Background(), config.ReportConfig{}); err == nil {
		t.Fatal("missing registry path should fail")
	}
}

func createGitRepository(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "KBM-flow tests")
	runGit(t, dir, "config", "user.email", "kbm-flow@example.invalid")
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", commandArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func writeReportFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestRunPersistsSnapshotForTheDashboard(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "sample")
	createGitRepository(t, repoDir)
	registryPath := filepath.Join(root, "repos.yml")
	writeReportFile(t, root, "repos.yml", "repos:\n  sample:\n    path: "+repoDir+"\n")

	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	snapshot, err := Run(context.Background(), config.ReportConfig{RegistryPath: registryPath}, db)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Summary.Repositories != 1 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	generatedAt, stored, err := db.RepoReportSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if generatedAt != snapshot.GeneratedAt || len(stored) == 0 {
		t.Fatalf("stored snapshot generated_at=%q bytes=%d", generatedAt, len(stored))
	}
}

func TestRunLocalOnlyCarriesRemoteSnapshotForward(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "sample")
	createGitRepository(t, repoDir)
	registryPath := filepath.Join(root, "repos.yml")
	writeReportFile(t, root, "repos.yml", "repos:\n  sample:\n    url: https://github.com/example/sample.git\n    path: "+repoDir+"\n")
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	openPRs := 2
	topics := []string{"go"}
	private := false
	project := "sample"
	openFindings := 3
	sonar := "ERROR"
	latest := map[string]GemLatest{
		"sample-gem": {Version: "2.0.0", Released: "2026-01-01T00:00:00Z", CheckedAt: time.Now().UTC().Format(time.RFC3339)},
	}
	old := Snapshot{
		Repos: []Repository{{
			Name: "sample", HeadSHA: "oldhead", StatusSig: "oldstatus",
			GitHub:          &GitHubInfo{OpenPRs: &openPRs, Topics: &topics, Private: &private},
			SonarCloud:      &sonar,
			SemgrepFindings: &SemgrepFindings{Project: &project, Open: &openFindings},
		}},
		GemLatest: latest,
	}
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveRepoReportSnapshot(context.Background(), "old", data); err != nil {
		t.Fatal(err)
	}

	got, err := RunWithOptions(context.Background(), config.ReportConfig{RegistryPath: registryPath}, db, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	repo := got.Repos[0]
	if got.RemoteEnabled || repo.GitHub == nil || repo.GitHub.OpenPRs == nil || *repo.GitHub.OpenPRs != 2 {
		t.Fatalf("remote values not retained: snapshot=%+v repo=%+v", got, repo)
	}
	if repo.SemgrepFindings == nil || !repo.SemgrepFindings.Stale {
		t.Fatalf("Semgrep reference should be marked stale: %+v", repo.SemgrepFindings)
	}
	if got.GemLatest["sample-gem"].Version != "2.0.0" ||
		got.Summary.OpenPRs == nil || *got.Summary.OpenPRs != 2 {
		t.Fatalf("cached summary not retained: summary=%+v gems=%v", got.Summary, got.GemLatest)
	}
	if !contains(repo.GitHub.StaleFields, "open_prs") {
		t.Fatalf("GitHub fields not marked stale: %+v", repo.GitHub.StaleFields)
	}
	if got.Summary.RemoteStaleRepos != 1 {
		t.Fatalf("stale remote repo count=%d", got.Summary.RemoteStaleRepos)
	}
}

func TestRunSeedsRemoteAndGemCacheFromRubyReference(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "sample")
	createGitRepository(t, repoDir)
	writeReportFile(t, repoDir, "Gemfile.lock", `GEM
  specs:
    sample-gem (1.0.0)

DEPENDENCIES
  sample-gem
`)
	registryPath := filepath.Join(root, "repos.yml")
	writeReportFile(t, root, "repos.yml", "repos:\n  sample:\n    url: https://github.com/example/sample.git\n    path: "+repoDir+"\n")
	referencePath := filepath.Join(root, "ruby-report.json")
	writeReportFile(t, root, "ruby-report.json", `{
  "generated_at":"2026-10-17T12:00:00Z",
  "gem_latest":{"sample-gem":{"v":"2.0.0","at":"2026-01-01T00:00:00Z","checked_at":"2026-10-17T11:00:00Z"}},
  "repos":{
    "sample":{
      "github":{"topics":["go"],"private":false,"open_prs":1,"pr_titles":["Fix"],"prs":[],"ci":"CI:success","tags_without_release":[],"releases_without_tag":[],"code_scanning":0,"dependabot":0},
      "sonarcloud":"OK",
      "semgrep_findings":{"project":"sample","open":0,"severities":{}},
      "gems":{"sample-gem":"1.0.0"},
      "gem_changes":{"added":["legacy-added-gem"],"removed":[]}
    }
  }
}`)
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	got, err := RunWithOptions(context.Background(), config.ReportConfig{
		RegistryPath: registryPath, ComparisonPath: referencePath,
	}, db, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	repo := got.Repos[0]
	if repo.GitHub == nil || repo.GitHub.OpenPRs == nil || *repo.GitHub.OpenPRs != 1 ||
		repo.SonarCloud == nil || *repo.SonarCloud != "OK" {
		t.Fatalf("legacy remote values not seeded: %+v", repo)
	}
	if repo.SemgrepFindings == nil || !repo.SemgrepFindings.Stale {
		t.Fatalf("legacy Semgrep values not marked as a reference: %+v", repo.SemgrepFindings)
	}
	if got.GemLatest["sample-gem"].Version != "2.0.0" ||
		got.Comparison.OpenPRs == nil || *got.Comparison.OpenPRs != 1 {
		t.Fatalf("legacy cache/comparison not loaded: %+v", got)
	}
	if len(repo.GemChanges.Added) != 1 || repo.GemChanges.Added[0] != "legacy-added-gem" ||
		len(repo.GemChanges.Removed) != 0 {
		t.Fatalf("legacy gem-change state was not preserved: %+v", repo.GemChanges)
	}
}

func TestRunPropagatesCollectErrors(t *testing.T) {
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := Run(context.Background(), config.ReportConfig{}, db); err == nil {
		t.Fatal("expected an unconfigured report to fail")
	}
}

func TestCollectRepositoryEdgeCases(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "afile")
	writeReportFile(t, dir, "afile", "not a directory")

	noPath := collectRepository(context.Background(), dir, "no-path", repositoryEntry{}, policyFile{})
	if noPath.Error != "path is not configured" || noPath.PathExists {
		t.Fatalf("no-path repo=%+v", noPath)
	}

	fileRepo := collectRepository(context.Background(), dir, "afile", repositoryEntry{Path: file}, policyFile{})
	if fileRepo.Error != "path is not a directory" || fileRepo.PathExists {
		t.Fatalf("file repo=%+v", fileRepo)
	}

	// A plain directory without Git metadata must report an error, not crash.
	plain := filepath.Join(dir, "plain")
	if err := os.MkdirAll(plain, 0755); err != nil {
		t.Fatal(err)
	}
	plainRepo := collectRepository(context.Background(), dir, "plain", repositoryEntry{Path: "plain"}, policyFile{})
	if !plainRepo.PathExists || !contains(plainRepo.Problems, "Git metadata unavailable") {
		t.Fatalf("plain repo=%+v", plainRepo)
	}
}

func TestCollectHonoursCancelledContext(t *testing.T) {
	dir := t.TempDir()
	writeReportFile(t, dir, "repos.yml", "repos:\n  a:\n    path: "+dir+"\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Collect(ctx, config.ReportConfig{RegistryPath: filepath.Join(dir, "repos.yml")}); err == nil {
		t.Fatal("expected cancellation to abort the collection")
	}
}

func TestMarkerAndProcfileChecks(t *testing.T) {
	dir := t.TempDir()
	if got := markerStatus(dir); got != "missing" {
		t.Fatalf("marker=%q", got)
	}
	if procfileOrphan(dir) {
		t.Fatal("no Procfile.dev means no orphan")
	}
	writeReportFile(t, dir, ".engineering-memory.yml", "marker: true\n")
	if got := markerStatus(dir); got != "present" {
		t.Fatalf("marker=%q", got)
	}
	writeReportFile(t, dir, "Procfile.dev", "web: bin/server\n")
	if !procfileOrphan(dir) {
		t.Fatal("Procfile.dev without a bin/dev runner is orphaned")
	}
	writeReportFile(t, dir, "bin/dev", "#!/bin/sh\nexec foreman start -f Procfile.dev\n")
	if procfileOrphan(dir) {
		t.Fatal("bin/dev running foreman is not orphaned")
	}
}

func TestRubyVersionIssuesRequireTagAndFile(t *testing.T) {
	dir := t.TempDir()
	// No .ruby-version and no ruby tag: not a Ruby project, nothing to flag.
	issues, err := rubyVersionIssues(dir, "3.4.11", map[string][]string{}, true)
	if err != nil || len(issues) != 0 {
		t.Fatalf("issues=%v err=%v", issues, err)
	}
	// Ruby-tagged project missing the file is flagged when the policy requires it.
	issues, err = rubyVersionIssues(dir, "3.4.11", map[string][]string{"language": {"ruby"}}, true)
	if err != nil || !contains(issues, "no .ruby-version file") {
		t.Fatalf("issues=%v err=%v", issues, err)
	}
	// Vendor copies of .ruby-version are ignored.
	writeReportFile(t, dir, "vendor/x/.ruby-version", "1.9.3\n")
	issues, err = rubyVersionIssues(dir, "3.4.11", map[string][]string{"language": {"ruby"}}, false)
	if err != nil || len(issues) != 0 {
		t.Fatalf("vendor .ruby-version should be skipped: issues=%v err=%v", issues, err)
	}
	// A matching version produces no issues.
	writeReportFile(t, dir, ".ruby-version", "3.4.11\n")
	issues, err = rubyVersionIssues(dir, "3.4.11", map[string][]string{}, false)
	if err != nil || len(issues) != 0 {
		t.Fatalf("issues=%v err=%v", issues, err)
	}
}

func TestSemgrepLicenseClassification(t *testing.T) {
	dir := t.TempDir()
	if got := semgrepLicense(dir); got != "" {
		t.Fatalf("no semgrep usage should be empty, got %q", got)
	}
	// Cloud CI invocation wins over everything else.
	writeReportFile(t, dir, "Makefile", "scan:\n\tsemgrep ci\n")
	if got := semgrepLicense(dir); got != "cloud-ci" {
		t.Fatalf("semgrep ci => %q", got)
	}
	// Registry packs without cloud login.
	writeReportFile(t, dir, "Makefile", "scan:\n\tsemgrep scan --config p/security-audit\n")
	if got := semgrepLicense(dir); got != "registry" {
		t.Fatalf("registry pack => %q", got)
	}
	// Plain local scan flagged as CE.
	writeReportFile(t, dir, "Makefile", "scan:\n\tsemgrep scan --oss-only\n")
	if got := semgrepLicense(dir); got != "ce" {
		t.Fatalf("oss-only => %q", got)
	}
	// A lone .semgrep.yml without registry extends is local but unmarked.
	if err := os.Remove(filepath.Join(dir, "Makefile")); err != nil {
		t.Fatal(err)
	}
	writeReportFile(t, dir, ".semgrep.yml", "rules: []\n")
	if got := semgrepLicense(dir); got != "local-unmarked" {
		t.Fatalf("local rules => %q", got)
	}
}

func TestGitEnvironmentDisablesOptionalLocks(t *testing.T) {
	env := withoutOptionalGitLocks([]string{"A=1", "GIT_OPTIONAL_LOCKS=1", "B=2"})
	count := 0
	for _, value := range env {
		if strings.HasPrefix(value, "GIT_OPTIONAL_LOCKS=") {
			count++
			if value != "GIT_OPTIONAL_LOCKS=0" {
				t.Fatalf("env=%v", env)
			}
		}
	}
	if count != 1 || len(env) != 3 {
		t.Fatalf("env=%v", env)
	}
	env = withoutOptionalGitLocks([]string{"A=1"})
	if len(env) != 2 || env[1] != "GIT_OPTIONAL_LOCKS=0" {
		t.Fatalf("env=%v", env)
	}
}

func TestLoadComparisonAggregatesLegacyRemoteMetrics(t *testing.T) {
	dir := t.TempDir()
	data := `{
  "generated_at": "2026-10-17 17:10:44 +0200",
  "repos": {
    "one": {
      "scanners": ["sonar", "semgrep"],
      "sonarcloud": "OK",
      "semgrep_findings": {"open": 4},
      "github": {"open_prs": 2}
    },
    "two": {
      "scanners": ["sonar", "semgrep"],
      "sonarcloud": "ERROR",
      "sonarcloud_failures": ["coverage 1<80"],
      "semgrep_findings": {"open": 2},
      "forgejo": {"open_prs": 1}
    },
    "three": {
      "github": {"open_prs": null}
    }
  }
}`
	path := filepath.Join(dir, "data.json")
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	got := loadComparison(path)
	if !got.Available || got.GeneratedAt != "2026-10-17 17:10:44 +0200" {
		t.Fatalf("comparison=%+v", got)
	}
	if got.SonarReportedProjects != 2 || got.SonarBlocked == nil || *got.SonarBlocked != 1 {
		t.Fatalf("Sonar summary=%+v", got)
	}
	if got.SemgrepReportedProjects != 2 || got.SemgrepOpenFindings == nil || *got.SemgrepOpenFindings != 6 {
		t.Fatalf("Semgrep summary=%+v", got)
	}
	if got.SemgrepBlocked == nil || *got.SemgrepBlocked != 2 {
		t.Fatalf("Semgrep blocked repositories=%+v", got)
	}
	if got.OpenPRRepositories != 2 || got.OpenPRs == nil || *got.OpenPRs != 3 {
		t.Fatalf("PR summary=%+v", got)
	}
}

func TestLoadComparisonDistinguishesMissingMetrics(t *testing.T) {
	if got := loadComparison(""); got.Available || got.Error != "not configured" {
		t.Fatalf("unconfigured comparison=%+v", got)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")
	if err := os.WriteFile(path, []byte(`{"generated_at":"now","repos":{"one":{}}}`), 0644); err != nil {
		t.Fatal(err)
	}
	got := loadComparison(path)
	if !got.Available || got.OpenPRs != nil || got.SemgrepOpenFindings != nil {
		t.Fatalf("missing remote metrics must be unknown, not zero: %+v", got)
	}
}

func TestRepositoryURLsOnlyExposeSafeWebLinks(t *testing.T) {
	display, link := repositoryURLs("https://github.com/example/repo.git")
	if display != "https://github.com/example/repo.git" || link != "https://github.com/example/repo" {
		t.Fatalf("display=%q link=%q", display, link)
	}
	display, link = repositoryURLs("https://token:secret@example.com/private.git")
	if display != "https://example.com/private.git" || link != "" {
		t.Fatalf("credential URL display=%q link=%q", display, link)
	}
	if _, link := repositoryURLs("javascript:alert(1)"); link != "" {
		t.Fatalf("unsafe scheme became a web link: %q", link)
	}
	if link := safeHTTPLink("javascript:alert(1)"); link != "" {
		t.Fatalf("unsafe API URL became a web link: %q", link)
	}
}

func TestShippedYAMLExamplesMatchReportSchema(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate report test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	registryPath := filepath.Join(root, "example-configs", "repos.example.yml")
	policyPath := filepath.Join(root, "example-configs", "policy.example.yml")
	repos, policy, err := loadSources(config.ReportConfig{
		RegistryPath: registryPath,
		PolicyPath:   policyPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := repos["example-project"]
	if !ok || entry.Path != "../path/to/example-project" || entry.RepoCommit != "0123456" {
		t.Fatalf("example registry entry=%+v", entry)
	}
	if policy.Versions.Ruby != "3.4.x" || policy.Versions.Node != "22" ||
		policy.Versions.Python == "" || policy.Versions.Go == "" ||
		policy.Versions.Bundler == "" || policy.Versions.Rubygems == "" ||
		!policy.Checks.RequireRubyVersionFile || policy.Checks.RequireNodeVersionFile {
		t.Fatalf("example policy=%+v", policy)
	}
	exception := policy.Checks.TagReleaseExceptions["tag-only-site"]
	if len(exception.TagsWithoutRelease) == 0 || exception.TagsWithoutRelease[0] != "*" ||
		policy.Gems.Slots["pagination"].Prefer != "pagy" ||
		policy.Gems.Watch["retired-gem"] == "" ||
		policy.Gems.DriftOK["shared-gem"] == "" ||
		policy.Gems.DormantOK["reviewed-gem"].ReviewAfter != 6 ||
		policy.NPM.DriftOK["shared-tool"] == "" ||
		policy.NPM.Watch["retired-cli"] == "" {
		t.Fatalf("full policy example parsed incorrectly: %+v", policy)
	}
}

// yamlKeySet flattens every yaml tag in a schema type — including inline
// structs and struct types reached through maps — into a key set.
func yamlKeySet(typ reflect.Type, out map[string]bool) {
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Map || typ.Kind() == reflect.Slice {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name != "" && name != "-" {
			out[name] = true
		}
		yamlKeySet(f.Type, out)
	}
}

// TestShippedYAMLExamplesCoverAllSchemaKeys keeps the shipped examples a
// complete reference: every schema key must appear in the file text —
// commented keys count, since the examples are the documentation.
func TestShippedYAMLExamplesCoverAllSchemaKeys(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate report test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	for _, tc := range []struct {
		file   string
		schema reflect.Type
	}{
		{"repos.example.yml", reflect.TypeOf(registryFile{})},
		{"policy.example.yml", reflect.TypeOf(policyFile{})},
	} {
		data, err := os.ReadFile(filepath.Join(root, "example-configs", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		keys := map[string]bool{} // nosemgrep: trailofbits.go.iterate-over-empty-map.iterate-over-empty-map — yamlKeySet fills it
		yamlKeySet(tc.schema, keys)
		var missing []string
		for key := range keys {
			if !strings.Contains(string(data), key+":") {
				missing = append(missing, key)
			}
		}
		if len(missing) > 0 {
			t.Errorf("%s does not document schema keys: %s", tc.file, strings.Join(missing, ", "))
		}
	}
}

func TestGemVersionComparisonAndRubyGemsRequirements(t *testing.T) {
	tests := []struct {
		left, right string
		want        int
	}{
		{left: "3.10.0", right: "3.9.9", want: 1},
		{left: "1.0.0.rc1", right: "1.0.0", want: -1},
		{left: "1.2", right: "1.2.0", want: 0},
	}
	for _, test := range tests {
		left, ok := parseGemVersion(test.left)
		if !ok {
			t.Fatalf("invalid test version %q", test.left)
		}
		right, ok := parseGemVersion(test.right)
		if !ok {
			t.Fatalf("invalid test version %q", test.right)
		}
		if got := compareGemVersions(left, right); got != test.want {
			t.Errorf("compare(%s,%s)=%d want %d", test.left, test.right, got, test.want)
		}
	}

	for _, test := range []struct {
		requirement, version string
		want                 bool
	}{
		{requirement: "~> 4.0", version: "4.9.1", want: true},
		{requirement: "~> 4.0", version: "5.0.0", want: false},
		{requirement: "~> 4.0.1", version: "4.0.9", want: true},
		{requirement: "~> 4.0.1", version: "4.1.0", want: false},
		{requirement: ">= 3.9, < 5.0", version: "4.2.0", want: true},
		{requirement: ">= 3.9, < 5.0", version: "5.0.0", want: false},
	} {
		version, ok := parseGemVersion(test.version)
		if !ok {
			t.Fatalf("invalid test version %q", test.version)
		}
		if got := gemRequirementSatisfied(test.requirement, version); got != test.want {
			t.Errorf("%s satisfied by %s = %v want %v", test.requirement, test.version, got, test.want)
		}
	}
}

func TestRubyReferenceDoesNotClearExistingGoStaleMarkers(t *testing.T) {
	currentPRs, referencePRs := 1, 2
	current := &Repository{
		Name: "sample",
		GitHub: &GitHubInfo{
			OpenPRs:     &currentPRs,
			StaleFields: []string{"open_prs"},
		},
	}
	reference := &Repository{
		Name:   "sample",
		GitHub: &GitHubInfo{OpenPRs: &referencePRs},
	}
	merged := mergeReferenceRemote(current, reference)
	if merged.GitHub == nil || merged.GitHub.OpenPRs == nil || *merged.GitHub.OpenPRs != 1 ||
		!contains(merged.GitHub.StaleFields, "open_prs") {
		t.Fatalf("stale Go remote value was replaced/cleared: %+v", merged.GitHub)
	}
}

func TestGemInventoryPolicyAndChanges(t *testing.T) {
	root := t.TempDir()
	writeReportFile(t, root, "Gemfile", "gem 'rails'\ngem 'kaminari'\n")
	writeReportFile(t, root, "Gemfile.lock", `GEM
  specs:
    actionpack (7.1.0)
    kaminari (1.2.2)
    liquid (4.0.4)
    rails (7.1.0)
      actionpack (>= 7.1)
      liquid (~> 4.0)

DEPENDENCIES
  kaminari
  rails (~> 7.1)
`)
	writeReportFile(t, root, "tools/Gemfile.lock", `GEM
  specs:
    liquid (5.0.0)

DEPENDENCIES
  another-gem
`)
	writeReportFile(t, root, "vendor/Gemfile.lock", `GEM
  specs:
    ignored (1.0.0)
`)
	writeReportFile(t, root, ".ruby-lsp/Gemfile.lock", `GEM
  specs:
    hidden-tooling-gem (1.0.0)
`)
	writeReportFile(t, root, "sample.gemspec", `Gem::Specification.new do |spec|
  spec.add_runtime_dependency "my-plugin", ">= 1.0", "< 2.0"
  spec.add_development_dependency "rspec", "~> 3.0"
end
`)

	policy := policyFile{}
	policy.Gems.Watch = map[string]string{"kaminari": "migrate to pagy", "liquid": "review"}
	policy.Gems.Slots = map[string]GemSlot{
		"pagination": {Prefer: "pagy", OK: []string{"kaminari"}},
	}
	data := collectGemInventory(root)
	old := &Repository{Gems: map[string]string{"rails": "7.0.0", "old-gem": "1.0.0"}}
	repo := Repository{}
	applyGemPolicy(&repo, data, policy, old)

	if repo.Gems["rails"] != "7.1.0" || repo.Gems["liquid"] != "4.0.4" {
		t.Fatalf("gems=%v", repo.Gems)
	}
	if len(repo.GemConflicts) != 1 || !strings.HasPrefix(repo.GemConflicts[0], "liquid:") {
		t.Fatalf("gem conflicts=%v", repo.GemConflicts)
	}
	if repo.GemsWatch["kaminari"] != "migrate to pagy" || len(repo.GemNotes) != 2 {
		t.Fatalf("watch=%v notes=%v", repo.GemsWatch, repo.GemNotes)
	}
	if repo.GemspecDeps["my-plugin"].Req != ">= 1.0, < 2.0" ||
		!repo.GemspecDeps["rspec"].Dev {
		t.Fatalf("gemspec dependencies=%v", repo.GemspecDeps)
	}
	if repo.LockfileMissing || !contains(repo.GemChanges.Added, "kaminari") ||
		!contains(repo.GemChanges.Removed, "old-gem") {
		t.Fatalf("lockfile=%v gem changes=%+v", repo.LockfileMissing, repo.GemChanges)
	}
	if len(repo.GemEdges["liquid"]) != 1 ||
		repo.GemEdges["liquid"][0].By != "rails" ||
		repo.GemEdges["liquid"][0].Req != "~> 4.0" {
		t.Fatalf("dependency edges=%v", repo.GemEdges)
	}
	if _, found := repo.Gems["ignored"]; found {
		t.Fatal("vendor lockfile should be skipped")
	}
	if _, found := repo.Gems["hidden-tooling-gem"]; found {
		t.Fatal("hidden tooling lockfile should be skipped like the Ruby glob")
	}
}

func TestGemFleetDriftOutdatedBlockersAndDormantExceptions(t *testing.T) {
	repos := []Repository{
		{
			Name: "alpha",
			Gems: map[string]string{
				"allowed-drift": "1.2.0", "patch-drift": "1.0.1",
				"blocked": "1.0.0", "quiet": "0.4.0", "held": "4.0.0",
			},
			GemEdges: map[string][]GemEdge{
				"blocked": {{By: "parent-gem", Req: "< 2.0", Src: "Gemfile.lock"}},
			},
		},
		{
			Name: "beta",
			Gems: map[string]string{
				"allowed-drift": "2.0.0", "patch-drift": "1.0.2", "blocked": "1.0.0",
			},
		},
	}
	policy := policyFile{}
	policy.Gems.DriftOK = map[string]string{"allowed-drift": "accepted split"}
	policy.Gems.DormantOK = map[string]DormantGemOK{
		"quiet": {Reviewed: "2020-01-01", ReviewAfter: 6, Reason: "spec-complete"},
	}
	policy.Gems.Hold = map[string]string{"held": "waiting on upstream support"}
	latest := map[string]GemLatest{
		"blocked": {
			Version: "2.0.0", Released: "2026-01-01T00:00:00Z",
		},
		"patch-drift": {
			Version: "1.0.3", Released: "2026-01-01T00:00:00Z",
		},
		"quiet": {
			Version: "0.4.0", Released: "2020-01-01T00:00:00Z",
		},
		"held": {
			Version: "5.0.0", Released: "2026-01-01T00:00:00Z",
		},
	}
	fleet := buildFleet(repos, policy, latest, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC))
	if _, ok := fleet.GemDriftOK["allowed-drift"]; !ok {
		t.Fatalf("allowed drift missing: %+v", fleet.GemDriftOK)
	}
	if fleet.GemDrift["patch-drift"].Severity != "info" {
		t.Fatalf("patch drift=%+v", fleet.GemDrift["patch-drift"])
	}
	if item := fleet.GemOutdated["blocked"]; item.Severity != "constrained" ||
		len(item.BlockedBy) != 1 || !strings.Contains(item.BlockedBy[0], "parent-gem") {
		t.Fatalf("blocked outdated gem=%+v", item)
	}
	if item := fleet.GemStale["quiet"]; item.Severity != "warn" ||
		!strings.Contains(item.Note, "review overdue") {
		t.Fatalf("stale dormant gem=%+v", item)
	}
	if item := fleet.GemHeld["held"]; item.Reason != "waiting on upstream support" ||
		item.Latest != "5.0.0" {
		t.Fatalf("held gem=%+v", item)
	}
	if _, flagged := fleet.GemOutdated["held"]; flagged {
		t.Fatalf("held gem also filed as outdated: %+v", fleet.GemOutdated)
	}
}

// Fault injection: a torn stored snapshot is derived state — the run
// regenerates it. previousSnapshot must degrade to "no prior snapshot"
// rather than block every refresh on the corruption.
func TestPreviousSnapshotDegradesOnCorruption(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Empty table → no snapshot.
	if old, err := previousSnapshot(ctx, db); err != nil || old != nil {
		t.Fatalf("empty: old=%v err=%v", old, err)
	}
	// Corrupt payload (write path validates JSON, so inject via raw SQL).
	if _, err := db.Exec(`INSERT INTO repo_report_snapshot(id,generated_at,data_json)
VALUES(1,'2026-10-21T00:00:00Z','{torn')`); err != nil {
		t.Fatal(err)
	}
	if old, err := previousSnapshot(ctx, db); err != nil || old != nil {
		t.Fatalf("corrupt snapshot must degrade to nil, got old=%v err=%v", old, err)
	}
	// A well-formed snapshot still loads.
	if err := db.SaveRepoReportSnapshot(ctx, "2026-10-21T01:00:00Z", []byte(`{"configured":true}`)); err != nil {
		t.Fatal(err)
	}
	old, err := previousSnapshot(ctx, db)
	if err != nil || old == nil || !old.Configured {
		t.Fatalf("valid snapshot: old=%+v err=%v", old, err)
	}
}
