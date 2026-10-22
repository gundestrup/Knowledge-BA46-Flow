package report

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"resty.dev/v3"
)

func TestNPMVersionAndRangeSemantics(t *testing.T) {
	tests := []struct {
		left, right string
		want        int
	}{
		{left: "10.0.0", right: "9.9.9", want: 1},
		{left: "1.2.3-beta.2", right: "1.2.3", want: -1},
		{left: "1.2.3", right: "1.2.3+build.4", want: 0},
	}
	for _, test := range tests {
		left, lok := parseNPMVersion(test.left)
		right, rok := parseNPMVersion(test.right)
		if !lok || !rok || compareNPMVersions(left, right) != test.want {
			t.Errorf("compare(%q,%q)=(%v,%v), want %d", test.left, test.right, lok, rok, test.want)
		}
	}
	for _, test := range []struct {
		rangeExpr, candidate string
		want                 bool
	}{
		{rangeExpr: ">=22", candidate: "22.0.0", want: true},
		{rangeExpr: ">=22 <25", candidate: "24.1.0", want: true},
		{rangeExpr: ">=22 <25", candidate: "25.0.0", want: false},
		{rangeExpr: "^0.2.3", candidate: "0.2.9", want: true},
		{rangeExpr: "^0.2.3", candidate: "0.3.0", want: false},
		{rangeExpr: "~1.2.3", candidate: "1.2.9", want: true},
		{rangeExpr: "~1.2.3", candidate: "1.3.0", want: false},
		{rangeExpr: "1.2.3 - 2.0.0", candidate: "2.0.0", want: true},
		{rangeExpr: "1.2.3 - 2.0.0", candidate: "2.0.1", want: false},
		{rangeExpr: "22.x || >=24", candidate: "23.0.0", want: false},
		{rangeExpr: "22.x || >=24", candidate: "24.0.0", want: true},
	} {
		if got := npmRangeSatisfies(test.rangeExpr, test.candidate); got != test.want {
			t.Errorf("range %q accepts %s = %v, want %v", test.rangeExpr, test.candidate, got, test.want)
		}
	}
}

func TestNPMInventoryReadsLockfilesNodePinsAndNestedProjects(t *testing.T) {
	root := t.TempDir()
	writeReportFile(t, root, ".nvmrc", "22\n")
	writeReportFile(t, root, "package.json", `{
  "name":"sample-app",
  "engines":{"node":">=22"},
  "dependencies":{"alpha":"^1.0.0"},
  "devDependencies":{"typescript":"~5.5.0"}
}`)
	writeReportFile(t, root, "package-lock.json", `{
  "name":"sample-app",
  "lockfileVersion":3,
  "packages":{
    "":{"name":"sample-app","dependencies":{"alpha":"^1.0.0"},"devDependencies":{"typescript":"~5.5.0"}},
    "node_modules/alpha":{"version":"1.2.0","dependencies":{"shared":"^2.0.0"}},
    "node_modules/alpha/node_modules/shared":{"version":"2.1.0"},
    "node_modules/typescript":{"version":"5.5.4"}
  }
}`)
	writeReportFile(t, root, "tools/package.json", `{"name":"tooling","devDependencies":{"shared":"^3.0.0"}}`)
	writeReportFile(t, root, "tools/package-lock.json", `{
  "lockfileVersion":3,
  "packages":{
    "":{"name":"tooling","devDependencies":{"shared":"^3.0.0"}},
    "node_modules/shared":{"version":"3.1.0"}
  }
}`)
	writeReportFile(t, root, "node_modules/ignored/package.json", `{"name":"ignored"}`)

	policy := policyFile{}
	policy.Versions.Node = "22"
	policy.Checks.RequireNodeVersionFile = true
	projects, issues := collectNPMProjects(root, policy)
	if len(projects) != 2 || len(issues) != 0 {
		t.Fatalf("projects=%+v issues=%v", projects, issues)
	}
	main := projects[0]
	tooling := projects[1]
	if main.Path != "" || main.Name != "sample-app" || main.NodeVersion != "22" ||
		main.NodeEngine != ">=22" || main.LockfileVersion != 3 {
		t.Fatalf("main project=%+v", main)
	}
	if main.Packages["alpha"] != "1.2.0" || main.Packages["shared"] != "2.1.0" ||
		main.Direct["alpha"] != "^1.0.0" || len(main.Edges["shared"]) == 0 {
		t.Fatalf("main package state=%+v", main)
	}
	if filepath.Base(tooling.Path) != "tools" {
		t.Fatalf("projects not sorted by manifest path: %+v", projects)
	}
}

func TestNPMNodePolicyReportsMissingLockAndRuntimePin(t *testing.T) {
	root := t.TempDir()
	writeReportFile(t, root, "package.json", `{"name":"tooling","devDependencies":{"eslint":"^10.0.0"}}`)
	policy := policyFile{}
	policy.Versions.Node = "22"
	policy.Checks.RequireNodeVersionFile = true

	projects, issues := collectNPMProjects(root, policy)
	if len(projects) != 1 || !projects[0].LockfileMissing {
		t.Fatalf("missing npm lockfile was not detected: %+v", projects)
	}
	if len(issues) != 2 ||
		!slices.Contains(issues, ": no .nvmrc or .node-version file") ||
		!slices.Contains(issues, ": package.json without lockfile") {
		t.Fatalf("missing Node pin issues=%v", issues)
	}
}

func TestNPMFleetReportsVersionDriftOutdatedAndBlockers(t *testing.T) {
	repos := []Repository{
		{
			Name: "alpha",
			NPMProjects: []NPMProject{{
				Path: "", Packages: map[string]string{"shared": "1.2.0", "blocked": "1.0.0", "held": "1.0.0"},
				PackageVersions: map[string][]string{"shared": {"1.2.0"}, "blocked": {"1.0.0"}, "held": {"1.0.0"}},
				Edges:           map[string][]NPMEdge{"blocked": {{By: "parent", Req: "<2.0.0", Project: ""}}},
			}},
		},
		{
			Name: "beta",
			NPMProjects: []NPMProject{{
				Path: "", Packages: map[string]string{"shared": "2.0.0", "blocked": "1.0.0"},
				PackageVersions: map[string][]string{"shared": {"2.0.0"}, "blocked": {"1.0.0"}},
			}},
		},
	}
	policy := policyFile{}
	policy.NPM.DriftOK = map[string]string{"shared": "separate release trains"}
	policy.NPM.Hold = map[string]string{"held": "pinned until migration lands"}
	latest := map[string]NPMLatest{
		"blocked": {Version: "2.0.0"},
		"shared":  {Version: "3.0.0"},
		"held":    {Version: "2.0.0"},
	}
	fleet := buildNPMFleet(repos, policy, latest, NodeReleaseCache{ByMajor: map[string]NodeRelease{}})
	if _, ok := fleet.PackageDriftOK["shared"]; !ok {
		t.Fatalf("allowed npm drift missing: %+v", fleet)
	}
	blocked := fleet.PackageOutdated["blocked"]
	if blocked.Severity != "constrained" || len(blocked.BlockedBy) != 1 ||
		!strings.Contains(blocked.BlockedBy[0], "parent <2.0.0") {
		t.Fatalf("blocked outdated npm package=%+v", blocked)
	}
	if fleet.PackageOutdated["shared"].Severity != "warn" {
		t.Fatalf("major npm update severity=%+v", fleet.PackageOutdated["shared"])
	}
	held := fleet.PackageHeld["held"]
	if held.Reason != "pinned until migration lands" || held.Latest != "2.0.0" {
		t.Fatalf("held npm package=%+v", held)
	}
	if _, flagged := fleet.PackageOutdated["held"]; flagged {
		t.Fatalf("held package also filed as outdated: %+v", fleet.PackageOutdated)
	}
}

func TestNPMLatestCacheUsesRegistryAndRetainsPreviousOnFailure(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !strings.HasSuffix(r.URL.Path, "/alpha") {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"dist-tags":{"latest":"2.3.0"}}`)
	}))
	defer server.Close()
	client := &remoteHTTP{client: resty.NewWithClient(server.Client()), npmRegistryBase: server.URL}
	installed := map[string]map[string][]string{"alpha": {"1.0.0": {"sample"}}}
	now := time.Now().UTC()
	cache := map[string]NPMLatest{
		"alpha": {Version: "1.0.0", CheckedAt: now.Add(-npmLatestTTL - time.Minute).Format(time.RFC3339)},
	}
	got := refreshNPMLatest(context.Background(), client, installed, cache, true)
	if calls != 1 || got["alpha"].Version != "2.3.0" {
		t.Fatalf("registry refresh calls=%d data=%v", calls, got)
	}

	server.Close()
	got = refreshNPMLatest(context.Background(), client, installed, got, true)
	if got["alpha"].Version != "2.3.0" || !got["alpha"].Stale {
		t.Fatalf("failed registry request discarded cached value: %v", got)
	}
}

func TestNodeReleaseCacheAndPinnedRuntimeOutdatedCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dist/index.json" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `[
{"version":"v22.13.0","lts":"Jod"},
{"version":"v22.14.0","lts":"Jod"},
{"version":"v23.0.0","lts":false},
{"version":"v24.0.0-rc.1","lts":false}
]`)
	}))
	defer server.Close()
	client := &remoteHTTP{client: resty.NewWithClient(server.Client()), nodeDistIndex: server.URL + "/dist/index.json"}

	cache := refreshNodeReleaseCache(context.Background(), client, NodeReleaseCache{}, false)
	if cache.ByMajor["22"].Version != "22.14.0" || cache.ByMajor["22"].LTS != "Jod" ||
		cache.ByMajor["23"].Version != "23.0.0" {
		t.Fatalf("Node release cache=%+v", cache)
	}
	repos := []Repository{{
		Name: "sample",
		NPMProjects: []NPMProject{
			{Path: "", NodeVersion: "22.13.0"},
			{Path: "tools", NodeVersion: "22"},
		},
	}}
	fleet := buildNPMFleet(repos, policyFile{}, map[string]NPMLatest{}, cache)
	if outdated := fleet.NodeOutdated["sample"]; outdated.Pinned != "22.13.0" ||
		outdated.Latest != "22.14.0" || outdated.LTS != "Jod" {
		t.Fatalf("Node pin not reported outdated: %+v", fleet.NodeOutdated)
	}
	if _, found := fleet.NodeOutdated["sample/tools"]; found {
		t.Fatalf("major-channel pin should not be treated as an exact outdated patch: %+v", fleet.NodeOutdated)
	}
}

func TestNodeReleaseFailureKeepsLastKnownVersions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nodejs.org unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	cache := NodeReleaseCache{
		ByMajor: map[string]NodeRelease{"22": {Version: "22.15.0", LTS: "Jod"}},
	}
	client := &remoteHTTP{client: resty.NewWithClient(server.Client()), nodeDistIndex: server.URL}
	got := refreshNodeReleaseCache(context.Background(), client, cache, true)
	if !got.Stale || got.ByMajor["22"].Version != "22.15.0" {
		t.Fatalf("Node release failure lost stale cache: %+v", got)
	}
}
