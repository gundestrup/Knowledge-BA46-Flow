package report

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"knowledge-ba46-flow/internal/config"

	resty "resty.dev/v3"
)

func TestMergeRepositoriesSelection(t *testing.T) {
	registry := map[string]repositoryEntry{"keep": {}, "one": {}}
	old := map[string]*Repository{
		"keep": {Name: "keep", Path: "old-keep"},
		"gone": {Name: "gone", Path: "old-gone"},
		"one":  {Name: "one", Path: "old-one"},
	}
	local := map[string]*Repository{
		"one":   {Name: "one", Path: "new-one"},
		"fresh": {Name: "fresh", Path: "new-fresh"},
	}

	full := mergeRepositories(registry, local, old, "")
	if len(full) != 2 || full[0].Name != "fresh" || full[1].Name != "one" {
		t.Fatalf("full selection=%v", full)
	}

	merged := mergeRepositories(registry, local, old, "one")
	got := map[string]string{}
	for _, repo := range merged {
		got[repo.Name] = repo.Path
	}
	if len(got) != 3 || got["keep"] != "old-keep" || got["one"] != "new-one" || got["fresh"] != "new-fresh" {
		t.Fatalf("selective merge=%v", got)
	}
}

func TestMergeReferenceRemoteCarriesStaleData(t *testing.T) {
	open := 3
	topics := []string{"a"}
	grade, gate := "B", "OK"
	findingsOpen := 7
	reference := &Repository{
		Name: "demo",
		Gems: map[string]string{"rails": "7.0.0"},
		GitHub: &GitHubInfo{
			Topics:  &topics,
			OpenPRs: &open,
		},
		Forgejo: &ForgejoInfo{
			OpenPRs: &open,
		},
		CodeFactor: &grade,
		SonarCloud: &gate,
		SemgrepFindings: &SemgrepFindings{
			Open: &findingsOpen,
		},
	}

	if clone := mergeReferenceRemote(nil, reference); clone == reference || clone.Name != "demo" {
		t.Fatalf("nil current should clone reference, got %v", clone)
	}

	merged := mergeReferenceRemote(&Repository{Name: "demo"}, reference)
	if merged.Gems["rails"] != "7.0.0" {
		t.Fatalf("gems not carried: %v", merged.Gems)
	}
	if merged.GitHub == nil || len(merged.GitHub.StaleFields) == 0 || merged.GitHub.OpenPRs == nil || *merged.GitHub.OpenPRs != 3 {
		t.Fatalf("github merge: %+v", merged.GitHub)
	}
	if merged.Forgejo == nil || len(merged.Forgejo.StaleFields) == 0 {
		t.Fatalf("forgejo merge: %+v", merged.Forgejo)
	}
	if merged.CodeFactor == nil || *merged.CodeFactor != "B" || !merged.CodeFactorStale {
		t.Fatalf("codefactor merge: %+v stale=%v", merged.CodeFactor, merged.CodeFactorStale)
	}
	if merged.SonarCloud == nil || *merged.SonarCloud != "OK" || !merged.SonarStale {
		t.Fatalf("sonar merge: %+v stale=%v", merged.SonarCloud, merged.SonarStale)
	}
	if merged.SemgrepFindings == nil || !merged.SemgrepFindings.Stale {
		t.Fatalf("semgrep merge: %+v", merged.SemgrepFindings)
	}
}

func TestMergeGitHubReferenceFillsOnlyMissing(t *testing.T) {
	open := 5
	current := &GitHubInfo{OpenPRs: &open, StaleFields: []string{"topics"}}
	reference := &GitHubInfo{OpenPRs: &[]int{9}[0], Topics: &[]string{"stale"}}

	merged := mergeGitHubReference(current, reference)
	if *merged.OpenPRs != 5 {
		t.Fatalf("fresh OpenPRs overwritten: %v", *merged.OpenPRs)
	}
	if merged.Topics == nil || (*merged.Topics)[0] != "stale" {
		t.Fatalf("missing topics should be carried, got %+v", merged.Topics)
	}
	if len(merged.StaleFields) != 1 || merged.StaleFields[0] != "topics" {
		t.Fatalf("stale fields=%v", merged.StaleFields)
	}
	if got := mergeGitHubReference(nil, reference); got == nil || len(got.StaleFields) == 0 {
		t.Fatalf("nil current should carry all as stale: %+v", got)
	}
}

func TestMergeForgejoReferenceCarries(t *testing.T) {
	runs := 2
	reference := &ForgejoInfo{OpenPRs: &[]int{4}[0], ActionRuns: &runs}
	merged := mergeForgejoReference(nil, reference)
	if merged.OpenPRs == nil || *merged.OpenPRs != 4 || merged.ActionRuns == nil || *merged.ActionRuns != 2 {
		t.Fatalf("forgejo carry: %+v", merged)
	}
	if len(merged.StaleFields) != 2 {
		t.Fatalf("stale fields=%v", merged.StaleFields)
	}
	current := &ForgejoInfo{OpenPRs: &[]int{1}[0]}
	out := mergeForgejoReference(current, reference)
	if *out.OpenPRs != 1 || *out.ActionRuns != 2 {
		t.Fatalf("partial merge: %+v", out)
	}
}

func TestMarkStaleHelpers(t *testing.T) {
	repos := []Repository{{SemgrepFindings: &SemgrepFindings{}}}
	markSemgrepStale(repos, "boom")
	if !repos[0].SemgrepFindings.Stale {
		t.Fatal("markSemgrepStale did not flag findings")
	}
	cache := map[string]NPMLatest{"a": {Version: "1.0.0"}}
	markNPMLatestStale(cache)
	if !cache["a"].Stale {
		t.Fatal("markNPMLatestStale did not flag cache entry")
	}
}

func TestCodeFactorNeedsAttention(t *testing.T) {
	for grade, want := range map[string]bool{
		"A+": false, "A": false, "B-": false,
		"C": true, "D": true, "F": true,
		"": false, "n/a": false,
	} {
		if got := codeFactorNeedsAttention(grade); got != want {
			t.Errorf("codeFactorNeedsAttention(%q)=%v want %v", grade, got, want)
		}
	}
}

func TestValidateRepository(t *testing.T) {
	registry := filepath.Join(t.TempDir(), "registry.yml")
	if err := os.WriteFile(registry, []byte("repos:\n  demo:\n    path: /x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.ReportConfig{RegistryPath: registry}
	if err := ValidateRepository(cfg, "demo"); err != nil {
		t.Fatalf("known repo rejected: %v", err)
	}
	if err := ValidateRepository(cfg, "other"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown repo should fail, got %v", err)
	}
	if err := ValidateRepository(config.ReportConfig{}, "demo"); err == nil {
		t.Fatal("missing registry path should fail")
	}
}

func TestExactNodePinHelpers(t *testing.T) {
	repos := []Repository{{NPMProjects: []NPMProject{{NodeVersion: "22.1.0"}}}}
	if !hasExactNodePins(repos) {
		t.Fatal("exact pin not detected")
	}
	repos[0].NPMProjects[0].NodeVersion = "22"
	if hasExactNodePins(repos) {
		t.Fatal("partial pin reported as exact")
	}
	for policy, want := range map[string]bool{
		"22.1.0": true, "v22.1.0": true, "node-20.0.0": true,
		"22": false, "lts": false, "*": false,
	} {
		if got := exactNodePolicy(policy); got != want {
			t.Errorf("exactNodePolicy(%q)=%v want %v", policy, got, want)
		}
	}
}

func TestNPMCacheStale(t *testing.T) {
	now := time.Now()
	fresh := now.Format(time.RFC3339)
	failedOld := now.Add(-2 * time.Hour).Format(time.RFC3339)
	checkedOld := now.Add(-npmLatestTTL - time.Minute).Format(time.RFC3339)
	cases := []struct {
		name  string
		entry NPMLatest
		want  bool
	}{
		{"fresh check", NPMLatest{CheckedAt: fresh}, false},
		{"no check", NPMLatest{}, true},
		{"stale recent failure", NPMLatest{Stale: true, FailedAt: fresh}, false},
		{"stale old failure", NPMLatest{Stale: true, FailedAt: failedOld}, true},
		{"stale never failed", NPMLatest{Stale: true}, true},
		{"check past TTL", NPMLatest{CheckedAt: checkedOld}, true},
		{"stale bad timestamp", NPMLatest{Stale: true, FailedAt: "bogus"}, true},
	}
	for _, tc := range cases {
		if got := npmCacheStale(tc.entry, now); got != tc.want {
			t.Errorf("%s: npmCacheStale=%v want %v", tc.name, got, tc.want)
		}
	}
}

func TestLockNodeDependenciesTree(t *testing.T) {
	deps := map[string]npmLockNode{
		"left-pad": {
			Version:  "1.3.0",
			Requires: map[string]string{"chalk": "^5.0.0"},
		},
		"chalk": {
			Version: "5.3.0",
			Dependencies: map[string]npmLockNode{
				"nested": {Version: "2.0.0", Requires: map[string]string{"chalk": "^5.0.0"}},
			},
		},
	}
	project := &NPMProject{Edges: map[string][]NPMEdge{}}
	versions := map[string]map[string]bool{}
	lockNodeDependencies(deps, "app", "apps/x", project, versions)

	if !versions["left-pad"]["1.3.0"] || !versions["chalk"]["5.3.0"] || !versions["nested"]["2.0.0"] {
		t.Fatalf("versions=%v", versions)
	}
	edges := project.Edges["chalk"]
	if len(edges) != 2 {
		t.Fatalf("chalk edges=%v", edges)
	}
	seen := map[string]bool{}
	for _, edge := range edges {
		if edge.Req != "^5.0.0" || edge.Project != "apps/x" {
			t.Fatalf("edge=%+v", edge)
		}
		seen[edge.By] = true
	}
	// v1 lockfile edges are attributed to the dependency-tree parent: the
	// top-level requirement reads By="app", the nested one By="chalk".
	if !seen["app"] || !seen["chalk"] {
		t.Fatalf("edge by set=%v", seen)
	}
	// Re-running must not duplicate identical edges.
	lockNodeDependencies(deps, "app", "apps/x", project, versions)
	if len(project.Edges["chalk"]) != 2 {
		t.Fatalf("edges duplicated: %v", project.Edges["chalk"])
	}
}

func TestParseNPMPartialEdges(t *testing.T) {
	cases := []struct {
		raw       string
		ok        bool
		specified int
		wildcard  bool
	}{
		{"22", true, 1, false},
		{"22.x", true, 1, true},
		{"22.*", true, 1, true},
		{"1.2.3", true, 3, false},
		{"v1.2.3", true, 3, false},
		{"1.2.3-beta.1", true, 3, false},
		{"1.2.3+build", true, 3, false},
		{"x", true, 0, true},
		{"*", true, 0, true},
		{"01", false, 0, false},
		{"", true, 0, true},
		{"1.2.3.4", false, 0, false},
		{"abc", false, 0, false},
		{"1.x.y", true, 1, true},
	}
	for _, tc := range cases {
		got, ok := parseNPMPartial(tc.raw)
		if ok != tc.ok {
			t.Errorf("parseNPMPartial(%q) ok=%v want %v", tc.raw, ok, tc.ok)
			continue
		}
		if ok && (got.specified != tc.specified || got.wildcard != tc.wildcard) {
			t.Errorf("parseNPMPartial(%q)=%+v want specified=%d wildcard=%v", tc.raw, got, tc.specified, tc.wildcard)
		}
	}
}

func TestReportSmallHelpers(t *testing.T) {
	if got := gemNames([]string{"a", "b"}); got != "a/b" {
		t.Fatalf("gemNames=%q", got)
	}
	if p := stringPointer("x"); p == nil || *p != "x" {
		t.Fatal("stringPointer")
	}
	if got := removeStaleField([]string{"a", "b", "a"}, "a"); len(got) != 1 || got[0] != "b" {
		t.Fatalf("removeStaleField=%v", got)
	}
	if !(requestAuth{}).empty() || (requestAuth{token: "x"}).empty() || (requestAuth{username: "u"}).empty() {
		t.Fatal("requestAuth.empty")
	}
	if got := copyGemLatest(&Snapshot{GemLatest: map[string]GemLatest{"g": {Version: "1"}}}); got["g"].Version != "1" {
		t.Fatalf("copyGemLatest=%v", got)
	}
	if got := copyGemLatest(nil); len(got) != 0 {
		t.Fatalf("copyGemLatest(nil)=%v", got)
	}
	if got := copyNPMLatest(&Snapshot{NPMLatest: map[string]NPMLatest{"n": {Version: "2"}}}); got["n"].Version != "2" {
		t.Fatalf("copyNPMLatest=%v", got)
	}
	if got := copyNodeReleaseCache(nil); got.ByMajor == nil {
		t.Fatal("copyNodeReleaseCache(nil) should init ByMajor")
	}
	repo := Repository{Gems: map[string]string{"b": "1", "c": "1"}}
	old := Repository{Gems: map[string]string{"a": "1", "b": "1"}}
	setGemChanges(&repo, &old)
	if len(repo.GemChanges.Added) != 1 || repo.GemChanges.Added[0] != "c" ||
		len(repo.GemChanges.Removed) != 1 || repo.GemChanges.Removed[0] != "a" {
		t.Fatalf("gem changes=%+v", repo.GemChanges)
	}
	setGemChanges(&repo, nil)
	if repo.GemChanges.Added == nil || repo.GemChanges.Removed == nil {
		t.Fatal("nil old should produce empty (non-nil) changes")
	}
}

func TestScannerConfigs(t *testing.T) {
	dir := t.TempDir()
	if got := scannerConfigs(dir); len(got) != 0 {
		t.Fatalf("empty repo scanners=%v", got)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".github", "dependabot.yml"), []byte("version: 2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := scannerConfigs(dir); len(got) != 1 || got[0] != "dependabot" {
		t.Fatalf("dependabot scanners=%v", got)
	}
	if err := os.Rename(
		filepath.Join(dir, ".github", "dependabot.yml"),
		filepath.Join(dir, ".github", "dependabot.yaml"),
	); err != nil {
		t.Fatal(err)
	}
	if got := scannerConfigs(dir); len(got) != 1 || got[0] != "dependabot" {
		t.Fatalf("dependabot.yaml scanners=%v", got)
	}
	if err := os.WriteFile(filepath.Join(dir, ".github", "dependabot.yml"), []byte("version: 2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := scannerConfigs(dir); len(got) != 1 || got[0] != "dependabot" {
		t.Fatalf("dependabot dedupe scanners=%v", got)
	}
}

func TestSonarStatusKinds(t *testing.T) {
	okServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"projectStatus":{"status":"OK","conditions":[]}}`))
	}))
	defer okServer.Close()
	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer failServer.Close()

	ctx := context.Background()
	withProps := t.TempDir()
	if err := os.WriteFile(filepath.Join(withProps, "sonar-project.properties"),
		[]byte("sonar.projectKey = my-proj\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withoutProps := t.TempDir()

	newH := func(cloud, local string) *remoteHTTP {
		return &remoteHTTP{
			client:         resty.NewWithClient(okServer.Client()),
			sonarCloudBase: cloud,
			localSonarBase: local,
		}
	}

	if got := newH(okServer.URL, okServer.URL).sonarStatus(ctx, withProps, "org/repo"); got == nil || got.Kind != "cloud" || !strings.Contains(got.URL, "my-proj") {
		t.Fatalf("cloud result=%+v", got)
	}
	if got := newH(failServer.URL, okServer.URL).sonarStatus(ctx, withProps, "org/repo"); got == nil || got.Kind != "local" || !strings.Contains(got.URL, "my-proj") {
		t.Fatalf("local result=%+v", got)
	}
	if got := newH(failServer.URL, failServer.URL).sonarStatus(ctx, withProps, "org/repo"); got != nil {
		t.Fatalf("both-down result=%+v", got)
	}
	if got := newH(okServer.URL, failServer.URL).sonarStatus(ctx, withoutProps, "org/repo"); got == nil || got.Kind != "autoscan" {
		t.Fatalf("autoscan result=%+v", got)
	}
	if got := newH(okServer.URL, okServer.URL).sonarStatus(ctx, withoutProps, ""); got != nil {
		t.Fatalf("no key/slug result=%+v", got)
	}
}

func TestNPMVersionSpreadSeverity(t *testing.T) {
	cases := []struct {
		versions []string
		want     string
	}{
		{[]string{"1.0.0", "1.0.5"}, "info"},
		{[]string{"1.0.0", "1.2.0"}, "warn"},
		{[]string{"1.0.0", "2.0.0"}, "warn"},
		{[]string{"bogus", "also-bogus"}, "info"},
		{[]string{"1.0.0", "bogus"}, "info"},
		{nil, "info"},
	}
	for _, tc := range cases {
		if got := npmVersionSpreadSeverity(tc.versions); got != tc.want {
			t.Errorf("npmVersionSpreadSeverity(%v)=%q want %q", tc.versions, got, tc.want)
		}
	}
}

func TestAppendUniqueEdgeDedupes(t *testing.T) {
	edge := GemEdge{By: "a", Req: "~>1", Src: "lock"}
	edges := appendUnique(nil, edge)
	if len(edges) != 1 {
		t.Fatalf("append=%v", edges)
	}
	edges = appendUnique(edges, edge)
	if len(edges) != 1 {
		t.Fatalf("duplicate appended: %v", edges)
	}
	edges = appendUnique(edges, GemEdge{By: "b", Req: "~>1", Src: "lock"})
	if len(edges) != 2 {
		t.Fatalf("distinct edge rejected: %v", edges)
	}
}
