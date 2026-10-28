package report

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"time"

	"knowledge-ba46-flow/internal/config"

	"resty.dev/v3"
)

func TestGitHubCollectorsReuseTokenAndParseReportFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/repository/") &&
			!strings.HasPrefix(r.URL.Path, "/api/v2/") &&
			r.Header.Get("Authorization") != "Bearer test-gh-token" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/repos/example/project":
			fmt.Fprint(w, `{"topics":["go","reporting"],"private":false,"default_branch":"main"}`)
		case "/repos/example/project/pulls":
			fmt.Fprint(w, `[{"number":7,"title":"Report update","html_url":"https://github.com/example/project/pull/7"}]`)
		case "/repos/example/project/actions/runs":
			fmt.Fprint(w, `{"workflow_runs":[{"name":"CI","conclusion":"success"}]}`)
		case "/repos/example/project/tags":
			fmt.Fprint(w, `[{"name":"v1.0.0"},{"name":"v2.0.0"}]`)
		case "/repos/example/project/releases":
			fmt.Fprint(w, `[{"tag_name":"v1.0.0"}]`)
		case "/repos/example/project/code-scanning/alerts":
			fmt.Fprint(w, `[{},{}]`)
		case "/repos/example/project/dependabot/alerts":
			fmt.Fprint(w, `[]`)
		case "/repository/github/example/project/badge":
			fmt.Fprint(w, `<svg><text x="1">B+</text></svg>`)
		case "/api/v2/github/example/repos/project/totals/":
			if r.URL.Query().Get("branch") != "main" {
				t.Errorf("codecov branch=%q", r.URL.Query().Get("branch"))
			}
			fmt.Fprint(w, `{"totals":{"coverage":96.7,"lines":120,"hits":116}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &remoteHTTP{
		client:         resty.NewWithClient(server.Client()),
		githubBase:     server.URL,
		codeFactorBase: server.URL,
		codecovBase:    server.URL,
	}

	info := client.githubInfo(context.Background(), "example", "project", "test-gh-token")
	if info.OpenPRs == nil || *info.OpenPRs != 1 ||
		info.CI == nil || *info.CI != "CI:success" ||
		info.Private == nil || *info.Private {
		t.Fatalf("github info=%+v", info)
	}
	if info.DefaultBranch == nil || *info.DefaultBranch != "main" {
		t.Fatalf("default branch=%+v", info.DefaultBranch)
	}
	if info.Topics == nil || strings.Join(*info.Topics, ",") != "go,reporting" {
		t.Fatalf("topics=%v", info.Topics)
	}
	if info.TagsWithoutRelease == nil || strings.Join(*info.TagsWithoutRelease, ",") != "v2.0.0" ||
		info.ReleasesWithoutTag == nil || len(*info.ReleasesWithoutTag) != 0 {
		t.Fatalf("tag/release fields=%+v", info)
	}
	if info.CodeScanning == nil || *info.CodeScanning != 2 ||
		info.Dependabot == nil || *info.Dependabot != 0 {
		t.Fatalf("alerts=%+v", info)
	}
	if grade := client.codeFactor(context.Background(), "example", "project"); grade == nil || *grade != "B+" {
		t.Fatalf("CodeFactor grade=%v", grade)
	}
	coverage, err := client.codecovCoverage(context.Background(), "example", "project", "main")
	if err != nil || coverage == nil || *coverage != 96.7 {
		t.Fatalf("Codecov coverage=%v err=%v", coverage, err)
	}
	// A repo unknown to Codecov (404) reports an error, not zero coverage.
	if coverage, err := client.codecovCoverage(context.Background(), "example", "absent", "main"); err == nil || coverage != nil {
		t.Fatalf("unknown repo must not report coverage: %v err=%v", coverage, err)
	}
}

func TestForgejoCollectorsParsePullsTagsAndActions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token forgejo-token" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/api/v1/repos/owner/project/pulls":
			fmt.Fprint(w, `[{"number":3,"title":"Fix","html_url":"https://forgejo.example/owner/project/pulls/3"}]`)
		case "/api/v1/repos/owner/project/tags":
			fmt.Fprint(w, `[{"name":"v1"},{"name":"v2"}]`)
		case "/api/v1/repos/owner/project/releases":
			fmt.Fprint(w, `[{"tag_name":"v1"}]`)
		case "/api/v1/repos/owner/project/actions/tasks":
			fmt.Fprint(w, `{"total_count":9}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	info := newRemoteHTTP().forgejoInfo(
		context.Background(), server.URL+"/api/v1", "owner", "project", requestAuth{token: "forgejo-token"},
	)
	if info.OpenPRs == nil || *info.OpenPRs != 1 || info.ActionRuns == nil || *info.ActionRuns != 9 {
		t.Fatalf("Forgejo info=%+v", info)
	}
	if info.TagsWithoutRelease == nil || strings.Join(*info.TagsWithoutRelease, ",") != "v2" {
		t.Fatalf("tag/release fields=%+v", info)
	}
}

func TestRemoteFailuresCarryOnlyMissingFieldsAsStale(t *testing.T) {
	oldPRs, currentPRs := 2, 0
	oldTopics := []string{"go"}
	currentTopics := []string{}
	old := &GitHubInfo{OpenPRs: &oldPRs, Topics: &oldTopics}
	current := &GitHubInfo{OpenPRs: nil, Topics: &currentTopics}

	got := carryGitHubStale(current, old)
	if got.OpenPRs == nil || *got.OpenPRs != 2 || got.Topics == nil || len(*got.Topics) != 0 {
		t.Fatalf("merged GitHub data=%+v", got)
	}
	if len(got.StaleFields) != 1 || got.StaleFields[0] != "open_prs" {
		t.Fatalf("stale fields=%v", got.StaleFields)
	}

	oldPRs, currentPRs = 3, 0
	oldForgejo := &ForgejoInfo{OpenPRs: &oldPRs}
	currentForgejo := &ForgejoInfo{OpenPRs: &currentPRs}
	mergedForgejo := carryForgejoStale(currentForgejo, oldForgejo)
	if mergedForgejo.OpenPRs == nil || *mergedForgejo.OpenPRs != 0 || len(mergedForgejo.StaleFields) != 0 {
		t.Fatalf("successful zero-count Forgejo response overwritten: %+v", mergedForgejo)
	}
}

func TestUnavailableGitHubDoesNotAbortReportAndKeepsStaleData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client := &remoteHTTP{
		client:         resty.NewWithClient(server.Client()),
		githubBase:     server.URL,
		codeFactorBase: server.URL,
		sonarCloudBase: server.URL,
		codecovBase:    server.URL,
	}
	openPRs := 4
	oldGrade := "C"
	oldSonar := "ERROR"
	oldCoverage := 88.5
	old := &Repository{
		GitHub:     &GitHubInfo{OpenPRs: &openPRs},
		CodeFactor: &oldGrade,
		SonarCloud: &oldSonar,
		Codecov:    &oldCoverage,
	}
	repo := Repository{Name: "sample"}
	refreshRemoteRepository(
		context.Background(), client, &repo, t.TempDir(),
		repositoryEntry{URL: "https://github.com/example/project.git"},
		old, "mock-token", false, nil,
	)
	if repo.GitHub == nil || repo.GitHub.OpenPRs == nil || *repo.GitHub.OpenPRs != 4 ||
		!contains(repo.GitHub.StaleFields, "open_prs") {
		t.Fatalf("GitHub outage erased stale PRs: %+v", repo.GitHub)
	}
	if repo.CodeFactor == nil || *repo.CodeFactor != "C" || !repo.CodeFactorStale {
		t.Fatalf("CodeFactor outage erased stale grade: %+v", repo)
	}
	if repo.SonarCloud == nil || *repo.SonarCloud != "ERROR" || !repo.SonarStale {
		t.Fatalf("Sonar outage erased stale gate: %+v", repo)
	}
	if repo.Codecov == nil || *repo.Codecov != 88.5 || !repo.CodecovStale {
		t.Fatalf("Codecov outage erased stale coverage: %+v", repo)
	}
}

func TestSonarCollectorParsesGateFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/qualitygates/project_status" || r.URL.Query().Get("projectKey") != "sample" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"projectStatus":{"status":"ERROR","conditions":[{"status":"ERROR","metricKey":"new_security_rating","actualValue":"3","comparator":"GT","errorThreshold":"1"}]}}`)
	}))
	defer server.Close()
	client := &remoteHTTP{client: resty.NewWithClient(server.Client()), sonarCloudBase: server.URL}

	gate := client.sonarGate(context.Background(), server.URL, "sample")
	if gate == nil || gate.Status != "ERROR" || len(gate.Failures) != 1 ||
		gate.Failures[0] != "new_security_rating 3>1" {
		t.Fatalf("sonar gate=%+v", gate)
	}
}

func TestSemgrepCollectorMapsProjectsByURLAndCountsSeverities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/deployments":
			fmt.Fprint(w, `{"deployments":[{"slug":"workspace"}]}`)
		case "/api/v1/deployments/workspace/projects":
			fmt.Fprint(w, `{"projects":[{"name":"custom-project","url":"https://github.com/example/project"}]}`)
		case "/api/v1/deployments/workspace/findings":
			fmt.Fprint(w, `{"findings":[{"severity":"high","repository":{"name":"custom-project"}},{"severity":"medium","repository":{"name":"custom-project"}},{"severity":"low","repository":{"name":"other"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &remoteHTTP{client: resty.NewWithClient(server.Client()), semgrepBase: server.URL + "/api/v1"}

	data, err := client.semgrepPlatform(context.Background(), "test-token")
	if err != nil {
		t.Fatal(err)
	}
	repos := []Repository{{Name: "project", SemgrepLicense: "ce"}}
	registry := map[string]repositoryEntry{
		"project": {URL: "https://github.com/example/project.git"},
	}
	refreshSemgrepFindings(data, repos, registry)
	got := repos[0].SemgrepFindings
	if got == nil || got.Project == nil || *got.Project != "custom-project" ||
		got.Open == nil || *got.Open != 2 || got.Severities["high"] != 1 || got.Severities["medium"] != 1 {
		t.Fatalf("Semgrep findings=%+v", got)
	}
}

func TestRubygemsCacheChoosesHighestVersionAndNewestRelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/versions/demo.json" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `[
{"number":"1.9.0","created_at":"2026-01-01T00:00:00Z","prerelease":false},
{"number":"1.10.0","created_at":"2025-12-01T00:00:00Z","prerelease":false},
{"number":"2.0.0.pre","created_at":"2026-02-01T00:00:00Z","prerelease":true}
]`)
	}))
	defer server.Close()
	client := &remoteHTTP{client: resty.NewWithClient(server.Client()), rubygemsBase: server.URL + "/versions"}
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	got, err := fetchGemLatest(context.Background(), client, "demo", now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "1.10.0" || got.Released != "2026-01-01T00:00:00Z" ||
		got.CheckedAt != now.Format(time.RFC3339) {
		t.Fatalf("gem latest=%+v", got)
	}
}

func TestRemoteCredentialSourcesPreferEnvironment(t *testing.T) {
	t.Setenv("GH_TOKEN", "github-token")
	t.Setenv("GITHUB_TOKEN", "other-github-token")
	t.Setenv("FORGEJO_TOKEN", "forgejo-token")
	t.Setenv("SEMGREP_APP_TOKEN", "semgrep-token")
	if got := githubToken(context.Background()); got != "github-token" {
		t.Fatalf("GitHub token=%q", got)
	}
	if got := forgejoAuth(context.Background(), "forgejo.example"); got.token != "forgejo-token" {
		t.Fatalf("Forgejo auth=%+v", got)
	}
	if got := semgrepToken(); got != "semgrep-token" {
		t.Fatalf("Semgrep token=%q", got)
	}
}

func TestForgejoCredentialsAreRestrictedToConfiguredHost(t *testing.T) {
	t.Setenv("FORGEJO_HOST", "")
	if host, ok := configuredForgejoHost(defaultForgejoHost); !ok || host != defaultForgejoHost {
		t.Fatalf("default Forgejo host=%q trusted=%v", host, ok)
	}
	if _, ok := configuredForgejoHost("forgejo.attacker.example"); ok {
		t.Fatal("unconfigured Forgejo host was trusted")
	}
	t.Setenv("FORGEJO_HOST", "git.example.invalid")
	if host, ok := configuredForgejoHost("git.example.invalid"); !ok || host != "git.example.invalid" {
		t.Fatalf("configured Forgejo host=%q trusted=%v", host, ok)
	}
}

func TestGemLatestCacheHonorsTTLAndForce(t *testing.T) {
	now := time.Now().UTC()
	cached := GemLatest{Version: "1.0.0", CheckedAt: now.Add(-gemLatestTTL / 2).Format(time.RFC3339)}
	if cacheStale(cached, now) {
		t.Fatal("fresh gem cache was stale")
	}
	cached.CheckedAt = now.Add(-gemLatestTTL - time.Minute).Format(time.RFC3339)
	if !cacheStale(cached, now) {
		t.Fatal("expired gem cache was fresh")
	}

	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, `[{"number":"2.0.0","created_at":"2026-01-01T00:00:00Z","prerelease":false}]`)
	}))
	defer server.Close()
	client := &remoteHTTP{client: resty.NewWithClient(server.Client()), rubygemsBase: server.URL}
	installed := map[string]map[string][]string{"demo": {"1.0.0": {"sample"}}}
	cache := map[string]GemLatest{
		"demo": {Version: "1.0.0", Released: "2025-01-01T00:00:00Z", CheckedAt: now.Format(time.RFC3339)},
	}
	refreshGemLatest(context.Background(), client, installed, cache, false)
	if calls != 0 {
		t.Fatalf("fresh cache triggered %d requests", calls)
	}
	refreshed := refreshGemLatest(context.Background(), client, installed, cache, true)
	if calls != 1 || refreshed["demo"].Version != "2.0.0" {
		t.Fatalf("forced refresh calls=%d latest=%+v", calls, refreshed["demo"])
	}
}

func TestForgejoAuthPrefersEnvToken(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "  test-token  ")
	auth := forgejoAuth(t.Context(), "forgejo.example")
	if auth.token != "test-token" {
		t.Fatalf("env token should win: %+v", auth)
	}
	// An empty env falls through to git-credential; without a stored
	// credential the result is an empty auth, not an error or a hang.
	t.Setenv("FORGEJO_TOKEN", "")
	auth = forgejoAuth(t.Context(), "nonexistent.invalid")
	if auth.token != "" || auth.password != "" {
		t.Fatalf("unexpected credentials for unknown host: %+v", auth)
	}
}

func TestForgejoCredentialsCachesPerHost(t *testing.T) {
	t.Setenv("FORGEJO_TOKEN", "tok")
	h := &remoteHTTP{}
	first := h.forgejoCredentials(t.Context(), "forgejo.example")
	second := h.forgejoCredentials(t.Context(), "forgejo.example")
	if first.token != "tok" || second.token != "tok" {
		t.Fatalf("cached credentials wrong: %+v %+v", first, second)
	}
}

func TestGitHubTokenEnvPriority(t *testing.T) {
	t.Setenv("GH_TOKEN", "gh-tok")
	t.Setenv("GITHUB_TOKEN", "github-tok")
	if got := githubToken(t.Context()); got != "gh-tok" {
		t.Fatalf("GH_TOKEN should take precedence: %q", got)
	}
	t.Setenv("GH_TOKEN", "")
	if got := githubToken(t.Context()); got != "github-tok" && got != "" {
		// Without env vars the helper falls through to `gh auth token`; accept
		// whatever that yields on this machine but never a wrong env value.
		t.Fatalf("unexpected token: %q", got)
	}
}

func TestSemgrepTokenPrefersEnv(t *testing.T) {
	t.Setenv("SEMGREP_APP_TOKEN", "  sg-tok  ")
	if got := semgrepToken(); got != "sg-tok" {
		t.Fatalf("env token should be trimmed and honored: %q", got)
	}
}

func TestMinCoveragePolicyFlagsLowCodecov(t *testing.T) {
	coverage := 62.4
	repo := Repository{Name: "app", Codecov: &coverage}
	min := 80.0
	got := deriveProblems(repo, "", TagReleaseException{}, &min)
	if len(got) != 1 || !strings.Contains(got[0], "codecov coverage") {
		t.Fatalf("expected a coverage problem: %v", got)
	}
	// Above the floor or without a configured floor there is no finding.
	coverage = 92.0
	if got := deriveProblems(repo, "", TagReleaseException{}, &min); len(got) != 0 {
		t.Fatalf("coverage above floor flagged: %v", got)
	}
	coverage = 10.0
	if got := deriveProblems(repo, "", TagReleaseException{}, nil); len(got) != 0 {
		t.Fatalf("no policy floor should not flag coverage: %v", got)
	}
	// Missing data is "not reported", never a false finding.
	repo.Codecov = nil
	if got := deriveProblems(repo, "", TagReleaseException{}, &min); len(got) != 0 {
		t.Fatalf("absent coverage flagged as low: %v", got)
	}
}

func TestNPMProblemsReportsLockfileAndWatched(t *testing.T) {
	repo := Repository{
		Name: "app",
		NPMProjects: []NPMProject{{
			Path:            "web",
			LockfileMissing: true,
			Watched:         map[string]string{"left-pad": "dropped"},
		}},
	}
	got := npmProblems(repo)
	if len(got) != 2 {
		t.Fatalf("expected lockfile + watched problems: %v", got)
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "lockfile") || !strings.Contains(joined, "left-pad") {
		t.Fatalf("problem text incomplete: %v", got)
	}
}

func TestGemAgeSeverityBoundaries(t *testing.T) {
	for age, want := range map[int]string{0: "dim", 23: "dim", 24: "info", 47: "info", 48: "warn", 120: "warn"} {
		if got := gemAgeSeverity(age); got != want {
			t.Errorf("gemAgeSeverity(%d)=%q want %q", age, got, want)
		}
	}
}

func TestRefreshRemoteRepositoryForRepoHonoursSelectionFilter(t *testing.T) {
	client := newRemoteHTTP()
	repo := Repository{Name: "skipped", CodeFactor: stringPointer("A"), CodeFactorStale: true}
	registry := map[string]repositoryEntry{"skipped": {Path: "repo"}}
	refreshRemoteRepositoryForRepo(t.Context(), client, &repo, t.TempDir(), registry, nil, "",
		RunOptions{Repository: "other"}, nil)
	// A filtered repo is left completely untouched.
	if repo.CodeFactor == nil || !repo.CodeFactorStale {
		t.Fatalf("filtered repo should be untouched: %+v", repo)
	}
}

func TestRefreshRemoteRepositoryForRepoClearsRemotelessEntries(t *testing.T) {
	client := newRemoteHTTP()
	repo := Repository{
		Name:            "local-only",
		GitHub:          &GitHubInfo{AuthUnavailable: true},
		Forgejo:         &ForgejoInfo{},
		CodeFactor:      stringPointer("A"),
		CodeFactorStale: true,
		SonarCloud:      stringPointer("ERROR"),
	}
	old := map[string]*Repository{"local-only": {SonarCloud: stringPointer("OK"), SonarStale: true}}
	registry := map[string]repositoryEntry{"local-only": {Path: "repo"}}
	// No remote URL: the default branch clears all remote-derived state, and
	// sonar falls back to the previous snapshot's values (marked stale).
	refreshRemoteRepositoryForRepo(t.Context(), client, &repo, t.TempDir(), registry, old, "", RunOptions{}, nil)
	if repo.GitHub != nil || repo.Forgejo != nil || repo.CodeFactor != nil || repo.CodeFactorStale {
		t.Fatalf("remoteless repo should have cleared remote state: %+v", repo)
	}
	if repo.SonarCloud == nil || *repo.SonarCloud != "OK" || !repo.SonarStale {
		t.Fatalf("sonar should carry the stale previous value: %+v", repo)
	}
}

func TestRefreshSemgrepDataWithoutTokenMarksStale(t *testing.T) {
	t.Setenv("SEMGREP_APP_TOKEN", "")
	t.Setenv("HOME", t.TempDir()) // no ~/.semgrep/settings.yml
	snapshot := &Snapshot{Repos: []Repository{
		{Name: "a", SemgrepFindings: &SemgrepFindings{}},
		{Name: "b"},
		{Name: "c", SemgrepLicense: "cloud-ci"},
	}}
	refreshSemgrepData(t.Context(), newRemoteHTTP(), snapshot, nil)
	if !snapshot.Repos[0].SemgrepFindings.Stale {
		t.Fatal("findings should be marked stale when no semgrep token is configured")
	}
	// A repo without findings must not gain one.
	if snapshot.Repos[1].SemgrepFindings != nil {
		t.Fatal("markSemgrepStale should not fabricate findings")
	}
	// A cloud-ci repo records why platform findings are unavailable.
	got := snapshot.Repos[2].SemgrepFindings
	if got == nil || got.Error != "semgrep token is not configured" || !got.Stale {
		t.Fatalf("cloud-ci repo should record the failure reason: %+v", got)
	}
}

func TestRefreshRemoteDataEmptySnapshot(t *testing.T) {
	snapshot := &Snapshot{}
	if err := refreshRemoteData(t.Context(), config.ReportConfig{}, nil, snapshot, nil, RunOptions{}); err != nil {
		t.Fatalf("empty snapshot should be a no-op: %v", err)
	}
}

func TestNodeReleaseKnownMatchesProjectAndPolicy(t *testing.T) {
	snapshot := &Snapshot{
		Repos:        []Repository{{NPMProjects: []NPMProject{{NodeVersion: "20.1.0"}}}},
		NodeReleases: NodeReleaseCache{ByMajor: map[string]NodeRelease{"20": {Version: "20.1.0"}}},
	}
	if !nodeReleaseKnown(snapshot, policyFile{}) {
		t.Fatal("a project version matching a cached release should be known")
	}
	snapshot.NodeReleases.ByMajor = map[string]NodeRelease{}
	if nodeReleaseKnown(snapshot, policyFile{}) {
		t.Fatal("an uncached major should read as unknown")
	}
	var policy policyFile
	policy.Versions.Node = "22.0.0"
	snapshot.NodeReleases.ByMajor = map[string]NodeRelease{"22": {Version: "22.0.0"}}
	if !nodeReleaseKnown(snapshot, policy) {
		t.Fatal("an exact policy pin matching a cached release should be known")
	}
}
