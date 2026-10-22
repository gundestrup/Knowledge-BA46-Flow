package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"knowledge-ba46-flow/internal/analyzer"
	"knowledge-ba46-flow/internal/database"
	appversion "knowledge-ba46-flow/internal/version"
)

func TestRootCommandIncludesAllSubcommands(t *testing.T) {
	root := rootCmd()
	for _, name := range []string{"scan", "check-links", "validate", "analyze", "run", "enrich", "embed", "expand", "freshness", "tasks", "report", "list", "review", "accept", "reject", "apply", "prune", "serve", "version"} {
		if _, _, err := root.Find([]string{name}); err != nil {
			t.Errorf("command %q not registered: %v", name, err)
		}
	}
}

func TestVersionFlagAndCommandPrintEmbeddedVersion(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"version"}} {
		cmd := rootCmd()
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("args=%v: %v", args, err)
		}
		if !strings.Contains(output.String(), appversion.Version) {
			t.Fatalf("args=%v output=%q version=%q", args, output.String(), appversion.Version)
		}
	}
}

func TestConfigFlagWorksBeforeAndAfterCommand(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "note.md"), []byte("# note"), 0644); err != nil {
		t.Fatal(err)
	}
	values := map[string]any{
		"repository_path":     repo,
		"database_path":       filepath.Join(dir, "data", "knowledge.db"),
		"prompt_directory":    filepath.Join(dir, "prompts"),
		"ollama_url":          "http://localhost:11434",
		"ollama_model":        "test",
		"extensions":          []string{".md"},
		"exclude_directories": []string{".git"},
	}
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"--config", configPath, "scan"},
		{"scan", "--config", configPath},
	} {
		cmd := rootCmd()
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("args=%v: %v", args, err)
		}
	}
}

func TestReportCommandRefreshesConfiguredReport(t *testing.T) {
	root := t.TempDir()
	registryDir := filepath.Join(root, "config")
	repoDir := filepath.Join(root, "repo")
	if err := os.MkdirAll(registryDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registryDir, "repos.yml"), []byte("repos:\n  sample:\n    path: ../repo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	values := map[string]any{
		"repository_path":  repoDir,
		"database_path":    filepath.Join(root, "data", "knowledge.db"),
		"prompt_directory": filepath.Join(root, "prompts"),
		"ollama_url":       "http://localhost:11434",
		"ollama_model":     "test",
		"report": map[string]any{
			"registry_path":  filepath.Join(registryDir, "repos.yml"),
			"remote_enabled": false,
		},
	}
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	cmd := rootCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"report", "--config", configPath})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Reported 1 repositories") {
		t.Fatalf("report output=%q", output.String())
	}
}

func TestDecisionCommandsRequireOneID(t *testing.T) {
	for _, command := range []string{"accept", "reject"} {
		cmd := rootCmd()
		cmd.SetArgs([]string{command})
		if err := cmd.Execute(); err == nil {
			t.Errorf("%s without an ID should fail", command)
		}
	}
}

func TestCommandsWithNoPendingWork(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	values := map[string]any{
		"repository_path":  repo,
		"database_path":    filepath.Join(dir, "data", "knowledge.db"),
		"prompt_directory": filepath.Join(dir, "prompts"),
		"ollama_url":       "http://localhost:11434",
		"ollama_model":     "test",
	}
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"analyze", "run", "list", "review", "apply"} {
		cmd := rootCmd()
		cmd.SetArgs([]string{name, "--config", configPath})
		if err := cmd.Execute(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := rootCmd()
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{"serve", "--config", configPath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("serve should shut down cleanly when its context is cancelled: %v", err)
	}
}

func runCLI(args ...string) error {
	cmd := rootCmd()
	cmd.SetArgs(args)
	return cmd.Execute()
}

// writeCLIConfig creates a config file pointing at repo/ and data/knowledge.db
// under dir, with any overrides merged in.
func writeCLIConfig(t *testing.T, dir string, extra map[string]any) string {
	t.Helper()
	values := map[string]any{
		"repository_path":  filepath.Join(dir, "repo"),
		"database_path":    filepath.Join(dir, "data", "knowledge.db"),
		"prompt_directory": filepath.Join(dir, "prompts"),
		"ollama_url":       "http://localhost:11434",
		"ollama_model":     "test",
	}
	for k, v := range extra {
		values[k] = v
	}
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// seedDocument writes a markdown file and records it plus one pending
// suggestion in the database the CLI will open.
func seedDocument(t *testing.T, repo, databasePath, name, content string, s database.Suggestion) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(t.Context(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	sum := sha256.Sum256([]byte(content))
	doc := database.Document{Path: name, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content)), TagsJSON: "[]"}
	if _, err := db.UpsertScanned(ctx, doc, "scan"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(ctx, doc, "test-model", name, "", nil, []database.Suggestion{s}); err != nil {
		t.Fatal(err)
	}
}

func TestCommandsFailWithoutConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")
	for _, args := range [][]string{
		{"scan", "--config", missing},
		{"analyze", "--config", missing},
		{"run", "--config", missing},
		{"list", "--config", missing},
		{"review", "--config", missing},
		{"accept", "1", "--config", missing},
		{"reject", "1", "--config", missing},
		{"apply", "--config", missing},
		{"serve", "--config", missing},
	} {
		if err := runCLI(args...); err == nil {
			t.Errorf("%v succeeded without a config", args)
		}
	}
}

func TestUnknownCommandFails(t *testing.T) {
	if err := runCLI("not-a-command"); err == nil {
		t.Fatal("expected an unknown command to fail")
	}
}

func TestReportCommand(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	// Unconfigured: no report.registry_path — the command must fail cleanly.
	configPath := writeCLIConfig(t, dir, nil)
	if err := runCLI("report", "--config", configPath); err == nil {
		t.Fatal("report without a registry should fail")
	}
	// Configured with an empty registry: succeeds and persists the snapshot.
	registryPath := filepath.Join(dir, "repos.yml")
	if err := os.WriteFile(registryPath, []byte("repos: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	configPath = writeCLIConfig(t, dir, map[string]any{
		"report": map[string]any{"registry_path": registryPath},
	})
	if err := runCLI("report", "--config", configPath); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(t.Context(), filepath.Join(dir, "data", "knowledge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, data, err := db.RepoReportSnapshot(context.Background()); err != nil || len(data) == 0 {
		t.Fatalf("snapshot was not persisted: data=%s err=%v", data, err)
	}
}

func TestAcceptRejectAndApplyEndToEnd(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(dir, "data", "knowledge.db")
	configPath := writeCLIConfig(t, dir, nil)

	// #1: a move we accept — apply must rename the file.
	seedDocument(t, repo, databasePath, "a.md", "# A", database.Suggestion{Action: "move", TargetPath: "moved/a.md", Reason: "better"})
	// #2: an expand we reject — apply must not touch it.
	seedDocument(t, repo, databasePath, "b.md", "# B", database.Suggestion{Action: "expand", Reason: "more", ProposedContent: "# B expanded"})
	// #3: never decided — apply must leave it pending.
	seedDocument(t, repo, databasePath, "c.md", "# C", database.Suggestion{Action: "move", TargetPath: "b.md", Reason: "collision"})
	// #4: a move onto an existing file we accept — apply must fail it and keep the target.
	if err := os.WriteFile(filepath.Join(repo, "target.md"), []byte("# keep"), 0644); err != nil {
		t.Fatal(err)
	}
	seedDocument(t, repo, databasePath, "d.md", "# D", database.Suggestion{Action: "move", TargetPath: "target.md", Reason: "collision"})

	if err := runCLI("accept", "1", "--config", configPath); err != nil {
		t.Fatal(err)
	}
	if err := runCLI("accept", "1", "--config", configPath); err == nil {
		t.Fatal("re-deciding an accepted suggestion should fail")
	}
	if err := runCLI("reject", "2", "--config", configPath); err != nil {
		t.Fatal(err)
	}
	if err := runCLI("accept", "4", "--config", configPath); err != nil {
		t.Fatal(err)
	}
	if err := runCLI("accept", "bogus", "--config", configPath); err == nil {
		t.Fatal("a non-numeric suggestion ID should fail")
	}
	if err := runCLI("apply", "--config", configPath); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(repo, "moved", "a.md")); err != nil {
		t.Fatalf("accepted move was not applied: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(repo, "b.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "# B" {
		t.Fatalf("rejected expand changed the file: %q", content)
	}
	content, err = os.ReadFile(filepath.Join(repo, "target.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "# keep" {
		t.Fatalf("failed move overwrote the target: %q", content)
	}

	db, err := database.Open(t.Context(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for id, want := range map[int64]string{1: "applied", 2: "rejected", 3: "pending", 4: "failed"} {
		got, err := db.Suggestion(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != want {
			t.Fatalf("suggestion #%d status=%q want %q", id, got.Status, want)
		}
	}
}

func TestRunCommandRecordsSuccessfulCycle(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0755); err != nil {
		t.Fatal(err)
	}
	configPath := writeCLIConfig(t, dir, nil)
	if err := runCLI("run", "--config", configPath); err != nil {
		t.Fatal(err)
	}

	db, err := database.Open(t.Context(), filepath.Join(dir, "data", "knowledge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	overview, err := db.KnowledgeDashboard(context.Background(), "test-model")
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := db.JobsForOps(context.Background(), 10, "run")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Status != "done" || overview.LastCycleAt != jobs[0].Finished {
		t.Fatalf("overview=%+v jobs=%+v", overview, jobs)
	}
}

func TestApplyCommandSkipsStaleSuggestion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo, "guide.md")
	original := []byte("# Original")
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(dir, "data", "knowledge.db")
	db, err := database.Open(t.Context(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(original)
	doc := database.Document{Path: "guide.md", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(original)), TagsJSON: "[]"}
	if _, err := db.UpsertScanned(ctx, doc, "scan"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(ctx, doc, "test-model", "Guide", "", nil, []database.Suggestion{
		{Action: "expand", Reason: "add detail", ProposedContent: "# LLM content"},
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingSuggestions(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	if err := db.Decide(ctx, pending[0].ID, "accepted"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	changed := []byte("# User edit")
	if err := os.WriteFile(path, changed, 0644); err != nil {
		t.Fatal(err)
	}

	values := map[string]any{
		"repository_path":  repo,
		"database_path":    databasePath,
		"prompt_directory": filepath.Join(dir, "prompts"),
		"ollama_url":       "http://localhost:11434",
		"ollama_model":     "test",
	}
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		t.Fatal(err)
	}
	cmd := rootCmd()
	cmd.SetArgs([]string{"apply", "--config", configPath})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	db, err = database.Open(t.Context(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	suggestion, err := db.Suggestion(ctx, pending[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if suggestion.Status != "superseded" {
		t.Fatalf("status=%q", suggestion.Status)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(changed) {
		t.Fatalf("stale suggestion changed file: %q", got)
	}
}

func TestProgressPrinterOutput(t *testing.T) {
	var buf bytes.Buffer
	printer := progressPrinter(&buf)
	printer(analyzer.Progress{Index: 2, Total: 5, Path: "a.md", Outcome: "analyzing"})
	printer(analyzer.Progress{Index: 2, Total: 5, Path: "a.md", Outcome: "analyzed", Detail: "2 suggestions"})
	got := buf.String()
	if !strings.Contains(got, "[2/5] a.md") || !strings.Contains(got, "analyzed: 2 suggestions") {
		t.Fatalf("progress output=%q", got)
	}
}

func TestFinishJobOutcomes(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	job, err := db.CreateJob(ctx, "analyze")
	if err != nil {
		t.Fatal(err)
	}
	detail := "3 docs"
	var runErr error
	finishJob(db, job.ID, &detail, &runErr)
	done, err := db.Job(ctx, job.ID)
	if err != nil || done.Status != "done" || done.Detail != "3 docs" {
		t.Fatalf("done job=%+v err=%v", done, err)
	}

	job2, err := db.CreateJob(ctx, "scan")
	if err != nil {
		t.Fatal(err)
	}
	runErr = errors.New("boom")
	finishJob(db, job2.ID, &detail, &runErr)
	failed, err := db.Job(ctx, job2.ID)
	if err != nil || failed.Status != "failed" || failed.Error != "boom" {
		t.Fatalf("failed job=%+v err=%v", failed, err)
	}
}

func TestPruneCommandRemovesFinishedJobs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	configPath := writeCLIConfig(t, dir, nil)
	databasePath := filepath.Join(dir, "data", "knowledge.db")
	db, err := database.Open(t.Context(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		job, err := db.CreateJob(ctx, "scan")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.FinishJob(ctx, job.ID, "done", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	if err := runCLI("prune", "--keep", "2", "--config", configPath); err != nil {
		t.Fatal(err)
	}
	db, err = database.Open(t.Context(), databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	jobs, err := db.Jobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected 2 kept jobs, got %d", len(jobs))
	}
}

// taskModelServer answers /api/generate with a reply chosen by the request's
// model field, so the tasks command can route every lane through one fake.
func taskModelServer(t *testing.T, replies map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/generate":
			var req struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			reply, ok := replies[req.Model]
			if !ok {
				t.Errorf("generate called with unexpected model %q", req.Model)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"response": reply, "done": true})
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
			if strings.Contains(strings.ToLower(req.Model), "emb") {
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
}

func TestTasksCommandRunsEveryConfiguredLane(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	writeFile := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("guide.md", "---\ntags: [tool]\n---\n# Guide\n\nSubstantial guide body.\n")
	writeFile("STUB.md", "# TEMPLATE\n\n## What it is\n\n## When to reach for it\n")
	writeFile("wip.md", "---\ntags: [stub]\n---\n# Wip\n")

	prompts := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(prompts, 0755); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"base.md": "BASE", "knowledge-base-mcp-server.md": "MCP",
		"analyze.md": "AN", "enrich.md": "EN", "expand.md": "EX", "freshness.md": "FR",
	} {
		if err := os.WriteFile(filepath.Join(prompts, name), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}

	srv := taskModelServer(t, map[string]string{
		"enrich-m": `{"title":"Guide","summary":"s","tags":["tool"],"patch":null}`,
		"an-m":     `{"title":"Guide","summary":"s","tags":["tool"],"obsolete":false,"suggestions":[]}`,
		"fr-m":     `{"current":true,"summary":"ok","flags":[],"suggestions":[]}`,
		"ex-m": `{"title":"Wip","summary":"s","tags":["stub"],"reason":"placeholder",` +
			`"confidence":0.8,"proposed_content":"---\ntags: [stub]\n---\n# Wip\n\nExpanded body.\n"}`,
	})
	defer srv.Close()

	configPath := writeCLIConfig(t, dir, map[string]any{
		"ollama_url":          srv.URL,
		"mcp_server":          "knowledge-base-mcp-server",
		"ollama_embed_model":  "emb-m",
		"stub_max_body_lines": -1,
		"max_document_bytes":  10000,
		"ollama_task_models": map[string][]string{
			"enrich":    {"enrich-m"},
			"analyze":   {"an-m"},
			"expand":    {"ex-m"},
			"freshness": {"fr-m"},
		},
	})
	if err := runCLI("scan", "--config", configPath); err != nil {
		t.Fatal(err)
	}
	if err := runCLI("tasks", "--config", configPath); err != nil {
		t.Fatal(err)
	}

	db, err := database.Open(context.Background(), filepath.Join(dir, "data", "knowledge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	byPath := map[string]database.Document{}
	docs, err := db.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		byPath[d.Path] = d
	}
	guide := byPath["guide.md"]
	// The deterministic stage enriched the note locally, so the enrich lane
	// stamps "deterministic" as its producer rather than the model name.
	if guide.EnrichedSHA256 != guide.SHA256 || guide.EnrichedModel == "" ||
		guide.AnalyzedModel != "an-m" || guide.FreshnessModel != "fr-m" ||
		guide.EmbeddedModel != "emb-m" {
		t.Fatalf("guide.md lane lineage wrong: %+v", guide)
	}
	if len(guide.Embedding) == 0 {
		t.Fatal("embed pass should have stored a vector")
	}
	wip := byPath["wip.md"]
	if wip.ExpandModel != "ex-m" {
		t.Fatalf("stub should have been expanded by the expand lane: %+v", wip)
	}
	jobs, err := db.Jobs(ctx, 5)
	if err != nil || len(jobs) == 0 {
		t.Fatalf("expected a recorded job: %v", err)
	}
	var allJob *database.Job
	for i := range jobs {
		if jobs[i].Op == "all" {
			allJob = &jobs[i]
		}
	}
	if allJob == nil || allJob.Status != "done" {
		t.Fatalf("tasks job not recorded as done: %+v", jobs)
	}
	if !strings.Contains(allJob.Detail, "enrich:") || !strings.Contains(allJob.Detail, "analyze:") {
		t.Fatalf("job detail should summarize every lane: %q", allJob.Detail)
	}
}

func TestTasksGenerationAndEmbeddingOverridesStaySeparate(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "guide.md"),
		[]byte("---\ntags: [tool]\n---\n# Guide\n\nSubstantial guide body.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	prompts := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(prompts, 0755); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"base.md": "BASE", "knowledge-base-mcp-server.md": "MCP",
		"analyze.md": "AN", "enrich.md": "EN", "expand.md": "EX", "freshness.md": "FR",
	} {
		if err := os.WriteFile(filepath.Join(prompts, name), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// A generation-model override must never reach /api/embed. The independent
	// embedding override is checked against embedding capability instead.
	var mu sync.Mutex
	var embedModels []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch r.URL.Path {
		case "/api/generate":
			replies := map[string]string{
				"gen-m": `{"title":"Guide","summary":"s","tags":["tool"],"obsolete":false,"suggestions":[]}`,
			}
			reply, ok := replies[req.Model]
			if !ok {
				reply = `{"title":"G","summary":"s","tags":["tool"],"patch":null,"current":true,"flags":[],"suggestions":[]}`
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"response": reply, "done": true})
		case "/api/embed":
			mu.Lock()
			embedModels = append(embedModels, req.Model)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{0.5, 0.5}}})
		case "/api/show":
			capabilities := []string{"completion"}
			contextLength := 8192.0
			if strings.Contains(strings.ToLower(req.Model), "emb") {
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
	defer srv.Close()

	configPath := writeCLIConfig(t, dir, map[string]any{
		"ollama_url":          srv.URL,
		"mcp_server":          "knowledge-base-mcp-server",
		"ollama_embed_model":  "emb-m",
		"stub_max_body_lines": -1,
		"max_document_bytes":  10000,
		"ollama_task_models": map[string][]string{
			"analyze": {"gen-m"},
			"embed":   {"emb-m", "emb-alt"},
		},
	})
	if err := runCLI("scan", "--config", configPath); err != nil {
		t.Fatal(err)
	}
	if err := runCLI("tasks", "--config", configPath, "--model", "gen-m", "--embed-model", "emb-alt"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, m := range embedModels {
		if m != "emb-alt" {
			t.Fatalf("embed lane received generation model %q instead of embedding override", m)
		}
	}
	if len(embedModels) == 0 {
		t.Fatal("embed lane never ran")
	}
}

func TestTasksRequiresExternalConsentForCloudModelOverride(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	generateCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/show":
			var req struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			response := map[string]any{
				"capabilities": []string{"completion"},
				"remote_host":  "ollama.com",
			}
			_ = json.NewEncoder(w).Encode(response)
		case "/api/generate":
			mu.Lock()
			generateCalls++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"response": `{}`, "done": true})
		case "/api/embed":
			_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{0.5, 0.5}}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	configPath := writeCLIConfig(t, dir, map[string]any{
		"ollama_url":            srv.URL,
		"ollama_model_external": "gpt-oss:120b-cloud",
		"ollama_embed_model":    "embeddinggemma:300m",
	})
	if err := runCLI("tasks", "--config", configPath, "--model", "gpt-oss:120b-cloud"); err == nil ||
		!strings.Contains(err.Error(), "explicit cloud/remote consent") {
		t.Fatalf("cloud generation without --external should be blocked, got %v", err)
	}
	if err := runCLI("tasks", "--config", configPath, "--model", "gpt-oss:120b-cloud", "--external"); err != nil {
		t.Fatalf("explicit --external should permit the cloud model: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if generateCalls != 0 {
		t.Fatalf("empty repository should make no generation calls, got %d", generateCalls)
	}
}

func TestTaskCommandsRejectUnconfiguredExternal(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0755); err != nil {
		t.Fatal(err)
	}
	configPath := writeCLIConfig(t, dir, nil)
	for _, name := range []string{"expand", "freshness", "analyze", "run"} {
		if err := runCLI(name, "--external", "--config", configPath); err == nil {
			t.Errorf("%s --external without ollama_model_external should fail", name)
		}
	}
	// enrich and embed have no --external flag at all.
	for _, name := range []string{"enrich", "embed"} {
		cmd := rootCmd()
		cmd.SetArgs([]string{name, "--external", "--config", configPath})
		if err := cmd.Execute(); err == nil {
			t.Errorf("%s should reject an unknown --external flag", name)
		}
	}
}

func TestValidateCommandReportsViolations(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	configPath := writeCLIConfig(t, dir, nil)
	// An empty repository has no notes to violate the conventions.
	if err := runCLI("validate", "--config", configPath); err != nil {
		t.Fatalf("validate on an empty repo should pass: %v", err)
	}
	// A note with no ancestor README.md index violates the linking convention.
	if err := os.WriteFile(filepath.Join(repo, "orphan.md"), []byte("# Orphan\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := runCLI("validate", "--config", configPath); err == nil {
		t.Fatal("validate should fail when a note has no index README")
	}
}

func TestCheckLinksCommandRecordsDeadLinks(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer dead.Close()

	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	doc := "---\ntags: [tool]\n---\n# Doc\n\nSee [reference](" + dead.URL + "/gone).\n"
	if err := os.WriteFile(filepath.Join(repo, "doc.md"), []byte(doc), 0644); err != nil {
		t.Fatal(err)
	}
	configPath := writeCLIConfig(t, dir, map[string]any{
		"stub_max_body_lines": -1,
		"max_document_bytes":  10000,
	})
	if err := runCLI("scan", "--config", configPath); err != nil {
		t.Fatal(err)
	}
	if err := runCLI("check-links", "--config", configPath); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(repo, "doc.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "link_check:") || !strings.Contains(string(raw), dead.URL) {
		t.Fatalf("kbm.link_check should record the dead URL:\n%s", raw)
	}
	// The managed write must not have requeued the document for analysis.
	db, err := database.Open(context.Background(), filepath.Join(dir, "data", "knowledge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	docs, err := db.Documents(context.Background())
	if err != nil || len(docs) != 1 {
		t.Fatalf("docs=%v err=%v", docs, err)
	}
	jobs, err := db.Jobs(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	var linkJob *database.Job
	for i := range jobs {
		if jobs[i].Op == "link-check" {
			linkJob = &jobs[i]
		}
	}
	if linkJob == nil || linkJob.Status != "done" || !strings.Contains(linkJob.Detail, "dead") {
		t.Fatalf("link-check job should record a done detail with dead count: %+v", jobs)
	}
}
