package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	repositoryreport "knowledge-ba46-flow/internal/report"
	"knowledge-ba46-flow/internal/review"
)

// setup opens a temp DB with one document and one pending 'move' suggestion.
func setup(t *testing.T) (config.Config, *database.DB, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	repo := t.TempDir()
	content := []byte("# doc")
	if err := os.WriteFile(filepath.Join(repo, "a.md"), content, 0644); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	sum := sha256.Sum256(content)
	doc := database.Document{Path: "a.md", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content)), ModifiedUnix: 1, TagsJSON: "[]"}
	if _, err := db.UpsertScanned(ctx, doc, "s1"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(ctx, doc, "test-model", "t", "s", nil, []database.Suggestion{
		{Action: "move", TargetPath: "b.md", Reason: "better place", Confidence: 0.9},
	}); err != nil {
		t.Fatal(err)
	}
	// Fields normally defaulted by config.Load must be set explicitly here.
	c := config.Config{RepositoryPath: repo, Extensions: []string{".md"}, TrashDirectory: ".kbm-trash", OllamaModel: "test-model"}
	return c, db, httptest.NewServer(NewMux(c, db, nil))
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status=%d", url, res.StatusCode)
	}
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

// waitForJob polls /api/ops/{id} until the job leaves "running" — tests that
// start background ops must wait before teardown, or the job goroutine's
// FinishJob races db.Close and logs "database is closed" warnings.
func waitForJob(t *testing.T, srv *httptest.Server, id int64) {
	t.Helper()
	for i := 0; i < 100; i++ {
		time.Sleep(20 * time.Millisecond)
		var j struct {
			Status string `json:"status"`
		}
		getJSON(t, srv.URL+"/api/ops/"+strconv.FormatInt(id, 10), &j)
		if j.Status != "running" && j.Status != "" {
			return
		}
	}
	t.Fatalf("job %d still running after 2s", id)
}

func TestClientLogEndpoint(t *testing.T) {
	_, _, srv := setup(t)
	res, err := http.Post(srv.URL+"/api/client-log", "application/json",
		strings.NewReader(`{"message":"Maximum call stack size exceeded","source":"/static/dashboard.js","line":251,"stack":"at renderProgressChart..."}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("client-log status=%d, want 204", res.StatusCode)
	}

	res, err = http.Post(srv.URL+"/api/client-log", "application/json",
		strings.NewReader(`{not json`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("client-log invalid JSON status=%d, want 400", res.StatusCode)
	}
}

func TestSecurityHeaders(t *testing.T) {
	_, _, srv := setup(t)
	res, err := http.Get(srv.URL + "/api/documents")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for header, expected := range want {
		if got := res.Header.Get(header); got != expected {
			t.Errorf("%s = %q, want %q", header, got, expected)
		}
	}
	csp := res.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'self'") ||
		!strings.Contains(csp, "script-src 'self'") ||
		strings.Contains(csp, "unsafe-") {
		t.Errorf("CSP should forbid remote, inline and eval'd scripts: %q", csp)
	}
}

func TestListPendingSuggestions(t *testing.T) {
	_, _, srv := setup(t)
	defer srv.Close()
	var sugs []database.Suggestion
	getJSON(t, srv.URL+"/api/suggestions", &sugs)
	if len(sugs) != 1 || sugs[0].Action != "move" || sugs[0].TargetPath != "b.md" {
		t.Fatalf("sugs=%+v", sugs)
	}
}

func TestDocumentAndSuggestionDetailEndpoints(t *testing.T) {
	_, _, srv := setup(t)
	defer srv.Close()

	var docs []database.Document
	getJSON(t, srv.URL+"/api/documents", &docs)
	if len(docs) != 1 || docs[0].Path != "a.md" {
		t.Fatalf("documents=%+v", docs)
	}

	var suggestion database.Suggestion
	getJSON(t, srv.URL+"/api/suggestions/1", &suggestion)
	if suggestion.ID != 1 || suggestion.ProposedContent != "" || suggestion.Reason != "better place" {
		t.Fatalf("suggestion=%+v", suggestion)
	}

	for _, path := range []string{"/api/suggestions/999", "/api/ops/999"} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s status=%d", path, res.StatusCode)
		}
	}
}

func TestDecideAndApplyOverHTTP(t *testing.T) {
	_, db, srv := setup(t)
	defer srv.Close()

	// Accept the suggestion via the API.
	res, err := http.Post(srv.URL+"/api/suggestions/1/decide", "application/json", strings.NewReader(`{"status":"accepted"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("decide status=%d", res.StatusCode)
	}

	// Applying again to a decided suggestion must conflict.
	res, err = http.Post(srv.URL+"/api/suggestions/1/decide", "application/json", strings.NewReader(`{"status":"rejected"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", res.StatusCode)
	}

	// Apply executes the accepted move: a.md -> b.md.
	res, err = http.Post(srv.URL+"/api/apply", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var results []review.ApplyResult
	if err := json.NewDecoder(res.Body).Decode(&results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Applied || results[0].Status != "applied" {
		t.Fatalf("results=%+v", results)
	}

	sug, err := db.Suggestion(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if sug.Status != "applied" {
		t.Fatalf("status=%q", sug.Status)
	}
}

func TestOpsJobLifecycle(t *testing.T) {
	_, _, srv := setup(t)
	defer srv.Close()
	var jobs []database.Job
	getJSON(t, srv.URL+"/api/ops", &jobs)
	if len(jobs) != 0 {
		t.Fatalf("initial jobs=%+v", jobs)
	}

	// Start a scan job — returns 202 with a running job.
	res, err := http.Post(srv.URL+"/api/ops", "application/json", strings.NewReader(`{"op":"scan"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("ops status=%d", res.StatusCode)
	}
	var j struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(res.Body).Decode(&j); err != nil {
		t.Fatal(err)
	}
	if j.ID != 1 || j.Status != "running" {
		t.Fatalf("job=%+v", j)
	}

	waitForJob(t, srv, j.ID)
	var finished struct {
		Status string `json:"status"`
		Detail string `json:"detail"`
	}
	getJSON(t, srv.URL+"/api/ops/"+strconv.FormatInt(j.ID, 10), &finished)
	if finished.Status != "done" || !strings.Contains(finished.Detail, "Scanned 1 markdown files") {
		t.Fatalf("job=%+v", finished)
	}
	getJSON(t, srv.URL+"/api/ops", &jobs)
	if len(jobs) != 1 || jobs[0].Status != "done" {
		t.Fatalf("jobs=%+v", jobs)
	}

	// An invalid op is rejected outright.
	res, err = http.Post(srv.URL+"/api/ops", "application/json", strings.NewReader(`{"op":"bogus"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", res.StatusCode)
	}
}

func TestOpsRejectedWhileAnotherProcessRunsAJob(t *testing.T) {
	_, db, srv := setup(t)
	defer srv.Close()

	// A job row with a fresh heartbeat simulates a CLI scan/analyze running
	// outside the server process — HTTP ops must refuse to overlap it.
	if _, err := db.CreateJob(context.Background(), "analyze"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/ops", "/api/apply"} {
		var body io.Reader = strings.NewReader(`{"op":"scan"}`)
		if path == "/api/apply" {
			body = nil
		}
		res, err := http.Post(srv.URL+path, "application/json", body)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusConflict {
			t.Fatalf("%s during a running job: expected 409, got %d", path, res.StatusCode)
		}
	}
}

func TestSeparateReportDashboardAndBackgroundRun(t *testing.T) {
	c, db, oldServer := setup(t)
	oldServer.Close()

	root := t.TempDir()
	registryDir := filepath.Join(root, "config")
	repoDir := filepath.Join(root, "repo")
	if err := os.MkdirAll(registryDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	runReportGit(t, repoDir, "init")
	runReportGit(t, repoDir, "config", "user.name", "KBM-flow tests")
	runReportGit(t, repoDir, "config", "user.email", "kbm-flow@example.invalid")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# report test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runReportGit(t, repoDir, "add", "README.md")
	runReportGit(t, repoDir, "commit", "-m", "initial")

	registryPath := filepath.Join(registryDir, "repos.yml")
	if err := os.WriteFile(registryPath, []byte("repos:\n  sample:\n    path: ../repo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	c.Report.RegistryPath = registryPath
	srv := httptest.NewServer(NewMux(c, db, nil))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/report")
	if err != nil {
		t.Fatal(err)
	}
	page, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || !strings.Contains(string(page), `x-data="repoReportDashboard"`) ||
		!strings.Contains(string(page), `href="/"`) ||
		!strings.Contains(string(page), "/static/vendor/bulma.min.css") ||
		!strings.Contains(string(page), "/static/vendor/chart.umd.js") {
		t.Fatalf("report page status=%d body missing dashboard shell", res.StatusCode)
	}

	res, err = http.Post(srv.URL+"/api/report/run?repo=unknown", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown report repo status=%d", res.StatusCode)
	}

	res, err = http.Post(srv.URL+"/api/report/run", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var started database.Job
	if res.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		t.Fatalf("report run status=%d body=%s", res.StatusCode, body)
	}
	if err := json.NewDecoder(res.Body).Decode(&started); err != nil {
		res.Body.Close()
		t.Fatal(err)
	}
	res.Body.Close()
	if started.Op != "report" || started.Status != "running" {
		t.Fatalf("started job=%+v", started)
	}

	var finished database.Job
	for i := 0; i < 100; i++ {
		time.Sleep(20 * time.Millisecond)
		res, err = http.Get(srv.URL + "/api/report/jobs/" + strconv.FormatInt(started.ID, 10))
		if err != nil {
			t.Fatal(err)
		}
		err = json.NewDecoder(res.Body).Decode(&finished)
		res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if finished.Status != "running" {
			break
		}
	}
	if finished.Status != "done" || !strings.Contains(finished.Detail, "Reported 1 repositories") {
		t.Fatalf("finished report job=%+v", finished)
	}

	var snapshot repositoryreport.Snapshot
	getJSON(t, srv.URL+"/api/report", &snapshot)
	if !snapshot.Configured || len(snapshot.Repos) != 1 || snapshot.Repos[0].Name != "sample" {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	var regularJobs []database.Job
	getJSON(t, srv.URL+"/api/ops", &regularJobs)
	if len(regularJobs) != 0 {
		t.Fatalf("report job leaked into regular operations: %+v", regularJobs)
	}
	res, err = http.Get(srv.URL + "/api/ops/" + strconv.FormatInt(started.ID, 10))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("report job appeared under /api/ops/{id}: status=%d", res.StatusCode)
	}
}

func TestReportIsDisabledWithoutRegistryConfig(t *testing.T) {
	_, _, srv := setup(t)
	defer srv.Close()

	var snapshot repositoryreport.Snapshot
	getJSON(t, srv.URL+"/api/report", &snapshot)
	if snapshot.Configured || len(snapshot.Repos) != 0 {
		t.Fatalf("unexpected unconfigured report=%+v", snapshot)
	}
	res, err := http.Post(srv.URL+"/api/report/run", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured report run status=%d", res.StatusCode)
	}
}

func TestLandingDashboardAggregatesKnowledgeAndReportSummaries(t *testing.T) {
	c, db, oldServer := setup(t)
	oldServer.Close()
	ctx := context.Background()

	changed := database.Document{
		Path: "changed.md", SHA256: "new-hash", Size: 1, TagsJSON: "[]",
	}
	if _, err := db.UpsertScanned(ctx, changed, "scan-landing"); err != nil {
		t.Fatal(err)
	}
	run, err := db.CreateJob(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishJob(ctx, run.ID, "done", "cycle", ""); err != nil {
		t.Fatal(err)
	}
	lastRun, err := db.Job(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	sonarBlocked, semgrepBlocked, openFindings, openPRs := 1, 2, 4, 2
	snapshot := repositoryreport.Snapshot{
		Configured:  true,
		GeneratedAt: "2026-10-17T12:00:00Z",
		Summary: repositoryreport.Summary{
			Repositories: 28, SonarProjects: 17, SemgrepProjects: 12,
		},
		Comparison: repositoryreport.Comparison{
			Available: true, GeneratedAt: "2026-10-17T11:00:00Z",
			SonarBlocked: &sonarBlocked, SemgrepBlocked: &semgrepBlocked,
			SemgrepOpenFindings: &openFindings, OpenPRs: &openPRs,
		},
		Repos: []repositoryreport.Repository{},
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveRepoReportSnapshot(ctx, snapshot.GeneratedAt, data); err != nil {
		t.Fatal(err)
	}
	c.Report.RegistryPath = filepath.Join(t.TempDir(), "repos.yml")
	srv := httptest.NewServer(NewMux(c, db, nil))
	defer srv.Close()

	var got struct {
		Knowledge struct {
			ChangedFiles       int    `json:"changed_files"`
			LastCycleAt        string `json:"last_cycle_at"`
			DaysSinceLastCycle *int   `json:"days_since_last_cycle"`
		} `json:"knowledge"`
		Report struct {
			Configured  bool                        `json:"configured"`
			GeneratedAt string                      `json:"generated_at"`
			Summary     repositoryreport.Summary    `json:"summary"`
			Comparison  repositoryreport.Comparison `json:"comparison"`
		} `json:"report"`
	}
	getJSON(t, srv.URL+"/api/dashboard", &got)
	if got.Knowledge.ChangedFiles != 1 || got.Knowledge.LastCycleAt != lastRun.Finished ||
		got.Knowledge.DaysSinceLastCycle == nil || *got.Knowledge.DaysSinceLastCycle < 0 {
		t.Fatalf("knowledge summary=%+v", got.Knowledge)
	}
	if !got.Report.Configured || got.Report.Summary.Repositories != 28 ||
		got.Report.Summary.SonarProjects != 17 || got.Report.Comparison.SonarBlocked == nil ||
		*got.Report.Comparison.SonarBlocked != 1 || got.Report.Comparison.SemgrepBlocked == nil ||
		*got.Report.Comparison.SemgrepBlocked != 2 ||
		got.Report.Comparison.SemgrepOpenFindings == nil || *got.Report.Comparison.SemgrepOpenFindings != 4 ||
		got.Report.Comparison.OpenPRs == nil || *got.Report.Comparison.OpenPRs != 2 {
		t.Fatalf("report summary=%+v", got.Report)
	}
}

func TestUIAndEmbeddedAssetsAreServed(t *testing.T) {
	_, _, srv := setup(t)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("UI status=%d", res.StatusCode)
	}
	ui, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ui), "x-data=\"landingDashboard\"") ||
		!strings.Contains(string(ui), `href="/knowledge"`) ||
		!strings.Contains(string(ui), `href="/report"`) ||
		!strings.Contains(string(ui), "/static/vendor/bulma.min.css") ||
		!strings.Contains(string(ui), "/static/vendor/alpine.min.js") {
		t.Fatalf("landing page missing workspace cards or framework references")
	}

	res, err = http.Get(srv.URL + "/knowledge")
	if err != nil {
		t.Fatal(err)
	}
	knowledgeUI, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK ||
		!strings.Contains(string(knowledgeUI), `x-data="knowledgeDashboard"`) ||
		!strings.Contains(string(knowledgeUI), `href="/"`) {
		t.Fatalf("knowledge dashboard status=%d or missing home link", res.StatusCode)
	}

	for _, asset := range []struct {
		path string
		mime string
	}{
		{path: "/static/vendor/bulma.min.css", mime: "text/css"},
		{path: "/static/vendor/alpine.min.js", mime: "text/javascript"},
		{path: "/static/vendor/chart.umd.js", mime: "text/javascript"},
		{path: "/static/landing.css", mime: "text/css"},
		{path: "/static/landing.js", mime: "text/javascript"},
		{path: "/static/report.css", mime: "text/css"},
		{path: "/static/report.js", mime: "text/javascript"},
	} {
		res, err := http.Get(srv.URL + asset.path)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(res.Body)
		res.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), asset.mime) {
			t.Fatalf("%s status=%d content-type=%q", asset.path, res.StatusCode, res.Header.Get("Content-Type"))
		}
		if len(body) == 0 {
			t.Fatalf("%s returned an empty asset", asset.path)
		}
	}
}

func runReportGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	commandArgs := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", commandArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func TestApplyAndOperationsShareBusyGuard(t *testing.T) {
	c, db, srv := setup(t)
	defer srv.Close()
	o := &ops{c: c, db: db, running: true}

	applyRecorder := httptest.NewRecorder()
	applyRequest := httptest.NewRequest(http.MethodPost, "/api/apply", nil)
	o.applyAccepted(applyRecorder, applyRequest)
	if applyRecorder.Code != http.StatusConflict {
		t.Fatalf("apply status=%d", applyRecorder.Code)
	}

	opRecorder := httptest.NewRecorder()
	opRequest := httptest.NewRequest(http.MethodPost, "/api/ops", strings.NewReader(`{"op":"scan"}`))
	o.create(opRecorder, opRequest)
	if opRecorder.Code != http.StatusConflict {
		t.Fatalf("op status=%d", opRecorder.Code)
	}

	decisionRecorder := httptest.NewRecorder()
	decisionRequest := httptest.NewRequest(http.MethodPost, "/api/suggestions/1/decide", strings.NewReader(`{"status":"accepted"}`))
	o.decide(decisionRecorder, decisionRequest)
	if decisionRecorder.Code != http.StatusConflict {
		t.Fatalf("decision status=%d", decisionRecorder.Code)
	}

	o.c.Report.RegistryPath = filepath.Join(t.TempDir(), "repos.yml")
	reportRecorder := httptest.NewRecorder()
	reportRequest := httptest.NewRequest(http.MethodPost, "/api/report/run", nil)
	o.createReport(reportRecorder, reportRequest)
	if reportRecorder.Code != http.StatusConflict {
		t.Fatalf("report run status=%d", reportRecorder.Code)
	}
}

func TestAnalyzeAndRunJobsWithoutPendingDocuments(t *testing.T) {
	c, db, srv := setup(t)
	defer srv.Close()
	o := &ops{c: c, db: db}

	detail, err := o.runOp(context.Background(), database.Job{Op: "analyze"}, opRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(detail, "Analyzed 0; fast-enriched 0; locally enriched 0; skipped 0 by MCP policy; failed 0 (model ") ||
		!strings.HasSuffix(detail, "; rejected 0 unsafe edit(s); skipped 0 stub placeholder(s).") {
		t.Fatalf("analyze detail=%q", detail)
	}
	detail, err = o.runOp(context.Background(), database.Job{Op: "run"}, opRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "Scanned 1 markdown files; 0 new/changed.") ||
		!strings.Contains(detail, "Analyzed 0; fast-enriched 0; locally enriched 0; skipped 0 by MCP policy; failed 0 (model ") {
		t.Fatalf("run detail=%q", detail)
	}
}

func TestDecideRejectsMalformedBodyAndStatus(t *testing.T) {
	_, db, srv := setup(t)
	defer srv.Close()
	for _, body := range []string{"not-json", `{"status":"maybe"}`} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/suggestions/1/decide", strings.NewReader(body))
		decide(db).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body=%q status=%d", body, recorder.Code)
		}
	}
}

func postStatus(t *testing.T, method, url, body string) int {
	t.Helper()
	var res *http.Response
	var err error
	if method == http.MethodPost {
		res, err = http.Post(url, "application/json", strings.NewReader(body))
	} else {
		res, err = http.Get(url)
	}
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res.StatusCode
}

func TestMissingAndMalformedResourcesReturn404And400(t *testing.T) {
	_, _, srv := setup(t)
	defer srv.Close()

	// Non-numeric ids parse to -1, which can never exist.
	for _, path := range []string{
		"/api/suggestions/abc",
		"/api/ops/abc",
		"/no-such-page",
		"/static/missing.css",
	} {
		if code := postStatus(t, http.MethodGet, srv.URL+path, ""); code != http.StatusNotFound {
			t.Fatalf("GET %s status=%d", path, code)
		}
	}
	if code := postStatus(t, http.MethodPost, srv.URL+"/api/ops", "not-json"); code != http.StatusBadRequest {
		t.Fatalf("POST /api/ops status=%d", code)
	}
	if code := postStatus(t, http.MethodPost, srv.URL+"/api/suggestions/abc/decide", `{"status":"accepted"}`); code != http.StatusConflict {
		t.Fatalf("decide on a non-numeric id status=%d", code)
	}
}

func TestDatabaseFailuresReturnServerErrors(t *testing.T) {
	_, db, srv := setup(t)
	defer srv.Close()
	db.Close() // simulate a broken database; sql.DB.Close is idempotent

	for _, path := range []string{"/api/documents", "/api/suggestions", "/api/ops"} {
		if code := postStatus(t, http.MethodGet, srv.URL+path, ""); code != http.StatusInternalServerError {
			t.Fatalf("GET %s status=%d", path, code)
		}
	}
	if code := postStatus(t, http.MethodPost, srv.URL+"/api/apply", ""); code != http.StatusInternalServerError {
		t.Fatalf("POST /api/apply status=%d", code)
	}
}

func TestFailedOperationJobIsRecorded(t *testing.T) {
	c, db, srv := setup(t)
	defer srv.Close()
	// Scanning a repository that does not exist must fail the job, not the test.
	c.RepositoryPath = filepath.Join(t.TempDir(), "missing-repo")
	o := &ops{c: c, db: db}

	job, err := db.CreateJob(context.Background(), "scan")
	if err != nil {
		t.Fatal(err)
	}
	if !o.begin() {
		t.Fatal("ops guard should be free")
	}
	o.exec(job, c, opRequest{}) // synchronous call — exercises the error branch and running-flag reset
	got, err := db.Job(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" || got.Error == "" || got.Finished == "" {
		t.Fatalf("failed job=%+v", got)
	}
	if o.running {
		t.Fatal("ops guard was not released after a failed job")
	}
}

func TestListenFailsStaleJobsAndShutsDownOnCancel(t *testing.T) {
	c, db, srv := setup(t)
	defer srv.Close()
	job, err := db.CreateJob(context.Background(), "scan")
	if err != nil {
		t.Fatal(err)
	}
	c.HTTPAddr = "127.0.0.1:0" // ephemeral port — Listen never collides
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the process is being interrupted: shut down immediately
	if err := Listen(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	got, err := db.Job(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" || got.Error != "interrupted by server restart" {
		t.Fatalf("stale job=%+v", got)
	}
}

func TestListenReturnsBindError(t *testing.T) {
	c, db, srv := setup(t)
	defer srv.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	c.HTTPAddr = listener.Addr().String() // already bound — ListenAndServe must fail
	if err := Listen(context.Background(), c, db); err == nil {
		t.Fatal("expected a bind error on an occupied port")
	}
}

func TestReportEndpointsWhenUnconfigured(t *testing.T) {
	_, _, srv := setup(t) // setup config has no report.registry_path
	defer srv.Close()

	var snapshot map[string]any
	getJSON(t, srv.URL+"/api/report", &snapshot)
	if snapshot["configured"] != false {
		t.Fatalf("snapshot=%v", snapshot)
	}
	if code := postStatus(t, http.MethodPost, srv.URL+"/api/report/run", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("POST /api/report/run status=%d", code)
	}
	res, err := http.Get(srv.URL + "/report")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /report status=%d", res.StatusCode)
	}
}

func TestReportJobEndpointsIsolateReportOps(t *testing.T) {
	_, db, srv := setup(t)
	defer srv.Close()
	ctx := context.Background()

	reportJob, err := db.CreateJob(ctx, "report")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishJob(ctx, reportJob.ID, "done", "Reported 1 repositories; 0 problems.", ""); err != nil {
		t.Fatal(err)
	}
	scanJob, err := db.CreateJob(ctx, "scan")
	if err != nil {
		t.Fatal(err)
	}

	var reportJobs []database.Job
	getJSON(t, srv.URL+"/api/report/jobs", &reportJobs)
	if len(reportJobs) != 1 || reportJobs[0].ID != reportJob.ID {
		t.Fatalf("report jobs=%+v", reportJobs)
	}
	var opsJobs []database.Job
	getJSON(t, srv.URL+"/api/ops", &opsJobs)
	for _, j := range opsJobs {
		if j.Op == "report" {
			t.Fatalf("report job leaked into /api/ops: %+v", opsJobs)
		}
	}
	if len(opsJobs) != 1 || opsJobs[0].ID != scanJob.ID {
		t.Fatalf("ops jobs=%+v", opsJobs)
	}

	// The two detail endpoints must not expose each other's job kinds.
	var got database.Job
	getJSON(t, srv.URL+"/api/report/jobs/1", &got)
	if got.Op != "report" {
		t.Fatalf("report job=%+v", got)
	}
	if code := postStatus(t, http.MethodGet, srv.URL+"/api/report/jobs/2", ""); code != http.StatusNotFound {
		t.Fatalf("scan job via report endpoint status=%d", code)
	}
	if code := postStatus(t, http.MethodGet, srv.URL+"/api/ops/1", ""); code != http.StatusNotFound {
		t.Fatalf("report job via ops endpoint status=%d", code)
	}
}

func TestReportDataServesStoredSnapshot(t *testing.T) {
	c, db, srv := setup(t)
	defer srv.Close()
	c.Report.RegistryPath = filepath.Join(t.TempDir(), "repos.yml") // configured but missing is fine here

	// Configured but no snapshot yet: an empty snapshot, not an error.
	res, err := http.Get(srv.URL + "/api/report")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", res.StatusCode)
	}

	stored := []byte(`{"configured":true,"repos":[]}`)
	if err := db.SaveRepoReportSnapshot(context.Background(), "2026-01-15T10:00:00Z", stored); err != nil {
		t.Fatal(err)
	}
	mux := NewMux(c, db, nil) // rebuild so the handler sees the configured registry path
	snapSrv := httptest.NewServer(mux)
	defer snapSrv.Close()
	res, err = http.Get(snapSrv.URL + "/api/report")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(stored) {
		t.Fatalf("stored snapshot body=%q", body)
	}
}

func TestReportJobViaRunOp(t *testing.T) {
	c, db, srv := setup(t)
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "repos.yml"), []byte("repos: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	c.Report.RegistryPath = filepath.Join(dir, "repos.yml")
	o := &ops{c: c, db: db}

	detail, err := o.runOp(context.Background(), database.Job{Op: "report"}, opRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if detail != "Reported 0 repositories; 0 problems." {
		t.Fatalf("detail=%q", detail)
	}
	if _, err := o.runOp(context.Background(), database.Job{Op: "bogus"}, opRequest{}); err == nil {
		t.Fatal("expected an unknown op to fail")
	}
}

func TestShutdownRequiresLoopbackAndSignalsStop(t *testing.T) {
	c, db, srv := setup(t)
	defer srv.Close()
	o := &ops{c: c, db: db}
	stop := make(chan struct{}, 1)
	handler := o.shutdown(stop)

	newReq := func(remote, query string) (*httptest.ResponseRecorder, *http.Request) {
		r := httptest.NewRequest(http.MethodPost, "/api/shutdown"+query, nil)
		r.RemoteAddr = remote
		return httptest.NewRecorder(), r
	}

	// Non-loopback clients are refused outright.
	rec, r := newReq("8.8.8.8:1234", "")
	handler.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback status=%d", rec.Code)
	}

	// While an op runs in this process, shutdown conflicts unless forced.
	if !o.begin() {
		t.Fatal("guard should be free")
	}
	rec, r = newReq("127.0.0.1:1234", "")
	handler.ServeHTTP(rec, r)
	if rec.Code != http.StatusConflict {
		t.Fatalf("busy status=%d", rec.Code)
	}
	rec, r = newReq("127.0.0.1:1234", "?force=1")
	handler.ServeHTTP(rec, r)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("forced status=%d", rec.Code)
	}
	o.end()

	// The stop signal arrives shortly after the accepted response.
	select {
	case <-stop:
	case <-time.After(2 * time.Second):
		t.Fatal("stop channel never signalled")
	}

	// An unwired shutdown reports unavailable rather than hanging.
	rec, r = newReq("127.0.0.1:1234", "")
	o.shutdown(nil).ServeHTTP(rec, r)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired status=%d", rec.Code)
	}
}

func TestModelsEndpointReportsTaskRouting(t *testing.T) {
	c, db, _ := setup(t)
	c.OllamaModelFast = "fast-model"
	c.OllamaModelExternal = "cloud-model"
	c.OllamaEmbedModel = "embed-model"
	c.OllamaTaskModels = map[string][]string{"expand": {"expand-default", "expand-alt"}}
	srv := httptest.NewServer(NewMux(c, db, nil))
	defer srv.Close()

	var got struct {
		Models []string `json:"models"`
		Tasks  map[string]struct {
			Default string   `json:"default"`
			Options []string `json:"options"`
		} `json:"tasks"`
	}
	getJSON(t, srv.URL+"/api/models", &got)
	for _, m := range []string{"test-model", "fast-model", "cloud-model", "embed-model", "expand-default", "expand-alt"} {
		found := false
		for _, have := range got.Models {
			if have == m {
				found = true
			}
		}
		if !found {
			t.Fatalf("models list missing %q: %v", m, got.Models)
		}
	}
	if got.Tasks["expand"].Default != "expand-default" || len(got.Tasks["expand"].Options) < 3 {
		t.Fatalf("expand task routing wrong: %+v", got.Tasks["expand"])
	}
	if got.Tasks["freshness"].Default != "cloud-model" {
		t.Fatalf("freshness should default to the external lane: %+v", got.Tasks["freshness"])
	}
	if got.Tasks["enrich"].Default != "fast-model" {
		t.Fatalf("enrich should default to the fast lane: %+v", got.Tasks["enrich"])
	}
}

func TestOpsRejectsUnconfiguredModelOverride(t *testing.T) {
	_, _, srv := setup(t)
	defer srv.Close()
	res, err := http.Post(srv.URL+"/api/ops", "application/json",
		strings.NewReader(`{"op":"expand","model":"ghost"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("unconfigured model must be rejected: %d", res.StatusCode)
	}
}

func TestOpsAcceptsTaskAndAll(t *testing.T) {
	c, db, app := setup(t)
	app.Close()
	ollama := fakeOllama(t, `{}`)
	c.OllamaURL = ollama.URL
	srv := httptest.NewServer(NewMux(c, db, nil))
	defer srv.Close()
	// Model capability preflight succeeds; the link-only fixture stub has no
	// template, so the accepted job completes without a generation request.
	res, err := http.Post(srv.URL+"/api/ops", "application/json",
		strings.NewReader(`{"op":"expand","model":"test-model"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("expand op should be accepted: %d", res.StatusCode)
	}
	var j struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(res.Body).Decode(&j); err != nil {
		t.Fatal(err)
	}
	waitForJob(t, srv, j.ID)
}

func TestOpsRejectsExternalOnNonLaneOp(t *testing.T) {
	c, db, _ := setup(t)
	c.OllamaModelExternal = "cloud-model"
	srv := httptest.NewServer(NewMux(c, db, nil))
	defer srv.Close()
	res, err := http.Post(srv.URL+"/api/ops", "application/json",
		strings.NewReader(`{"op":"enrich","external":true}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("external on enrich must be rejected: %d", res.StatusCode)
	}
}

func TestRunOpExpandSkipsNonStubs(t *testing.T) {
	c, db, _ := setup(t)
	c.MaxDocumentBytes = 10000
	c.StubMaxBodyLines = -1 // fixture one-liner must count as a real document
	o := &ops{c: c, db: db}
	// The fixture doc is not a stub: the expand task skips it without any LLM
	// call, and the job detail reports the skip.
	detail, err := o.runOp(context.Background(), database.Job{Op: "expand"}, opRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "expand:") || !strings.Contains(detail, "skipped") {
		t.Fatalf("expand detail=%q", detail)
	}
	doc, err := db.Documents(context.Background())
	if err != nil || doc[0].ExpandSHA256 != doc[0].SHA256 {
		t.Fatalf("skipped doc should be stamped: %+v", doc)
	}
}

func TestRunOpEmbedRequiresModel(t *testing.T) {
	c, db, _ := setup(t)
	// No embed model configured and no override — the lane reports
	// ErrNoTaskModel rather than running a nil client.
	o := &ops{c: c, db: db}
	if _, err := o.runOp(context.Background(), database.Job{Op: "embed"}, opRequest{}); err == nil {
		t.Fatal("embed without a configured model must fail")
	}
}

// fakeOllama serves the generate/embed/show endpoints with canned replies so
// task ops can run end-to-end without a real model.
func fakeOllama(t *testing.T, genReply string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/generate":
			_ = json.NewEncoder(w).Encode(map[string]any{"response": genReply, "done": true})
		case "/api/embed":
			_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{0.5, 0.5}}})
		case "/api/show":
			var req struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			capabilities := []string{"completion"}
			contextLength := 8192.0
			if strings.Contains(strings.ToLower(req.Model), "embed") {
				capabilities = []string{"embedding"}
				contextLength = 2048
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"capabilities": capabilities,
				"model_info":   map[string]any{"m.context_length": contextLength},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// taskOpsFixture extends setup() with the prompt files the task passes load
// and a config wired to the fake Ollama server.
func taskOpsFixture(t *testing.T, genReply string) (config.Config, *database.DB) {
	t.Helper()
	c, db, _ := setup(t)
	c.OllamaURL = fakeOllama(t, genReply).URL
	c.OllamaNumCtx = 8192
	c.MaxDocumentBytes = 10000
	c.StubMaxBodyLines = -1
	c.MCPServer = config.MCPServerKnowledgeBase
	c.PromptDirectory = t.TempDir()
	for name, text := range map[string]string{
		"base.md": "BASE", config.MCPServerKnowledgeBase + ".md": "MCP",
		"analyze.md": "ANALYZE", "enrich.md": "ENRICH",
		"expand.md": "EXPAND", "freshness.md": "FRESHNESS",
	} {
		if err := os.WriteFile(filepath.Join(c.PromptDirectory, name), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return c, db
}

func TestRunOpEnrichStampsFastLaneOnly(t *testing.T) {
	reply := `{"title":"Doc","summary":"s","tags":["x"],"patch":null}`
	c, db := taskOpsFixture(t, reply)
	o := &ops{c: c, db: db}
	detail, err := o.runOp(context.Background(), database.Job{Op: "enrich"}, opRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "test-model") {
		t.Fatalf("enrich detail=%q", detail)
	}
	docs, err := db.Documents(context.Background())
	if err != nil || len(docs) != 1 {
		t.Fatalf("docs=%v err=%v", docs, err)
	}
	if docs[0].EnrichedSHA256 != docs[0].SHA256 {
		t.Fatalf("enrich lineage not stamped: %+v", docs[0])
	}
	// The local analysis lane must not move — enrich-only is a separate pass.
	if docs[0].AnalyzedModel != "test-model" {
		t.Fatalf("analysis lineage clobbered: %+v", docs[0])
	}
}

func TestRunOpEmbedStoresVector(t *testing.T) {
	c, db := taskOpsFixture(t, `{}`)
	c.OllamaEmbedModel = "embed-model"
	o := &ops{c: c, db: db}
	if _, err := o.runOp(context.Background(), database.Job{Op: "embed"}, opRequest{}); err != nil {
		t.Fatal(err)
	}
	docs, err := db.Documents(context.Background())
	if err != nil || len(docs) != 1 {
		t.Fatalf("docs=%v err=%v", docs, err)
	}
	if len(docs[0].Embedding) == 0 || docs[0].EmbeddedSHA256 != docs[0].SHA256 {
		t.Fatalf("embedding not stored/stamped: %+v", docs[0])
	}
}

func TestRunOpFreshnessWritesManagedBlock(t *testing.T) {
	reply := `{"current":true,"summary":"up to date","flags":[],"suggestions":[]}`
	c, db := taskOpsFixture(t, reply)
	o := &ops{c: c, db: db}
	if _, err := o.runOp(context.Background(), database.Job{Op: "freshness"}, opRequest{}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(c.RepositoryPath, "a.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "freshness:") || !strings.Contains(string(raw), `"current"`) && !strings.Contains(string(raw), "current") {
		t.Fatalf("kbm.freshness not written:\n%s", raw)
	}
	docs, err := db.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The fixture bypassed the scanner, so body_sha256 is empty and the stamp
	// falls back to the canonical hash.
	if docs[0].FreshnessSHA256 != docs[0].SHA256 {
		t.Fatalf("freshness lineage not stamped: %+v", docs[0])
	}
	// The managed write must not have changed the canonical hash.
	if docs[0].SHA256 != docs[0].AnalyzedSHA256 {
		t.Fatal("kbm write invalidated the canonical hash")
	}
}

func TestRunOpAllSkipsUnconfiguredLanes(t *testing.T) {
	reply := `{"title":"Doc","summary":"s","tags":["x"],"patch":null,"obsolete":false,"suggestions":[]}`
	c, db := taskOpsFixture(t, reply)
	// No embed model configured: the all-run must skip that lane, not fail.
	o := &ops{c: c, db: db}
	detail, err := o.runOp(context.Background(), database.Job{Op: "all"}, opRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "enrich:") || !strings.Contains(detail, "analyze:") || strings.Contains(detail, "embed:") {
		t.Fatalf("all detail should skip the unconfigured embed lane: %q", detail)
	}
	docs, err := db.Documents(context.Background())
	if err != nil || len(docs) != 1 {
		t.Fatalf("docs=%v err=%v", docs, err)
	}
	if docs[0].EnrichedSHA256 != docs[0].SHA256 || docs[0].FreshnessSHA256 == "" {
		t.Fatalf("all-run lineage incomplete: %+v", docs[0])
	}
}

func TestRunOpAllModelOverrideKeepsEmbedLane(t *testing.T) {
	// A --model override applies to generation lanes only. The embed lane
	// must still call /api/embed with ollama_embed_model — a chat model
	// override there would fail the batch.
	var mu sync.Mutex
	embedModels, genModels := map[string]int{}, map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		m, _ := req["model"].(string)
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/generate":
			genModels[m]++
			_ = json.NewEncoder(w).Encode(map[string]any{"response": `{"title":"Doc","summary":"s","tags":["x"],"patch":null,"obsolete":false,"suggestions":[]}`, "done": true})
		case "/api/embed":
			embedModels[m]++
			_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{0.5, 0.5}}})
		case "/api/show":
			capabilities := []string{"completion"}
			contextLength := 8192.0
			if strings.Contains(strings.ToLower(m), "embed") {
				capabilities = []string{"embedding"}
				contextLength = 2048
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"capabilities": capabilities,
				"model_info":   map[string]any{"m.context_length": contextLength},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c, db := taskOpsFixture(t, "")
	c.OllamaURL = srv.URL
	c.OllamaEmbedModel = "embed-m"
	c.OllamaTaskModels = map[string][]string{config.TaskAnalyze: {"cloud-m"}}
	o := &ops{c: c, db: db}
	if _, err := o.runOp(context.Background(), database.Job{Op: "all"}, opRequest{Model: "cloud-m"}); err != nil {
		t.Fatal(err)
	}
	if embedModels["embed-m"] == 0 || embedModels["cloud-m"] != 0 {
		t.Fatalf("embed lane must use the embed model under --model override: %v", embedModels)
	}
	if genModels["cloud-m"] == 0 {
		t.Fatalf("generation lanes should have used the override model: %v", genModels)
	}
}

func TestProgressAndReportHistoryEndpoints(t *testing.T) {
	_, db, srv := setup(t)
	defer srv.Close()

	// setup() scans one document and records one analysis with one suggestion,
	// so every series totals exactly one regardless of bucket boundaries.
	var progress struct {
		Days []database.KnowledgeProgressDay `json:"days"`
	}
	getJSON(t, srv.URL+"/api/progress", &progress)
	var notes, analyzed, suggestions int
	for _, day := range progress.Days {
		notes += day.Notes
		analyzed += day.Analyzed
		suggestions += day.Suggestions
	}
	if notes != 1 || analyzed != 1 || suggestions != 1 {
		t.Fatalf("progress totals notes=%d analyzed=%d suggestions=%d (days=%+v)", notes, analyzed, suggestions, progress.Days)
	}

	var history struct {
		Points []struct {
			GeneratedAt string         `json:"generated_at"`
			Summary     map[string]any `json:"summary"`
		} `json:"points"`
	}
	getJSON(t, srv.URL+"/api/report/history", &history)
	if len(history.Points) != 0 {
		t.Fatalf("history before any run=%+v", history.Points)
	}
	if err := db.SaveRepoReportHistory(t.Context(), "2026-10-20T10:00:00Z", []byte(`{"problems":3}`)); err != nil {
		t.Fatal(err)
	}
	getJSON(t, srv.URL+"/api/report/history", &history)
	if len(history.Points) != 1 || history.Points[0].GeneratedAt != "2026-10-20T10:00:00Z" {
		t.Fatalf("history=%+v", history.Points)
	}
	if history.Points[0].Summary["problems"] != float64(3) {
		t.Fatalf("summary=%+v", history.Points[0].Summary)
	}
}

// Fault injection at the data seam: a torn repo_report_snapshot payload can
// only arrive via direct DB corruption (SaveRepoReportSnapshot validates on
// write). The endpoint must answer 500 — never serve corrupt JSON.
func TestReportEndpointRejectsCorruptStoredSnapshot(t *testing.T) {
	c, db, srv := setup(t)
	defer srv.Close()
	c.Report.RegistryPath = filepath.Join(t.TempDir(), "repos.yml")
	srv.Config.Handler = NewMux(c, db, nil)

	if _, err := db.Exec(`INSERT INTO repo_report_snapshot(id,generated_at,data_json)
VALUES(1,'2026-10-21T00:00:00Z','{corrupt payload')`); err != nil {
		t.Fatal(err)
	}
	res, err := http.Get(srv.URL + "/api/report")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("/api/report on corrupt snapshot: status=%d", res.StatusCode)
	}
}

// Dry-run estimation answers "what would this queue" without creating a job —
// the dashboard's pre-flight view must never start work or contact a model.
func TestOpsDryRunEstimatesQueueWithoutStartingAJob(t *testing.T) {
	c, db, srv := setup(t)
	defer srv.Close()

	// A second scanned-but-never-analyzed document is the pending queue.
	content := []byte("# another doc")
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "b.md"), content, 0644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	if _, err := db.UpsertScanned(t.Context(), database.Document{
		Path: "b.md", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content)), ModifiedUnix: 1, TagsJSON: "[]",
	}, "s1"); err != nil {
		t.Fatal(err)
	}

	res, err := http.Post(srv.URL+"/api/ops", "application/json", strings.NewReader(`{"op":"analyze","dry_run":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("dry-run status=%d", res.StatusCode)
	}
	var estimate struct {
		Documents    int                            `json:"documents"`
		Pending      map[string]int                 `json:"pending"`
		Dependents   int                            `json:"dependents"`
		AnalyzeTotal int                            `json:"analyze_total"`
		Warnings     []string                       `json:"warnings"`
		Stamps       map[string]database.ModelStamp `json:"stamps"`
	}
	if err := json.NewDecoder(res.Body).Decode(&estimate); err != nil {
		t.Fatal(err)
	}
	if estimate.Pending["analyze"] != 1 || estimate.AnalyzeTotal != 1 || estimate.Documents != 2 {
		t.Fatalf("estimate=%+v", estimate)
	}
	if stamp := estimate.Stamps["analyzed"]; stamp.Model != "test-model" || stamp.Docs != 1 {
		t.Fatalf("analyzed stamp=%+v", stamp)
	}
	var jobs []database.Job
	getJSON(t, srv.URL+"/api/ops", &jobs)
	if len(jobs) != 0 {
		t.Fatalf("dry-run created a job: %+v", jobs)
	}

	// Model stamps surface through /api/models for the dropdown marking.
	var models struct {
		Stamps map[string]database.ModelStamp `json:"stamps"`
	}
	getJSON(t, srv.URL+"/api/models", &models)
	if models.Stamps["analyzed"].Model != "test-model" || models.Stamps["analyzed"].Docs != 1 {
		t.Fatalf("stamps=%+v", models.Stamps)
	}

	// Deterministic ops have no queue to estimate.
	if status := postStatus(t, http.MethodPost, srv.URL+"/api/ops", `{"op":"scan","dry_run":true}`); status != http.StatusBadRequest {
		t.Fatalf("scan dry-run status=%d", status)
	}
}

// TestOpsDryRunWarnsAndCountsDependents exercises the dry run's two
// user-facing protections: an unchanged note linked to changed evidence is
// counted as a dependent, and a different embedding selection surfaces the
// re-vectorization warning. It also pins the batch skip — a stage with no
// configured model is left out of the estimate rather than failing it.
func TestOpsDryRunWarnsAndCountsDependents(t *testing.T) {
	ctx := context.Background()
	c, db, srv := setup(t)
	defer srv.Close()

	type estimateResult struct {
		Pending    map[string]int `json:"pending"`
		Dependents int            `json:"dependents"`
		Total      int            `json:"analyze_total"`
		Warnings   []string       `json:"warnings"`
	}
	decode := func(res *http.Response) estimateResult {
		var e estimateResult
		if err := json.NewDecoder(res.Body).Decode(&e); err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != http.StatusOK {
			t.Fatalf("dry-run status=%d", res.StatusCode)
		}
		return e
	}

	// This config has no embedding model at all: the embed stage is absent
	// from the batch estimate, not a 400.
	res, err := http.Post(srv.URL+"/api/ops", "application/json", strings.NewReader(`{"op":"all","dry_run":true}`))
	if err != nil {
		t.Fatal(err)
	}
	estimate := decode(res)
	res.Body.Close()
	if _, ok := estimate.Pending["embed"]; ok {
		t.Fatalf("unconfigured embed stage should be skipped, pending=%+v", estimate.Pending)
	}

	// b.md names a.md as related evidence; once a.md changes, b.md is a
	// dependent even though its own content and stamps are untouched.
	bContent := []byte("---\nrelated:\n  - a.md\n---\n\n# b doc\n")
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "b.md"), bContent, 0644); err != nil {
		t.Fatal(err)
	}
	bSum := sha256.Sum256(bContent)
	bDoc := database.Document{Path: "b.md", SHA256: hex.EncodeToString(bSum[:]), Size: int64(len(bContent)), ModifiedUnix: 1, TagsJSON: "[]", FrontmatterJSON: `{"related":["a.md"]}`}
	if _, err := db.UpsertScanned(ctx, bDoc, "s1"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(ctx, bDoc, "test-model", "t", "s", nil, nil); err != nil {
		t.Fatal(err)
	}

	// Change a.md: it goes pending and b.md's evidence context changes.
	aContent := []byte("# doc v2")
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "a.md"), aContent, 0644); err != nil {
		t.Fatal(err)
	}
	docs, err := db.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var aDoc database.Document
	for _, d := range docs {
		if d.Path == "a.md" {
			aDoc = d
		}
	}
	aSum := sha256.Sum256(aContent)
	aDoc.SHA256 = hex.EncodeToString(aSum[:])
	aDoc.Size = int64(len(aContent))
	if _, err := db.UpsertScanned(ctx, aDoc, "s2"); err != nil {
		t.Fatal(err)
	}

	// Stamp the corpus under the previous embedding model and a deterministic
	// enrich pass; the deterministic lane is not a model choice and must not
	// produce a mismatch warning.
	vec := []byte{0, 0, 128, 63}
	if err := db.SaveEmbedding(ctx, aDoc, "emb-a", vec); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveEmbedding(ctx, bDoc, "emb-a", vec); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveEnrichment(ctx, aDoc, "deterministic", "t", "s", nil, nil); err != nil {
		t.Fatal(err)
	}

	// The configured embedding default now differs from the recorded lineage —
	// the model-swap scenario a user cannot see without the stamp.
	c.OllamaEmbedModel = "emb-b"
	c.MaxDocumentBytes = 10000 // config.Load defaults this; a literal zero makes every doc unreadable
	warnSrv := httptest.NewServer(NewMux(c, db, nil))
	defer warnSrv.Close()

	res, err = http.Post(warnSrv.URL+"/api/ops", "application/json", strings.NewReader(`{"op":"all","dry_run":true}`))
	if err != nil {
		t.Fatal(err)
	}
	estimate = decode(res)
	res.Body.Close()
	if estimate.Pending["analyze"] != 1 || estimate.Dependents != 1 || estimate.Total != 2 {
		t.Fatalf("estimate=%+v, want analyze pending=1 dependents=1 total=2", estimate)
	}
	if estimate.Pending["embed"] != 2 {
		t.Fatalf("embed model change should re-queue both docs, pending=%+v", estimate.Pending)
	}
	var embedWarn, dependentsWarn, deterministicWarn bool
	for _, w := range estimate.Warnings {
		embedWarn = embedWarn || strings.Contains(w, "re-vectorized")
		dependentsWarn = dependentsWarn || strings.Contains(w, "evidence context changed")
		deterministicWarn = deterministicWarn || strings.Contains(w, "deterministic")
	}
	if !embedWarn || !dependentsWarn || deterministicWarn {
		t.Fatalf("warnings=%v, want embed mismatch + dependents warnings and no deterministic one", estimate.Warnings)
	}
}
