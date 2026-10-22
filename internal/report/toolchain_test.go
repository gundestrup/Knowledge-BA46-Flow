package report

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"resty.dev/v3"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
)

// eolCycles is a compact endoflife.date fixture: two supported cycles (newest
// first), one EOL cycle, one not-yet-released cycle, and an LTS window that
// has opened plus one that has not.
const eolCycles = `[
	{"cycle":"3.16","releaseDate":"2099-10-01","eol":false,"latest":"3.16.0a1"},
	{"cycle":"24","releaseDate":"2025-05-06","eol":"2028-04-30","lts":"2025-10-28","latest":"24.22.0"},
	{"cycle":"26","releaseDate":"2026-05-05","eol":"2029-04-30","lts":"2099-10-28","latest":"26.12.0"},
	{"cycle":"22","releaseDate":"2024-04-24","eol":"2027-04-30","lts":"2024-10-29","latest":"22.23.3"},
	{"cycle":"20","releaseDate":"2023-04-18","eol":"2026-01-01","lts":"2023-10-24","latest":"20.20.2"}
]`

func toolchainTestClient(t *testing.T, handler http.HandlerFunc) (*remoteHTTP, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := newRemoteHTTP()
	client.client = resty.NewWithClient(server.Client())
	client.endOfLifeBase = server.URL + "/api"
	client.rubygemsBase = server.URL + "/versions"
	return client, server
}

func TestPickCycleChannels(t *testing.T) {
	var cycles []eolCycle
	if err := json.Unmarshal([]byte(eolCycles), &cycles); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)

	stable, err := pickCycle(cycles, "stable", now)
	if err != nil || stable != "26.12.0" {
		t.Fatalf("stable=%q err=%v — want newest released cycle", stable, err)
	}
	lts, err := pickCycle(cycles, "lts", now)
	if err != nil || lts != "24.22.0" {
		t.Fatalf("lts=%q err=%v — 26's LTS window has not opened yet", lts, err)
	}
	floor, err := pickCycle(cycles, "eol-floor", now)
	if err != nil || floor != "22.23.3" {
		t.Fatalf("eol-floor=%q err=%v — want oldest supported cycle", floor, err)
	}
	if _, err := pickCycle(cycles, "bogus", now); err == nil {
		t.Fatal("unknown channel must fail")
	}
}

func TestResolvePolicyVersionsFetchesChannels(t *testing.T) {
	calls := map[string]int{}
	client, _ := toolchainTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls[r.URL.Path]++
		switch r.URL.Path {
		case "/api/nodejs.json":
			fmt.Fprint(w, eolCycles)
		case "/versions/bundler.json":
			fmt.Fprint(w, `[{"number":"4.0.24.pre1","prerelease":true},{"number":"4.0.23","prerelease":false}]`)
		default:
			http.NotFound(w, r)
		}
	})
	var policy policyFile
	policy.Versions.Ruby = "3.4.x"
	policy.Versions.Node = "lts"
	policy.Versions.Bundler = "stable"
	policy.Versions.Go = "eol-floor"

	resolved, pins := resolvePolicyVersions(t.Context(), client, policy, nil, true, false)

	if resolved["Ruby"] != "3.4.x" || pins["Ruby"].Resolved != "3.4.x" {
		t.Fatalf("literal ruby should pass through: %+v", pins["Ruby"])
	}
	if resolved["Node"] != "24.22.0" || pins["Node"].Source != "endoflife.date" {
		t.Fatalf("node lts resolved=%q pin=%+v", resolved["Node"], pins["Node"])
	}
	if resolved["Bundler"] != "4.0.23" || pins["Bundler"].Source != "rubygems.org" {
		t.Fatalf("bundler stable resolved=%q pin=%+v (prerelease must be skipped)", resolved["Bundler"], pins["Bundler"])
	}
	if resolved["Go"] != "" || pins["Go"].Resolved != "" || pins["Go"].Note == "" {
		t.Fatalf("failed fetch should resolve empty with a note: %+v", pins["Go"])
	}
	if pins["Python"].Policy != "" {
		t.Fatalf("unset policy should produce an empty pin: %+v", pins["Python"])
	}
}

func TestResolvePolicyVersionsCachesAndMarksStale(t *testing.T) {
	calls := 0
	client, _ := toolchainTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		fmt.Fprint(w, eolCycles)
	})
	var policy policyFile
	policy.Versions.Node = "lts"

	previous := map[string]PolicyPin{
		"Node": {Policy: "lts", Resolved: "24.0.0", Source: "endoflife.date",
			CheckedAt: time.Now().UTC().Format(time.RFC3339)},
	}
	resolved, pins := resolvePolicyVersions(t.Context(), client, policy, previous, true, false)
	if calls != 0 || resolved["Node"] != "24.0.0" {
		t.Fatalf("fresh cache should be reused: calls=%d resolved=%q", calls, resolved["Node"])
	}

	// Local-only runs carry the last resolution forward marked stale.
	resolved, pins = resolvePolicyVersions(t.Context(), client, policy, pins, false, false)
	if calls != 0 || resolved["Node"] != "24.0.0" || !pins["Node"].Stale {
		t.Fatalf("local-only should carry stale pin: calls=%d pin=%+v", calls, pins["Node"])
	}

	// Never resolved + local-only: empty resolution with an explanatory note.
	resolved, pins = resolvePolicyVersions(t.Context(), client, policy, nil, false, false)
	if resolved["Node"] != "" || pins["Node"].Note == "" {
		t.Fatalf("unresolved channel should explain itself: %+v", pins["Node"])
	}

	// A policy change invalidates the cached pin for that product.
	policy.Versions.Node = "stable"
	resolved, _ = resolvePolicyVersions(t.Context(), client, policy, previous, false, false)
	if resolved["Node"] != "" {
		t.Fatalf("changed policy must not reuse another channel's pin: %q", resolved["Node"])
	}
}

func TestToolchainIssuesDetectsPins(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(".python-version", "3.15.0\n")
	write("go.mod", "module example\n\ngo 1.27\n\ntoolchain go1.27.3\n")
	write("app/Gemfile.lock", "GEM\n  specs:\n\nBUNDLED WITH\n   4.0.20\n")
	write(".tool-versions", "nodejs 24.22.0\nrubygems 4.0.23\npython 3.14.8\nruby 3.4.10\n")
	write(".github/workflows/ci.yml", "jobs:\n  test:\n    steps:\n      - uses: ruby/setup-ruby@v1\n        with:\n          rubygems: 4.0.22\n")
	write("vendor/ignored/.python-version", "3.10.0\n")

	pins, issues := toolchainIssues(dir, func() policyFile {
		var policy policyFile
		policy.Versions.Ruby = "3.5.x"
		// Node is set on purpose: the .tool-versions nodejs pin below must
		// NOT produce a toolchain issue — externalCheck keeps it with
		// nodeVersionIssues. Removing that flag fails this test.
		policy.Versions.Node = ">=26"
		policy.Versions.Python = ">=3.15"
		policy.Versions.Go = ">=1.27.3"
		policy.Versions.Bundler = ">=4.0.23"
		policy.Versions.Rubygems = ">=4.0.23"
		return policy
	}())

	// .python-version 3.15.0 satisfies >=3.15, the go.mod toolchain line and
	// the .tool-versions rubygems pin satisfy their floors, and nodejs pins
	// stay with nodeVersionIssues — so these must be the only issues.
	want := []string{
		".tool-versions: ruby 3.4.10 does not satisfy 3.5.x",
		".tool-versions: python 3.14.8 does not satisfy >=3.15",
		"go.mod: go 1.27 does not satisfy >=1.27.3",
		"app/Gemfile.lock: bundler 4.0.20 does not satisfy >=4.0.23",
		".github/workflows/ci.yml: rubygems 4.0.22 does not satisfy >=4.0.23",
	}
	slices.Sort(issues)
	slices.Sort(want)
	if !slices.Equal(issues, want) {
		t.Fatalf("issues=%v, want %v", issues, want)
	}
	for _, pin := range pins {
		if strings.HasPrefix(pin.File, "vendor/") {
			t.Fatalf("vendored pin must not be detected: %+v", pin)
		}
	}
}

// TestToolchainProductAccessors guards the declarative table: every product's
// set must write the field its get reads, and entries must not collide — a
// future adapter whose set targets the wrong Versions field fails here
// instead of silently dropping resolved values.
func TestToolchainProductAccessors(t *testing.T) {
	policy := &policyFile{}
	for _, product := range toolchainProducts {
		product.set(policy, "9.9.9-"+product.field)
	}
	for _, product := range toolchainProducts {
		if got := product.get(policy); got != "9.9.9-"+product.field {
			t.Errorf("%s get = %q after set — accessor mismatch", product.field, got)
		}
	}
}

// TestRunResolvesChannelIntoSnapshot exercises the full plumbing: a channel
// policy, a seeded prior resolution, a local-only run that carries it stale,
// and node checks evaluated against the carried value.
func TestRunResolvesChannelIntoSnapshot(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "sample")
	createGitRepository(t, repoDir)
	writeReportFile(t, repoDir, ".nvmrc", "22.0.0\n")
	writeReportFile(t, repoDir, "package.json", `{"name":"sample","version":"1.0.0"}`)
	registryPath := filepath.Join(root, "repos.yml")
	writeReportFile(t, root, "repos.yml", "repos:\n  sample:\n    path: "+repoDir+"\n")
	writeReportFile(t, root, "policy.yml", "versions:\n  node: \"lts\"\n")

	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	old := Snapshot{PolicyVersions: map[string]PolicyPin{
		"Node": {Policy: "lts", Resolved: "24.0.0", Source: "endoflife.date",
			CheckedAt: time.Now().UTC().Format(time.RFC3339)},
	}}
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveRepoReportSnapshot(t.Context(), "old", data); err != nil {
		t.Fatal(err)
	}

	got, err := RunWithOptions(t.Context(), config.ReportConfig{
		RegistryPath: registryPath,
		PolicyPath:   filepath.Join(root, "policy.yml"),
	}, db, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pin := got.PolicyVersions["Node"]
	if pin.Resolved != "24.0.0" || !pin.Stale {
		t.Fatalf("local-only run should carry the pin stale: %+v", pin)
	}
	if !contains(got.Repos[0].NodeIssues, ": Node pin 22.0.0 does not satisfy 24.0.0") {
		t.Fatalf("node checks must evaluate the carried resolution: %v", got.Repos[0].NodeIssues)
	}
}

// TestNodeVersionInToolVersions covers asdf .tool-versions node pins.
func TestNodeVersionInToolVersions(t *testing.T) {
	content := "ruby 3.4.11\nnodejs 24.22.0\npython 3.15.0\n"
	if got := nodeVersionInFile(".tool-versions", content); got != "24.22.0" {
		t.Fatalf("tool-versions node pin=%q", got)
	}
	if got := nodeVersionInFile(".nvmrc", "  24.22.0\n"); got != "24.22.0" {
		t.Fatalf("nvmrc pin=%q", got)
	}
	if got := nodeVersionInFile(".tool-versions", "ruby 3.4.11\n"); got != "" {
		t.Fatalf("no nodejs line should mean no pin: %q", got)
	}
}

// TestRubyRangePolicy covers the switch from exact match to range semantics:
// family and floor expressions, plus backward-compatible exact pins.
func TestRubyRangePolicy(t *testing.T) {
	dir := t.TempDir()
	writeReportFile(t, dir, ".ruby-version", "3.4.10\n")

	for expression, wantIssue := range map[string]bool{
		"3.4.x":        false,
		"3.4":          false,
		">=3.4 <4":     false,
		"3.4.10":       false,
		"3.5.x":        true,
		">=3.4.11":     true,
		">=3.5":        true,
		"3.4.11":       true,
		">=3.4 || 4.x": false,
	} {
		issues, err := rubyVersionIssues(dir, expression, map[string][]string{}, false)
		if err != nil {
			t.Fatal(err)
		}
		if (len(issues) > 0) != wantIssue {
			t.Fatalf("expression %q: issues=%v wantIssue=%v", expression, issues, wantIssue)
		}
	}
}

// TestToolchainIssuesPhpAndCustom covers PHP detection (dedicated field,
// composer require.php as a reversed constraint check, platform pin) and the
// generic versions.custom adapter over .tool-versions / .<name>-version.
func TestToolchainIssuesPhpAndCustom(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(".php-version", "8.3.0\n")
	write("composer.json", `{"require":{"php":"^8.1"},"config":{"platform":{"php":"8.2.5"}}}`)
	write(".tool-versions", "php 8.2.1\nterraform 1.6.0\npostgres 16.4\n")
	write("module/.elixir-version", "1.15.0\n")

	var policy policyFile
	policy.Versions.Php = ">=8.3"
	policy.Versions.Custom = map[string]string{
		"terraform": ">=1.9",
		"elixir":    ">=1.16",
	}
	pins, issues := toolchainIssues(dir, policy)

	// .php-version 8.3.0 satisfies >=8.3; platform pin 8.2.5 and the
	// .tool-versions 8.2.1 pin do not. require.php ^8.1 is checked in the
	// constraint direction: >=8.3 does not satisfy ^8.1, so it is flagged
	// too. postgres has no policy — detected but unflagged.
	want := []string{
		".tool-versions: php 8.2.1 does not satisfy >=8.3",
		"composer.json (platform): php 8.2.5 does not satisfy >=8.3",
		"composer.json: php ^8.1 excludes policy >=8.3",
		".tool-versions: terraform 1.6.0 does not satisfy >=1.9",
		"module/.elixir-version: elixir 1.15.0 does not satisfy >=1.16",
	}
	slices.Sort(issues)
	slices.Sort(want)
	if !slices.Equal(issues, want) {
		t.Fatalf("issues=%v, want %v", issues, want)
	}
	for _, pin := range pins {
		if pin.Product == "postgres" && !pin.Constraint {
			continue // detected, no policy — informational only
		}
	}
}

// TestResolveCustomProduct exercises the versions.custom resolution path:
// the key doubles as the endoflife slug, channels work identically, and a
// key colliding with a built-in product gets a note rather than applying.
func TestResolveCustomProduct(t *testing.T) {
	client, _ := toolchainTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/terraform.json" {
			fmt.Fprint(w, `[{"cycle":"1.14","releaseDate":"2025-11-19","eol":false,"latest":"1.14.2"}]`)
			return
		}
		http.NotFound(w, r)
	})
	var policy policyFile
	policy.Versions.Custom = map[string]string{
		"terraform": "stable",
		"ruby":      "9.9",    // reserved name — must not apply
		"elixir":    "stable", // 404 → unresolved with note
	}
	resolved, pins := resolvePolicyVersions(t.Context(), client, policy, nil, true, false)

	if resolved["terraform"] != "1.14.2" || pins["terraform"].Source != "endoflife.date" {
		t.Fatalf("custom channel resolution: %+v", pins["terraform"])
	}
	if pins["ruby"].Note == "" || resolved["ruby"] != "" {
		t.Fatalf("reserved custom key must not apply: %+v", pins["ruby"])
	}
	if pins["elixir"].Note == "" || resolved["elixir"] != "" {
		t.Fatalf("missing upstream data should produce a note: %+v", pins["elixir"])
	}
}
