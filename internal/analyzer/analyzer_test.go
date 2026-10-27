package analyzer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/scanner"
	"knowledge-ba46-flow/internal/similarity"
)

// fakeLLM records the prompt and returns canned JSON — lets the whole analysis
// pipeline run in tests without a real Ollama.
type fakeLLM struct {
	prompt  string
	prompts []string
	format  json.RawMessage
	reply   string
	err     error
	queue   []string // when set, each call shifts the next canned reply
}

func (f *fakeLLM) Generate(_ context.Context, prompt string, format json.RawMessage) (string, error) {
	f.prompt = prompt
	f.prompts = append(f.prompts, prompt)
	f.format = format
	if len(f.queue) > 0 {
		f.reply = f.queue[0]
		f.queue = f.queue[1:]
	}
	return f.reply, f.err
}

// setup builds a temp repo with one markdown file, a temp prompt dir and a DB.
func setup(t *testing.T) (config.Config, *database.DB) {
	t.Helper()
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, "a.md"), []byte("# Alpha doc"), 0644)
	promptDir := t.TempDir()
	os.WriteFile(filepath.Join(promptDir, "base.md"), []byte("BASE"), 0644)
	os.WriteFile(filepath.Join(promptDir, config.MCPServerKnowledgeBase+".md"), []byte("MCP"), 0644)
	os.WriteFile(filepath.Join(promptDir, "analyze.md"), []byte("TASK"), 0644)
	os.WriteFile(filepath.Join(promptDir, "enrich.md"), []byte("ENRICH"), 0644)
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c := config.Config{
		RepositoryPath:   repo,
		WriteScope:       "all",
		PromptDirectory:  promptDir,
		MCPServer:        config.MCPServerKnowledgeBase,
		Extensions:       []string{".md"},
		TrashDirectory:   ".kbm-trash",
		MaxDocumentBytes: 10000,
		OllamaModel:      "test-model",
		EnrichmentMode:   config.EnrichmentModeAlways,
		// Disable size-based stub detection: fixture documents are one-liners
		// that must stay analyzable; stub behavior has dedicated tests.
		StubMaxBodyLines: -1,
	}
	return c, db
}

func TestRunAnalyzesPendingDoc(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: "```json\n" + `{"title":"Alpha","summary":"S","tags":["x","y"],"obsolete":false,
		"suggestions":[{"action":"move","target_path":"b.md","reason":"r","confidence":1.7,"proposed_content":""},
		               {"action":"teleport","target_path":"","reason":"bogus","confidence":0.1,"proposed_content":""}]}` + "\n```"}

	r, err := Run(ctx, c, db, llm)
	if err != nil {
		t.Fatal(err)
	}
	if r.Processed != 1 || r.Failed != 0 {
		t.Fatalf("result=%+v", r)
	}

	// The schema must be sent as the format (structured outputs), and the prompt
	// must contain base + task + context + document.
	if !strings.Contains(string(llm.format), `"type": "object"`) {
		t.Fatalf("format=%s", llm.format)
	}
	for _, want := range []string{"BASE", "TASK", "Current path: a.md", "# Alpha doc"} {
		if !strings.Contains(llm.prompt, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}

	// Document is now analyzed; path tag "a" merged with LLM tags x,y;
	// confidence clamped to 1; the invalid "teleport" action was dropped.
	docs, _ := db.Pending(ctx, "test-model", "test-model", false)
	if len(docs) != 0 {
		t.Fatalf("still pending: %+v", docs)
	}
	sugs, _ := db.PendingSuggestions(ctx)
	if len(sugs) != 1 || sugs[0].Action != "move" || sugs[0].Confidence != 1.0 {
		t.Fatalf("sugs=%+v", sugs)
	}
}

func TestRunExternalLaneKeepsLocalLineage(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	c.OllamaModelExternal = "ext-model"
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: `{"title":"T","summary":"S","tags":[],"obsolete":false,"suggestions":[]}`}
	if _, err := Run(ctx, c, db, llm); err != nil {
		t.Fatal(err)
	}
	// Enhanced run: same pipeline, external lane. It must stamp external_*
	// and leave analyzed_* alone.
	r, err := RunProgress(ctx, c, db, Generators{Analysis: llm}, RunOptions{External: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Processed != 1 {
		t.Fatalf("external run should process the doc: %+v", r)
	}
	docs, _ := db.Documents(ctx)
	if docs[0].AnalyzedModel != "test-model" || docs[0].ExternalModel != "ext-model" {
		t.Fatalf("lanes: analyzed=%q external=%q", docs[0].AnalyzedModel, docs[0].ExternalModel)
	}
	// A following normal run must not see the doc as new or drifted.
	if r, err := Run(ctx, c, db, llm); err != nil || r.Processed != 0 {
		t.Fatalf("normal rerun=%+v err=%v", r, err)
	}
	// And the external lane is incremental on its own markers.
	if r, err := RunProgress(ctx, c, db, Generators{Analysis: llm}, RunOptions{External: true}); err != nil || r.Processed != 0 {
		t.Fatalf("external rerun=%+v err=%v", r, err)
	}
	// A changed file is pending in both lanes.
	os.WriteFile(filepath.Join(c.RepositoryPath, "a.md"), []byte("# Alpha doc v2"), 0644)
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	if pending, _ := db.Pending(ctx, "test-model", "test-model", false); len(pending) != 1 {
		t.Fatalf("local pending=%v", pending)
	}
	if pending, _ := db.PendingExternal(ctx, "ext-model", "test-model", false, false); len(pending) != 1 {
		t.Fatalf("external pending=%v", pending)
	}
}

func TestRunExternalRequiresConfiguredModel(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	_, err := RunProgress(ctx, c, db, Generators{Analysis: &fakeLLM{}}, RunOptions{External: true})
	if err == nil || !strings.Contains(err.Error(), "ollama_model_external") {
		t.Fatalf("want ollama_model_external error, got %v", err)
	}
}

func TestRunProgressEmitsPerDocumentEvents(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	os.WriteFile(filepath.Join(c.RepositoryPath, "b.md"), []byte("# Beta doc"), 0644)
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: `{"title":"T","summary":"S","tags":[],"obsolete":false,"suggestions":[]}`}

	var events []Progress
	r, err := RunProgress(ctx, c, db, Generators{Analysis: llm}, RunOptions{Progress: func(p Progress) { events = append(events, p) }})
	if err != nil {
		t.Fatal(err)
	}
	if r.Processed != 2 {
		t.Fatalf("result=%+v", r)
	}
	// Each document produces an "analyzing" start event then a terminal event.
	if len(events) != 4 {
		t.Fatalf("events=%+v", events)
	}
	for i, want := range []struct {
		index   int
		outcome string
	}{{1, "analyzing"}, {1, "analyzed"}, {2, "analyzing"}, {2, "analyzed"}} {
		if events[i].Index != want.index || events[i].Total != 2 || events[i].Outcome != want.outcome {
			t.Fatalf("event[%d]=%+v want index=%d outcome=%s", i, events[i], want.index, want.outcome)
		}
	}
	if !strings.Contains(events[1].String(), "a.md") || !strings.Contains(events[3].Detail, "0 suggestion") {
		t.Fatalf("event text=%q / %q", events[1].String(), events[3].Detail)
	}
}

func TestRunRecordsLLMErrorPerDocument(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{err: os.ErrClosed}

	r, err := Run(ctx, c, db, llm)
	if err != nil {
		t.Fatal(err)
	}
	if r.Processed != 0 || r.Failed != 1 {
		t.Fatalf("result=%+v", r)
	}
	// Failure leaves the doc pending (status=error, hash mismatch remains).
	docs, _ := db.Pending(ctx, "test-model", "test-model", false)
	if len(docs) != 1 || docs[0].Status != "error" || docs[0].LastError == "" {
		t.Fatalf("docs=%+v", docs)
	}
}

func TestRunRecordsInvalidJSON(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}

	r, err := Run(ctx, c, db, &fakeLLM{reply: strings.Repeat("x", 1200)})
	if err != nil {
		t.Fatal(err)
	}
	if r.Processed != 0 || r.Failed != 1 {
		t.Fatalf("result=%+v", r)
	}
	docs, err := db.Pending(ctx, "test-model", "test-model", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Status != "error" || !strings.Contains(docs[0].LastError, "invalid JSON") {
		t.Fatalf("pending docs=%+v", docs)
	}
	if len(docs[0].LastError) >= 1200 {
		t.Fatalf("LLM response was not truncated: error length=%d", len(docs[0].LastError))
	}
}

func TestRunRecordsOversizedDocumentWithoutCallingLLM(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	c.MaxDocumentBytes = 4
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: `{"title":"should not run"}`}
	r, err := Run(ctx, c, db, llm)
	if err != nil {
		t.Fatal(err)
	}
	if r.Processed != 0 || r.Failed != 1 || llm.prompt != "" {
		t.Fatalf("result=%+v prompt=%q", r, llm.prompt)
	}
	docs, err := db.Pending(ctx, "test-model", "test-model", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Status != "error" || !strings.Contains(docs[0].LastError, "max_document_bytes") {
		t.Fatalf("pending docs=%+v", docs)
	}
}

func TestRunAbortsWhenPromptFilesAreMissing(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(c.PromptDirectory, "analyze.md")); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: `{"title":"unused"}`}
	r, err := Run(ctx, c, db, llm)
	if err == nil {
		t.Fatal("expected missing prompt error")
	}
	if r.Processed != 0 || r.Failed != 0 || llm.prompt != "" {
		t.Fatalf("result=%+v prompt=%q", r, llm.prompt)
	}
}

func TestRunDropsSelfMovesAndNormalizesTags(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: `{"title":"Alpha","summary":"S","tags":["a","New Tag","new-tag"],"obsolete":false,
		"suggestions":[{"action":"move","target_path":"a.md","reason":"self move","confidence":0.5,"proposed_content":""},
		               {"action":"expand","target_path":"","reason":"more detail","confidence":0.4,"proposed_content":"# Alpha doc\n\nMore"}]}`}

	r, err := Run(ctx, c, db, llm)
	if err != nil {
		t.Fatal(err)
	}
	if r.Processed != 1 {
		t.Fatalf("result=%+v", r)
	}
	sugs, err := db.PendingSuggestions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sugs) != 1 || sugs[0].Action != "expand" {
		t.Fatalf("self-move was not dropped: %+v", sugs)
	}
	docs, err := db.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Path tag "a" (from a.md) merged with LLM tags; "New Tag" and "new-tag" dedupe.
	if len(docs) != 1 || docs[0].TagsJSON != `["a","new-tag"]` {
		t.Fatalf("tags=%s", docs[0].TagsJSON)
	}
}

func TestRunDoesNotCallLLMWhenNothingIsPending(t *testing.T) {
	c, db := setup(t)
	llm := &fakeLLM{}
	result, err := Run(context.Background(), c, db, llm)
	if err != nil {
		t.Fatal(err)
	}
	if result != (Result{}) || llm.prompt != "" || len(llm.format) != 0 {
		t.Fatalf("result=%+v prompt=%q format=%s", result, llm.prompt, llm.format)
	}
}

func TestReanalyzeAllForcesStableDocuments(t *testing.T) {
	c, db := setup(t)
	c.EnrichmentMode = config.EnrichmentModeAlways
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	reply := `{"title":"Alpha","summary":"Summary","tags":["alpha"],"obsolete":false,"suggestions":[]}`
	first, err := RunProgress(t.Context(), c, db, Generators{Analysis: &fakeLLM{reply: reply}}, RunOptions{})
	if err != nil || first.Processed != 1 {
		t.Fatalf("initial analysis=%+v error=%v", first, err)
	}
	secondModel := &fakeLLM{reply: reply}
	second, err := RunProgress(t.Context(), c, db, Generators{Analysis: secondModel}, RunOptions{ReanalyzeAll: true})
	if err != nil || second.Processed != 1 || len(secondModel.prompts) != 1 {
		t.Fatalf("forced analysis=%+v prompts=%d error=%v", second, len(secondModel.prompts), err)
	}
}

func TestRunSkipsDocumentsExcludedByMCPPolicy(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	privateBody := "PRIVATE BODY MUST NOT REACH THE MODEL"
	content := "---\ntitle: Private note\nkb_policy:\n  no_llm_context: true\n---\n" + privateBody
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "a.md"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: `{"title":"ignored","summary":"ignored","tags":[],"obsolete":false,"suggestions":[]}`}
	result, err := Run(ctx, c, db, llm)
	if err != nil {
		t.Fatal(err)
	}
	if result.Processed != 0 || result.Skipped != 1 || result.Failed != 0 {
		t.Fatalf("result=%+v", result)
	}
	if len(llm.prompts) != 0 || len(llm.format) != 0 {
		t.Fatalf("policy-excluded document reached LLM: prompts=%d format=%s", len(llm.prompts), llm.format)
	}
	pending, err := db.Pending(ctx, "test-model", "test-model", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("policy skip remained pending: %+v", pending)
	}
	docs, err := db.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Status != "analyzed" ||
		docs[0].Title != "Private note" ||
		!strings.Contains(docs[0].Summary, "skipped") {
		t.Fatalf("policy skip was not recorded locally: %+v", docs)
	}
}

func TestRunOmitsPolicyExcludedPathsFromOtherPrompts(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	private := "---\ntitle: Private\nkb_policy:\n  no_llm_context: true\nrelated: [a.md]\n---\nprivate body"
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "private.md"), []byte(private), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: `{"title":"A","summary":"S","tags":[],"obsolete":false,"suggestions":[]}`}
	result, err := Run(ctx, c, db, llm)
	if err != nil {
		t.Fatal(err)
	}
	if result.Processed != 1 || result.Skipped != 1 || len(llm.prompts) != 1 {
		t.Fatalf("result=%+v prompts=%d", result, len(llm.prompts))
	}
	if strings.Contains(llm.prompts[0], "private.md") || strings.Contains(llm.prompts[0], "private body") {
		t.Fatalf("policy-excluded document leaked into another prompt:\n%s", llm.prompts[0])
	}
}

func TestRunSkipsStubTaggedDocuments(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	stub := "---\ntitle: Assembly\ntags: [stub]\nrelated: [gone.md]\n---\n# Assembly\n\nPlaceholder — see the language note.\n"
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "a.md"), []byte(stub), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: `{"title":"ignored","summary":"ignored","tags":[],"obsolete":false,"suggestions":[]}`}
	result, err := Run(ctx, c, db, llm)
	if err != nil {
		t.Fatal(err)
	}
	if result.Processed != 0 || result.Stubs != 1 || result.Skipped != 0 || result.Failed != 0 {
		t.Fatalf("result=%+v", result)
	}
	if len(llm.prompts) != 0 {
		t.Fatalf("stub document reached LLM: prompts=%d", len(llm.prompts))
	}
	// The deterministic link repair still runs: gone.md resolves to nothing.
	sugs, err := db.PendingSuggestions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sugs) != 1 || sugs[0].Origin != "deterministic" || strings.Contains(sugs[0].ProposedContent, "gone.md") {
		t.Fatalf("sugs=%+v", sugs)
	}
	docs, err := db.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Status != "analyzed" ||
		!strings.Contains(docs[0].Summary, "stub") {
		t.Fatalf("stub skip was not recorded locally: %+v", docs)
	}
}

func TestRunKeepsStubVisibleAsLinkCandidate(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	// a.md links to the stub: a valid, human-written link to a placeholder
	// must survive the deterministic check and reach the prompt as a
	// candidate — being linkable is the stub's whole job.
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "a.md"),
		[]byte("---\nrelated: [frameworks/asm.md]\n---\n# Alpha doc"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(c.RepositoryPath, "frameworks"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "frameworks", "asm.md"),
		[]byte("---\ntitle: Assembly\ntags: [stub]\n---\n# Assembly\n\nStub.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: `{"title":"A","summary":"S","tags":[],"obsolete":false,"suggestions":[]}`}
	result, err := Run(ctx, c, db, llm)
	if err != nil {
		t.Fatal(err)
	}
	if result.Processed != 1 || result.Stubs != 1 || len(llm.prompts) != 1 {
		t.Fatalf("result=%+v prompts=%d", result, len(llm.prompts))
	}
	if !strings.Contains(llm.prompts[0], "frameworks/asm.md") {
		t.Fatalf("stub missing from another document's prompt context:\n%s", llm.prompts[0])
	}
	sugs, err := db.PendingSuggestions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sugs {
		if s.Origin == "deterministic" && strings.Contains(s.Reason, "asm.md") {
			t.Fatalf("valid link to stub was flagged as unresolved: %+v", s)
		}
	}
}

func TestRunSkipsSmallBodyDocumentsAsStubs(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	c.StubMaxBodyLines = 5 // production default: at most 5 non-empty body lines
	// a.md has real content; b.md is a 3-line placeholder with no stub tag.
	os.WriteFile(filepath.Join(c.RepositoryPath, "a.md"),
		[]byte("---\ntitle: Alpha\n---\n# Alpha\n\nline\nline\nline\nline\nline\n"), 0644)
	os.WriteFile(filepath.Join(c.RepositoryPath, "b.md"),
		[]byte("---\ntitle: Assembly\n---\n# Assembly\n\nSee languages/.\n"), 0644)
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: `{"title":"A","summary":"S","tags":[],"obsolete":false,"suggestions":[]}`}
	result, err := Run(ctx, c, db, llm)
	if err != nil {
		t.Fatal(err)
	}
	if result.Processed != 1 || result.Stubs != 1 || result.Failed != 0 || len(llm.prompts) != 1 {
		t.Fatalf("result=%+v prompts=%d", result, len(llm.prompts))
	}
	if !strings.Contains(llm.prompts[0], "a.md") {
		t.Fatalf("expected a.md prompt, got:\n%s", llm.prompts[0])
	}
}

func TestRunRetriesAnalysisOnceOnInvalidJSON(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{queue: []string{
		`{"title":"A\z"}`, // invalid escape — a systematic model slip
		`{"title":"A","summary":"S","tags":[],"obsolete":false,"suggestions":[]}`,
	}}
	result, err := Run(ctx, c, db, llm)
	if err != nil {
		t.Fatal(err)
	}
	if result.Processed != 1 || result.Failed != 0 || len(llm.prompts) != 2 {
		t.Fatalf("result=%+v prompts=%d", result, len(llm.prompts))
	}
	if !strings.Contains(llm.prompts[1], "failed JSON parsing") {
		t.Fatalf("retry prompt lacks the parser error note:\n%s", llm.prompts[1])
	}
}

func TestRunFailsAnalysisAfterRetryStillInvalid(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: `{"title":`}
	result, err := Run(ctx, c, db, llm)
	if err != nil {
		t.Fatal(err)
	}
	if result.Processed != 0 || result.Failed != 1 || len(llm.prompts) != 2 {
		t.Fatalf("result=%+v prompts=%d", result, len(llm.prompts))
	}
}

func TestRunFeedsFrontmatterContextAndMergesTags(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	os.WriteFile(filepath.Join(c.RepositoryPath, "linked.md"), []byte("# Link target"), 0644)
	// Overwrite a.md with curated frontmatter: a resolvable and a broken link.
	os.WriteFile(filepath.Join(c.RepositoryPath, "a.md"), []byte(`---
title: Alpha
tags: [declared]
related: [linked.md, gone.md]
tier: golden
---
# Alpha doc`), 0644)
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: `{"title":"A","summary":"S","tags":["llm-tag"],"obsolete":false,"suggestions":[]}`}
	if _, err := Run(ctx, c, db, llm); err != nil {
		t.Fatal(err)
	}
	// a.md is analysed first (paths order alphabetically) — inspect its prompt.
	aPrompt := llm.prompts[0]
	for _, want := range []string{
		`Document frontmatter`, `"title":"Alpha"`,
		"Frontmatter issue:", `tier "golden"`,
		`Frontmatter related target "gone.md" does not match any known document`,
	} {
		if !strings.Contains(aPrompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, aPrompt)
		}
	}
	// The resolvable link is not flagged.
	if strings.Contains(aPrompt, `"linked.md" does not match`) {
		t.Fatalf("valid link flagged:\n%s", aPrompt)
	}
	// Frontmatter-declared tags merge into the stored tags.
	docs, _ := db.Documents(ctx)
	var a database.Document
	for _, d := range docs {
		if d.Path == "a.md" {
			a = d
		}
	}
	if !strings.Contains(a.TagsJSON, `"declared"`) || !strings.Contains(a.TagsJSON, `"llm-tag"`) {
		t.Fatalf("tags=%q", a.TagsJSON)
	}
}

func TestRunDropsMalformedAndInvalidProposedFrontmatter(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: `{"title":"A","summary":"S","tags":[],"obsolete":false,"suggestions":[
		{"action":"expand","target_path":"","reason":"bad yaml","confidence":0.5,"proposed_content":"---\ntitle: [\n---\nbody"},
		{"action":"expand","target_path":"","reason":"fix tier","confidence":0.5,"proposed_content":"---\ntier: golden\n---\n# Alpha doc"},
		{"action":"expand","target_path":"","reason":"clean","confidence":0.5,"proposed_content":"---\ntitle: A\n---\n# Alpha doc"}
	]}`}
	if _, err := Run(ctx, c, db, llm); err != nil {
		t.Fatal(err)
	}
	sugs, _ := db.PendingSuggestions(ctx)
	if len(sugs) != 1 || sugs[0].Reason != "clean" {
		t.Fatalf("expected only the valid proposal, got %+v", sugs)
	}
	for _, s := range sugs {
		if strings.Contains(s.Reason, "bad yaml") {
			t.Fatalf("malformed proposal kept: %+v", s)
		}
	}
}

func TestRunDropsProseProposedContent(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	// Commentary must not replace a document even if it is valid JSON.
	body := "# Alpha doc\\n\\n" + strings.Repeat("the quick brown fox jumps over lazy dogs in the repository ", 20)
	os.WriteFile(filepath.Join(c.RepositoryPath, "a.md"), []byte(strings.ReplaceAll(body, "\\n", "\n")), 0644)
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: `{"title":"A","summary":"S","tags":[],"obsolete":false,"suggestions":[
		{"action":"expand","target_path":"","reason":"commentary not content","confidence":0.9,
		 "proposed_content":"The document should include a related field pointing to other docs."},
		{"action":"expand","target_path":"","reason":"real rewrite","confidence":0.9,
		 "proposed_content":"` + body + `\\n\\n## Added section\\n\\nMore detail here."}
	]}`}
	if _, err := Run(ctx, c, db, llm); err != nil {
		t.Fatal(err)
	}
	sugs, _ := db.PendingSuggestions(ctx)
	if len(sugs) != 1 || sugs[0].Reason != "real rewrite" {
		t.Fatalf("expected only the real rewrite kept, got %+v", sugs)
	}
}

func TestRunReportsInboundLinksAndUnreferencedSiblings(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	// A project note that already links to a.md, plus an unmentioned script
	// sitting next to a.md.
	os.WriteFile(filepath.Join(c.RepositoryPath, "project.md"),
		[]byte("---\nrelated: [a.md]\n---\n# Project uses alpha\n"), 0644)
	os.WriteFile(filepath.Join(c.RepositoryPath, "deploy.sh"), []byte("#!/bin/sh\n"), 0644)
	os.WriteFile(filepath.Join(c.RepositoryPath, "notes.txt"), []byte("mentioned below\n"), 0644)
	os.WriteFile(filepath.Join(c.RepositoryPath, "a.md"), []byte("# Alpha doc\n\nSee notes.txt for details.\n"), 0644)
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: `{"title":"A","summary":"S","tags":[],"obsolete":false,"suggestions":[]}`}
	if _, err := Run(ctx, c, db, llm); err != nil {
		t.Fatal(err)
	}
	// a.md is processed first; its prompt should carry the new context.
	aPrompt := llm.prompts[0]
	for _, want := range []string{
		"Inbound links", "- project.md",
		"Sibling files the document does not mention", "deploy.sh",
		"File modified:",
	} {
		if !strings.Contains(aPrompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, aPrompt)
		}
	}
	// notes.txt IS mentioned in the body — not reported as unreferenced.
	if strings.Contains(aPrompt, "- notes.txt") && strings.Contains(aPrompt, "does not mention") {
		t.Fatalf("referenced sibling wrongly flagged:\n%s", aPrompt)
	}
}

func TestTwoModelRunEnrichesThenAnalyzes(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	c.OllamaModelFast = "fast-model"
	os.WriteFile(filepath.Join(c.RepositoryPath, "a.md"), []byte(`---
title: Alpha
kb_policy:
  resource_read: allow
---
# Alpha doc about containers`), 0644)
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	// Unsafe fields are ignored even when a real model violates the schema.
	fast := &fakeLLM{reply: `{"title":"Alpha","summary":"fast summary","tags":["containers"],
		"frontmatter_patch":{"set":{"tags":["containers"],"tier":"working","kb_policy":{"no_llm_context":true},
		"kb_policy.resource_read":"deny"},"remove":["kb_policy.sensitivity"]}}`}
	slow := &fakeLLM{reply: `{"title":"Alpha","summary":"deep summary","tags":[],"obsolete":false,"suggestions":[]}`}

	r, err := RunProgress(ctx, c, db, Generators{Analysis: slow, Enrich: fast}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Processed != 1 || r.Enriched != 1 {
		t.Fatalf("result=%+v", r)
	}
	if len(fast.prompts) != 1 || !strings.Contains(fast.prompts[0], "ENRICH") {
		t.Fatalf("enrich prompt=%v", fast.prompts)
	}
	if len(slow.prompts) != 1 || !strings.Contains(slow.prompts[0], "TASK") {
		t.Fatalf("analysis prompt=%v", slow.prompts)
	}
	docs, _ := db.Documents(ctx)
	if docs[0].EnrichedModel != "fast-model" || docs[0].EnrichedSHA256 != docs[0].SHA256 {
		t.Fatalf("enrichment markers=%+v", docs[0])
	}
	// The analysis pass ran last, so its summary wins.
	if docs[0].Summary != "deep summary" {
		t.Fatalf("summary=%q", docs[0].Summary)
	}
	sugs, _ := db.PendingSuggestions(ctx)
	if len(sugs) != 1 || sugs[0].Origin != "enrich" || sugs[0].Action != "expand" {
		t.Fatalf("sugs=%+v", sugs)
	}
	if !strings.Contains(sugs[0].ProposedContent, "- containers") {
		t.Fatalf("patch not applied:\n%s", sugs[0].ProposedContent)
	}
	if strings.Contains(sugs[0].ProposedContent, "no_llm_context") ||
		strings.Contains(sugs[0].ProposedContent, "kb_policy.") ||
		strings.Contains(sugs[0].ProposedContent, "tier:") ||
		strings.Contains(sugs[0].ProposedContent, `"deny"`) {
		t.Fatalf("protected field leak into patch:\n%s", sugs[0].ProposedContent)
	}
}

func TestDeterministicFixRemovesOnlyUnknownLinks(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	os.WriteFile(filepath.Join(c.RepositoryPath, "real.md"), []byte("# Real"), 0644)
	os.WriteFile(filepath.Join(c.RepositoryPath, "a.md"), []byte(`---
related: [real.md, gone.md, ../escape.md]
---
# Alpha doc`), 0644)
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: `{"title":"A","summary":"S","tags":[],"obsolete":false,"suggestions":[]}`}
	if _, err := Run(ctx, c, db, llm); err != nil {
		t.Fatal(err)
	}
	sugs, _ := db.PendingSuggestions(ctx)
	var det *database.Suggestion
	for i := range sugs {
		if sugs[i].Origin == "deterministic" {
			det = &sugs[i]
		}
	}
	if det == nil {
		t.Fatalf("no deterministic suggestion: %+v", sugs)
	}
	if det.Confidence != 1.0 || det.Action != "expand" {
		t.Fatalf("det=%+v", det)
	}
	if !strings.Contains(det.Reason, "gone.md") || !strings.Contains(det.Reason, "../escape.md") {
		t.Fatalf("reason=%q", det.Reason)
	}
	// The valid link stays; the broken and invalid ones are gone.
	if !strings.Contains(det.ProposedContent, "real.md") ||
		strings.Contains(det.ProposedContent, "gone.md") ||
		strings.Contains(det.ProposedContent, "escape.md") {
		t.Fatalf("proposed content:\n%s", det.ProposedContent)
	}
}

func TestEnrichOnlyRunForAlreadyAnalyzedDoc(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	// Complete a normal single-model analysis first.
	slow := &fakeLLM{reply: `{"title":"A","summary":"S","tags":[],"obsolete":false,"suggestions":[]}`}
	if _, err := Run(ctx, c, db, slow); err != nil {
		t.Fatal(err)
	}
	// Now configure a fast model — the unchanged doc is pending for enrich only.
	c.OllamaModelFast = "fast-model"
	fast := &fakeLLM{reply: `{"title":"A2","summary":"fast","tags":[],"frontmatter_patch":{"set":{},"remove":[]}}`}
	r, err := RunProgress(ctx, c, db, Generators{Analysis: slow, Enrich: fast}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Processed != 0 || r.Enriched != 1 {
		t.Fatalf("result=%+v", r)
	}
	if len(fast.prompts) != 1 {
		t.Fatalf("enrich calls=%d", len(fast.prompts))
	}
	if len(slow.prompts) != 1 { // the first Run's call, nothing new
		t.Fatalf("analysis calls=%d", len(slow.prompts))
	}
	docs, _ := db.Pending(ctx, "test-model", "fast-model", false)
	if len(docs) != 0 {
		t.Fatalf("still pending: %+v", docs)
	}
}

// fakeEmbedder records embedded texts and returns a deterministic vector.
type fakeEmbedder struct {
	texts []string
	err   error
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	f.texts = append(f.texts, text)
	return []float32{0.5, 0.5, 0}, f.err
}

func TestRunBackfillsEmbeddingsForVisibleDocsOnly(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	os.WriteFile(filepath.Join(c.RepositoryPath, "visible.md"), []byte("# Visible doc\n\nSome body text."), 0644)
	os.WriteFile(filepath.Join(c.RepositoryPath, "private.md"),
		[]byte("---\nkb_policy:\n  no_llm_context: true\n---\n# Private\n\nMust never reach any model."), 0644)
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	c.OllamaEmbedModel = "test-embedder"
	emb := &fakeEmbedder{}
	llm := &fakeLLM{reply: `{"title":"A","summary":"S","tags":[],"obsolete":false,"suggestions":[]}`}
	if _, err := RunProgress(ctx, c, db, Generators{Analysis: llm, Embed: emb}, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, txt := range emb.texts {
		if strings.Contains(txt, "Must never reach") {
			t.Fatalf("private content was embedded: %v", emb.texts)
		}
	}
	if len(emb.texts) != 2 { // a.md from setup + visible.md — not private.md
		t.Fatalf("embedded texts=%v", emb.texts)
	}
	docs, _ := db.Documents(ctx)
	for _, d := range docs {
		hasVec := len(similarity.UnmarshalEmbedding(d.Embedding)) > 0
		if d.Path == "visible.md" && !hasVec {
			t.Fatal("visible doc has no stored embedding")
		}
		if d.Path == "private.md" && hasVec {
			t.Fatal("private doc was embedded — privacy violation")
		}
	}
}

func TestRankCandidatesPrefersSimilarityThenSameDirAndLinked(t *testing.T) {
	fm := map[string]any{"related": []any{"zzz/linked.md"}}
	cands := []database.Document{
		{Path: "aaa/far.md"},
		{Path: "runbooks/other.md"},
		{Path: "zzz/linked.md"},
		{Path: "sim/dup.md"},
	}
	docSig := similarity.Signature("alpha beta gamma delta epsilon")
	sigs := map[string][]uint64{
		"sim/dup.md": similarity.Signature("alpha beta gamma delta epsilon"),
	}
	got := rankCandidates("runbooks/deploy.md", fm, cands, docSig, sigs, nil, nil, nil, 50)
	want := []string{"sim/dup.md", "runbooks/other.md", "zzz/linked.md", "aaa/far.md"}
	if len(got) != len(want) {
		t.Fatalf("got=%v", got)
	}
	for i := range want {
		if got[i].doc.Path != want[i] {
			t.Fatalf("got=%v want=%v", got, want)
		}
	}
}

func TestRankCandidatesPrefersEmbeddingRelatedness(t *testing.T) {
	cands := []database.Document{
		{Path: "zzz/topical.md"},
		{Path: "runbooks/overlap.md"},
	}
	docSig := similarity.Signature("alpha beta gamma delta epsilon")
	sigs := map[string][]uint64{
		"runbooks/overlap.md": docSig, // literal overlap but different topic
	}
	docEmb := []float32{1, 0, 0}
	embeds := map[string][]float32{
		"zzz/topical.md": {0.9, 0.1, 0}, // topically close, no text overlap
	}
	got := rankCandidates("runbooks/deploy.md", nil, cands, docSig, sigs, docEmb, embeds, nil, 50)
	if got[0].doc.Path != "zzz/topical.md" {
		t.Fatalf("embedding-related doc should outrank text overlap: %+v", got)
	}
	// Without embeddings the shingle winner comes first — the fallback.
	got = rankCandidates("runbooks/deploy.md", nil, cands, docSig, sigs, nil, nil, nil, 50)
	if got[0].doc.Path != "runbooks/overlap.md" {
		t.Fatalf("shingle fallback broken: %+v", got)
	}
}

func TestNewNamedEvidenceReanalyzesUnchangedSource(t *testing.T) {
	c, db := setup(t)
	source := filepath.Join(c.RepositoryPath, "a.md")
	sourceText := "# UI choice\n\nUse UIkit for this interface.\n"
	if err := os.WriteFile(source, []byte(sourceText), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	firstModel := &fakeLLM{reply: `{"title":"UI choice","summary":"Summary","tags":["ui"],"obsolete":false,"suggestions":[]}`}
	first, err := RunProgress(t.Context(), c, db, Generators{Analysis: firstModel}, RunOptions{})
	if err != nil || first.Processed != 1 {
		t.Fatalf("initial analysis=%+v error=%v", first, err)
	}

	catalogDir := filepath.Join(c.RepositoryPath, "frameworks", "css")
	if err := os.MkdirAll(catalogDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(catalogDir, "README.md"),
		[]byte("# CSS frameworks\n\nCatalog of frameworks.\n\n- [UIkit](uikit.md) — modular CSS framework.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(catalogDir, "uikit.md"), []byte("# UIkit\n\nA modular CSS framework.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	secondModel := &fakeLLM{reply: `{"title":"Note","summary":"Summary","tags":["css"],"obsolete":false,"suggestions":[]}`}
	second, err := RunProgress(t.Context(), c, db, Generators{Analysis: secondModel}, RunOptions{})
	if err != nil || second.Processed != 3 {
		t.Fatalf("analysis after adding named evidence=%+v error=%v", second, err)
	}
	sourcePrompts := 0
	for _, prompt := range secondModel.prompts {
		if strings.Contains(prompt, "Current path: a.md\n") {
			sourcePrompts++
			if !strings.Contains(prompt, "frameworks/css/uikit.md") ||
				!strings.Contains(prompt, "Exact-name evidence from candidate notes") {
				t.Fatalf("dependent source prompt lacks new target evidence: %s", prompt)
			}
		}
	}
	if sourcePrompts != 1 {
		t.Fatalf("unchanged source was presented %d times, want one evidence refresh", sourcePrompts)
	}
	unchanged, err := os.ReadFile(source)
	if err != nil || frontmatter.Canonical(string(unchanged)) != frontmatter.Canonical(sourceText) {
		t.Fatalf("evidence refresh mutated source: %q error=%v", unchanged, err)
	}
}

func TestChangedEvidenceRefreshesSourcePendingOnlyForEnrichment(t *testing.T) {
	c, db := setup(t)
	c.OllamaModelFast = "fast"
	source := filepath.Join(c.RepositoryPath, "a.md")
	sourceText := "# UI choice\n\nUse UIkit for this interface.\n"
	if err := os.WriteFile(source, []byte(sourceText), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	fastFailure := &fakeLLM{err: errors.New("fake optional enrichment timeout")}
	firstAnalysis := &fakeLLM{reply: `{"title":"UI choice","summary":"Summary","tags":["ui"],"obsolete":false,"suggestions":[]}`}
	first, err := RunProgress(t.Context(), c, db, Generators{Enrich: fastFailure, Analysis: firstAnalysis}, RunOptions{})
	if err != nil || first.Processed != 1 || first.EnrichmentFailed != 1 {
		t.Fatalf("initial pass=%+v error=%v", first, err)
	}

	dir := filepath.Join(c.RepositoryPath, "frameworks", "css")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "uikit.md"), []byte("# UIkit\n\nA modular CSS framework.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	secondAnalysis := &fakeLLM{reply: `{"title":"Note","summary":"Summary","tags":["css"],"obsolete":false,"suggestions":[]}`}
	second, err := RunProgress(t.Context(), c, db, Generators{Enrich: fastFailure, Analysis: secondAnalysis}, RunOptions{})
	if err != nil || second.Processed != 2 {
		t.Fatalf("follow-up pass=%+v error=%v", second, err)
	}
	sourcePrompts := 0
	for _, prompt := range secondAnalysis.prompts {
		if strings.Contains(prompt, "Current path: a.md\n") &&
			strings.Contains(prompt, "frameworks/css/uikit.md") {
			sourcePrompts++
		}
	}
	if sourcePrompts != 1 {
		t.Fatalf("source with a successful old analysis was not refreshed after new evidence: %v", secondAnalysis.prompts)
	}
	if original, err := os.ReadFile(source); err != nil ||
		frontmatter.Canonical(string(original)) != frontmatter.Canonical(sourceText) {
		t.Fatalf("dependency refresh changed source: %q error=%v", original, err)
	}
}
