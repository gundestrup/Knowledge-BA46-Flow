package analyzer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	os.WriteFile(filepath.Join(promptDir, "expand.md"), []byte("EXPAND"), 0644)
	os.WriteFile(filepath.Join(promptDir, "freshness.md"), []byte("FRESHNESS"), 0644)
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

func TestRunRefusesUnscannedSourceChangesBeforeLLM(t *testing.T) {
	c, db := setup(t)
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	writeDoc(t, c.RepositoryPath, "a.md", "# Alpha doc\n\nChanged on disk after the scan.\n")
	model := &fakeLLM{reply: `{"title":"Alpha","summary":"s","tags":[],"suggestions":[]}`}
	result, err := RunProgress(t.Context(), c, db, Generators{Analysis: model}, RunOptions{ReanalyzeAll: true})
	if err != nil || result.Failed != 1 || len(model.prompts) != 0 {
		t.Fatalf("result=%+v prompts=%d err=%v; stale bytes must not reach the model", result, len(model.prompts), err)
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
	if docs[0].EnrichedModel != "fast-model" || docs[0].EnrichedSHA256 != docs[0].EnrichmentInputSHA256 {
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

func TestBodyOnlyChangeRefreshesOnlyConsumedBodyEvidence(t *testing.T) {
	c, db := setup(t)
	target := filepath.Join(c.RepositoryPath, "quasar.md")
	consumer := filepath.Join(c.RepositoryPath, "consumer.md")
	unrelated := filepath.Join(c.RepositoryPath, "unrelated.md")
	if err := os.WriteFile(target, []byte("---\ntitle: Quasar\n---\n# Quasar\n\n"+strings.Repeat("targetword ", 40)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(consumer, []byte("# Consumer\n\nQuasar is the important system for this note.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unrelated, []byte("# Unrelated\n\n"+strings.Repeat("unrelatedword ", 40)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	firstModel := &fakeLLM{reply: `{"title":"Note","summary":"Summary","tags":["x"],"obsolete":false,"suggestions":[]}`}
	if first, err := RunProgress(t.Context(), c, db, Generators{Analysis: firstModel}, RunOptions{}); err != nil || first.Processed != 4 {
		t.Fatalf("initial analysis=%+v error=%v", first, err)
	}

	// Body-only edit leaves frontmatter unchanged. Only the note that actually
	// received the target's entity excerpt should refresh.
	if err := os.WriteFile(target, []byte("---\ntitle: Quasar\n---\n# Quasar\n\n"+strings.Repeat("rewrittenword ", 40)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	secondModel := &fakeLLM{reply: `{"title":"Note","summary":"Summary","tags":["x"],"obsolete":false,"suggestions":[]}`}
	second, err := RunProgress(t.Context(), c, db, Generators{Analysis: secondModel}, RunOptions{})
	if err != nil || second.Processed != 2 || second.Dependents != 1 {
		t.Fatalf("body-only edit should reprocess the changed doc plus its actual evidence consumer: %+v error=%v", second, err)
	}
	consumerSeen := false
	for _, prompt := range secondModel.prompts {
		if strings.Contains(prompt, "Current path: consumer.md") {
			consumerSeen = true
		}
		if strings.Contains(prompt, "Current path: unrelated.md") {
			t.Fatal("unrelated note re-analyzed on another note's body-only edit")
		}
	}
	if !consumerSeen {
		t.Fatal("body evidence consumer did not refresh")
	}
}

func TestTitleOnlyChangeRefreshesOnlyMentionConsumers(t *testing.T) {
	c, db := setup(t)
	linker := filepath.Join(c.RepositoryPath, "linker.md")
	target := filepath.Join(c.RepositoryPath, "target.md")
	if err := os.WriteFile(linker, []byte("# Linker\n\nSee [Target](target.md).\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("---\ntitle: Old title\n---\n# Old title\n\nBody.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	firstModel := &fakeLLM{reply: `{"title":"Note","summary":"Summary","tags":["x"],"obsolete":false,"suggestions":[]}`}
	if first, err := RunProgress(t.Context(), c, db, Generators{Analysis: firstModel}, RunOptions{}); err != nil || first.Processed != 3 {
		t.Fatalf("initial analysis=%+v error=%v", first, err)
	}

	// A title-only edit changes what prompts show for the target — but only
	// docs that *consumed* it refresh: the linker mentions it as an entity
	// (relied-upon annotation), while the fixture's a.md merely had it in its
	// ranked candidate list (displayed, not a dependency) and must not fire.
	if err := os.WriteFile(target, []byte("---\ntitle: New title\n---\n# New title\n\nBody.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	secondModel := &fakeLLM{reply: `{"title":"Note","summary":"Summary","tags":["x"],"obsolete":false,"suggestions":[]}`}
	second, err := RunProgress(t.Context(), c, db, Generators{Analysis: secondModel}, RunOptions{})
	if err != nil || second.Processed != 2 {
		t.Fatalf("title-only edit should reprocess target + its mention consumer: %+v error=%v", second, err)
	}
	processed := map[string]bool{}
	for _, prompt := range secondModel.prompts {
		for _, line := range strings.Split(prompt, "\n") {
			if strings.HasPrefix(line, "Current path: ") {
				processed[strings.TrimPrefix(line, "Current path: ")] = true
			}
		}
	}
	if !processed["linker.md"] {
		t.Fatalf("linker must refresh — it consumed the target as an entity mention")
	}
	if processed["a.md"] {
		t.Fatalf("a.md must not re-analyze — the target was only a displayed candidate")
	}
}

func TestInboundEvidenceChangesOnlyWhenLinkTopologyChanges(t *testing.T) {
	c, db := setup(t)
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(c.RepositoryPath, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("target.md", "---\ntitle: Target\n---\n# Target\n\nUnrelated target prose.\n")
	write("z-source.md", "---\ntitle: Old source\nrelated:\n  - target.md\n---\n# Source\n\nDistinctive source material.\n")
	for i := 0; i < 20; i++ {
		write(fmt.Sprintf("a-noise-%02d.md", i), fmt.Sprintf("# Noise %d\n\nUnique unrelated token%d material.\n", i, i))
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	reply := `{"title":"Processed","summary":"Summary","tags":["x"],"obsolete":false,"suggestions":[]}`
	if _, err := RunProgress(t.Context(), c, db, Generators{Analysis: &fakeLLM{reply: reply}}, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	targetDoc, err := db.Documents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var targetRow database.Document
	for _, doc := range targetDoc {
		if doc.Path == "target.md" {
			targetRow = doc
			break
		}
	}
	ev, ok := parseAnalysisEvidenceRecord(targetRow.AnalysisEvidenceJSON)
	if !ok || !slices.Contains(ev.Inbound, "z-source.md") || slices.Contains(ev.Fm, "z-source.md") {
		t.Fatalf("expected inbound path-only evidence outside ranked annotations: %+v", ev)
	}

	// A title edit to an incoming source does not change the target's inbound
	// path list; the target did not see that source's title in its prompt.
	write("z-source.md", "---\ntitle: New source\nrelated:\n  - target.md\n---\n# Source\n\nDistinctive source material.\n")
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	titleEditModel := &fakeLLM{reply: reply}
	if _, err := RunProgress(t.Context(), c, db, Generators{Analysis: titleEditModel}, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, prompt := range titleEditModel.prompts {
		if strings.Contains(prompt, "Current path: target.md") {
			t.Fatal("inbound target was refreshed although its path-only evidence was unchanged")
		}
	}

	// Removing the relationship changes the actual inbound context and must
	// refresh the target, even though neither its source nor own body changed.
	write("z-source.md", "---\ntitle: New source\n---\n# Source\n\nDistinctive source material.\n")
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	inboundAfterRemoval, err := db.InboundLinks(t.Context(), "target.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(inboundAfterRemoval) != 0 {
		t.Fatalf("scanner did not update inbound index after link removal: %v", inboundAfterRemoval)
	}
	topologyModel := &fakeLLM{reply: reply}
	if _, err := RunProgress(t.Context(), c, db, Generators{Analysis: topologyModel}, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	targetPrompts := 0
	for _, prompt := range topologyModel.prompts {
		if strings.Contains(prompt, "Current path: target.md") {
			targetPrompts++
		}
	}
	if targetPrompts != 1 {
		t.Fatalf("target should refresh once when inbound topology changes, got %d", targetPrompts)
	}
}

func TestCommonFilenameWordsDoNotFanOutDependents(t *testing.T) {
	c, db := setup(t)
	// Five notes that merely share the word "migration" — a new note named
	// for that word must not re-analyze them: the word is vocabulary, not a
	// name. One note also mentions the rare word "uikify" and stays a
	// genuine dependent.
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("note-%d.md", i)
		body := fmt.Sprintf("# Note %d\n\nThe migration plan is documented elsewhere.\n", i)
		if i == 0 {
			body = "# Note 0\n\nUse uikify for this interface; migration notes are elsewhere.\n"
		}
		if err := os.WriteFile(filepath.Join(c.RepositoryPath, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	firstModel := &fakeLLM{reply: `{"title":"Note","summary":"Summary","tags":["x"],"obsolete":false,"suggestions":[]}`}
	if _, err := RunProgress(t.Context(), c, db, Generators{Analysis: firstModel}, RunOptions{}); err != nil {
		t.Fatalf("initial analysis error=%v", err)
	}
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "db-migration-uikify-runbook.md"),
		[]byte("# DB migration runbook\n\nSteps for the migration.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	secondModel := &fakeLLM{reply: `{"title":"Note","summary":"Summary","tags":["x"],"obsolete":false,"suggestions":[]}`}
	second, err := RunProgress(t.Context(), c, db, Generators{Analysis: secondModel}, RunOptions{})
	if err != nil {
		t.Fatalf("run error=%v", err)
	}
	// New note plus exactly one dependent — the rare-word match, not the
	// five common-word mentions.
	if second.Processed != 2 {
		t.Fatalf("expected new note + rare-word dependent only, got %+v", second)
	}
	refreshed := 0
	for _, prompt := range secondModel.prompts {
		if strings.Contains(prompt, "Current path: note-0.md") {
			refreshed++
		}
		for i := 1; i < 5; i++ {
			if strings.Contains(prompt, fmt.Sprintf("Current path: note-%d.md", i)) {
				t.Fatalf("note-%d re-analyzed on shared vocabulary", i)
			}
		}
	}
	if refreshed != 1 {
		t.Fatalf("rare-word dependent expected once, got %d", refreshed)
	}
}

func TestCatalogIndexEditRefreshesOnlyNewInboundTargets(t *testing.T) {
	c, db := setup(t)
	// Production case: an index page that every section note links back to.
	index := filepath.Join(c.RepositoryPath, "lessons", "README.md")
	if err := os.MkdirAll(filepath.Dir(index), 0755); err != nil {
		t.Fatal(err)
	}
	writeIndex := func(extra string) {
		content := "---\ndocument_id: 33333333-3333-4333-8333-333333333333\ntitle: Lessons index\n---\n# Lessons index\n\n| Note | Related |\n| --- | --- |\n" + extra
		if err := os.WriteFile(index, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeIndex("| [One](one.md) | - |\n")
	for _, name := range []string{"one.md", "two.md", "three.md", "four.md", "five.md"} {
		body := "# Shared note\n\nCatalog: [overview](README.md)\n\nCommon catalog navigation text.\n"
		if err := os.WriteFile(filepath.Join(c.RepositoryPath, "lessons", name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Keep the README outside the ranked candidate window for old notes. This
	// isolates the link-topology change from semantic candidate evidence.
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("noise-%02d.md", i)
		body := "# Shared note\n\nCatalog: [overview](README.md)\n\nCommon catalog navigation text.\n"
		if err := os.WriteFile(filepath.Join(c.RepositoryPath, "lessons", name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// An unrelated note neither links to nor ranks the index.
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "lessons", "other.md"),
		[]byte("# Other\n\nUnrelated content about databases.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	firstModel := &fakeLLM{reply: `{"title":"Note","summary":"Summary","tags":["x"],"obsolete":false,"suggestions":[]}`}
	if _, err := RunProgress(t.Context(), c, db, Generators{Analysis: firstModel}, RunOptions{}); err != nil {
		t.Fatalf("initial analysis error=%v", err)
	}
	// Net change: four appended catalog rows. The index's frontmatter stays
	// unchanged; only the four new target inbound sets change.
	writeIndex("| [One](one.md) | - |\n| [Two](two.md) | - |\n| [Three](three.md) | - |\n| [Four](four.md) | - |\n| [Five](five.md) | - |\n")
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	secondModel := &fakeLLM{reply: `{"title":"Note","summary":"Summary","tags":["x"],"obsolete":false,"suggestions":[]}`}
	second, err := RunProgress(t.Context(), c, db, Generators{Analysis: secondModel}, RunOptions{})
	if err != nil {
		t.Fatalf("run error=%v", err)
	}
	var processedPaths []string
	for _, prompt := range secondModel.prompts {
		for _, line := range strings.Split(prompt, "\n") {
			if strings.HasPrefix(line, "Current path: ") {
				processedPaths = append(processedPaths, strings.TrimPrefix(line, "Current path: "))
				break
			}
		}
	}
	// The index plus the four notes newly added to its inbound list. The
	// existing row and all other catalog linkers keep the same context.
	if second.Processed != 5 || second.Dependents != 4 {
		t.Fatalf("expected index + four changed inbound contexts, got %+v paths=%v", second, processedPaths)
	}
	for _, prompt := range secondModel.prompts {
		if strings.Contains(prompt, "Current path: lessons/one.md") ||
			strings.Contains(prompt, "Current path: lessons/other.md") ||
			strings.Contains(prompt, "Current path: lessons/noise-") {
			t.Fatalf("unchanged catalog linker re-analyzed on index-row edit")
		}
	}
}

func TestDeletedDocumentSeedsEvidenceDependents(t *testing.T) {
	c, db := setup(t)
	linker := filepath.Join(c.RepositoryPath, "linker.md")
	target := filepath.Join(c.RepositoryPath, "target.md")
	if err := os.WriteFile(linker, []byte("# Linker\n\nSee [Target](target.md).\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("# Target\n\nBody.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	firstModel := &fakeLLM{reply: `{"title":"Note","summary":"Summary","tags":["x"],"obsolete":false,"suggestions":[]}`}
	if first, err := RunProgress(t.Context(), c, db, Generators{Analysis: firstModel}, RunOptions{}); err != nil || first.Processed != 3 {
		t.Fatalf("initial analysis=%+v error=%v", first, err)
	}

	// Deleting a cited note is evidence drift: every doc that consumed it —
	// at either level — refreshes, but nothing else does.
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	estimate, err := EstimateAnalysis(t.Context(), c, db, RunOptions{})
	if err != nil {
		t.Fatalf("estimate error=%v", err)
	}
	if estimate.Pending != 0 || estimate.Dependents != 2 || estimate.Total != 2 {
		t.Fatalf("deletion estimate should include evidence consumers, got %+v", estimate)
	}
	needsSweep, err := db.NeedsRemovalSweep(t.Context(), false)
	if err != nil || !needsSweep {
		t.Fatalf("estimate must not acknowledge removal events: needed=%v err=%v", needsSweep, err)
	}
	failedModel := &fakeLLM{err: errors.New("temporary model failure")}
	failed, err := RunProgress(t.Context(), c, db, Generators{Analysis: failedModel}, RunOptions{})
	if err != nil || failed.Failed != 2 {
		t.Fatalf("failed dependent run=%+v error=%v, want two retryable document failures", failed, err)
	}
	needsSweep, err = db.NeedsRemovalSweep(t.Context(), false)
	if err != nil || !needsSweep {
		t.Fatalf("failed dependent analysis must remain retryable: needed=%v err=%v", needsSweep, err)
	}
	secondModel := &fakeLLM{reply: `{"title":"Note","summary":"Summary","tags":["x"],"obsolete":false,"suggestions":[]}`}
	second, err := RunProgress(t.Context(), c, db, Generators{Analysis: secondModel}, RunOptions{})
	if err != nil {
		t.Fatalf("run error=%v", err)
	}
	if second.Processed != 2 || second.Dependents != 2 {
		t.Fatalf("deletion should refresh only evidence consumers, got %+v", second)
	}
	seen := map[string]bool{}
	for _, prompt := range secondModel.prompts {
		for _, path := range []string{"linker.md", "a.md"} {
			if strings.Contains(prompt, "Current path: "+path) {
				seen[path] = true
			}
		}
	}
	if !seen["linker.md"] || !seen["a.md"] {
		t.Fatalf("expected evidence consumers refreshed after deletion, got %v", seen)
	}
	// The tombstone remains in SQLite, but the local lane acknowledged this
	// deletion generation. A later run must not repeatedly re-analyze stale
	// frontmatter references to the same missing path.
	thirdModel := &fakeLLM{reply: secondModel.reply}
	third, err := RunProgress(t.Context(), c, db, Generators{Analysis: thirdModel}, RunOptions{})
	if err != nil || third.Processed != 0 || third.Dependents != 0 {
		t.Fatalf("deletion evidence should be idempotent after one sweep: %+v error=%v", third, err)
	}
	needsExternal, err := db.NeedsRemovalSweep(t.Context(), true)
	if err != nil || !needsExternal {
		t.Fatalf("local acknowledgement must not consume external deletion work: needed=%v err=%v", needsExternal, err)
	}
}

func TestExternalLaneMeasuresDriftAgainstOwnSeenStamp(t *testing.T) {
	c, db := setup(t)
	c.OllamaModelExternal = "ext-model"
	linker := filepath.Join(c.RepositoryPath, "linker.md")
	target := filepath.Join(c.RepositoryPath, "target.md")
	if err := os.WriteFile(linker, []byte("# Linker\n\nSee [Target](target.md).\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("---\ntitle: Old\n---\n# Old\n\nBody.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	externalModel := &fakeLLM{reply: `{"title":"Note","summary":"Summary","tags":["x"],"obsolete":false,"suggestions":[]}`}
	if _, err := RunProgress(t.Context(), c, db, Generators{Analysis: externalModel}, RunOptions{External: true}); err != nil {
		t.Fatalf("external run error=%v", err)
	}
	var originalExternalEvidence string
	if err := db.QueryRow(`SELECT external_analysis_evidence_json FROM documents WHERE path='target.md'`).Scan(&originalExternalEvidence); err != nil {
		t.Fatal(err)
	}
	if originalExternalEvidence == "" {
		t.Fatal("external lane did not store its own evidence")
	}

	// Frontmatter edit, then a local run: the local lane's seen stamps move
	// to the new frontmatter while the external lane's stay behind.
	if err := os.WriteFile(target, []byte("---\ntitle: New\n---\n# New\n\nBody.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	localModel := &fakeLLM{reply: `{"title":"Note","summary":"Summary","tags":["x"],"obsolete":false,"suggestions":[]}`}
	if _, err := RunProgress(t.Context(), c, db, Generators{Analysis: localModel}, RunOptions{}); err != nil {
		t.Fatalf("local run error=%v", err)
	}
	var localEvidence, externalEvidence, localSeenFm, externalSeenFm string
	if err := db.QueryRow(`SELECT analysis_evidence_json,external_analysis_evidence_json,seen_fm_sha256,seen_external_fm_sha256
FROM documents WHERE path='target.md'`).Scan(&localEvidence, &externalEvidence, &localSeenFm, &externalSeenFm); err != nil {
		t.Fatal(err)
	}
	if localEvidence == "" || externalEvidence == "" || externalEvidence != originalExternalEvidence {
		t.Fatalf("local analysis overwrote external evidence: local=%q external=%q original_external=%q", localEvidence, externalEvidence, originalExternalEvidence)
	}
	if localSeenFm == externalSeenFm {
		t.Fatalf("expected per-lane frontmatter stamps to diverge after local-only update; both=%q", localSeenFm)
	}

	// A second local run has nothing left: no pending and no fm drift vs
	// seen_fm. The external lane still sees the same edit as fm drift.
	localModel2 := &fakeLLM{reply: localModel.reply}
	second, err := RunProgress(t.Context(), c, db, Generators{Analysis: localModel2}, RunOptions{})
	if err != nil || second.Processed != 0 {
		t.Fatalf("local lane should be fully current, got %+v error=%v", second, err)
	}
	externalModel2 := &fakeLLM{reply: localModel.reply}
	third, err := RunProgress(t.Context(), c, db, Generators{Analysis: externalModel2}, RunOptions{External: true})
	if err != nil {
		t.Fatalf("external run error=%v", err)
	}
	linkerPrompts := 0
	for _, prompt := range externalModel2.prompts {
		if strings.Contains(prompt, "Current path: linker.md") {
			linkerPrompts++
		}
	}
	if linkerPrompts != 1 {
		t.Fatalf("external lane should refresh the linker on its own fm drift, got %d; run=%+v", linkerPrompts, third)
	}
}

func TestMarshalAnalysisEvidenceRecordsOnlyInjected(t *testing.T) {
	ranked := []candidate{
		{doc: database.Document{Path: "ranked.md"}},
		{doc: database.Document{Path: "mentioned.md"}, mentions: []entityMention{{path: "mentioned.md"}}},
		{doc: database.Document{Path: "scored.md"}, sim: 0.2, rel: 0.7},
	}
	raw := marshalAnalysisEvidence(ranked, []string{"inbound.md"}, []string{"sib.md"},
		map[string]string{"merge.md": "body"}, []string{"entity-prose.md"},
		"Frontmatter validation context")
	fmSet, fullSet, ok := parseAnalysisEvidence(raw)
	if !ok {
		t.Fatal("evidence did not round-trip")
	}
	for _, path := range []string{"ranked.md", "mentioned.md", "scored.md"} {
		if !fmSet[path] {
			t.Fatalf("fm evidence missing %s: %s", path, raw)
		}
	}
	if fmSet["inbound.md"] || fmSet["sib.md"] {
		t.Fatalf("path-only context was conflated with annotation evidence: %s", raw)
	}
	for _, path := range []string{"merge.md", "entity-prose.md", "scored.md"} {
		if !fullSet[path] {
			t.Fatalf("full evidence missing %s: %s", path, raw)
		}
	}
	// A mentioned-but-unrendered candidate is annotation-level only — the
	// render cap decides what actually entered the prompt.
	if fullSet["mentioned.md"] {
		t.Fatalf("unrendered entity mention must not record body evidence: %s", raw)
	}
	ev, ok := parseAnalysisEvidenceRecord(raw)
	if !ok || ev.Version != 2 || ev.ValidationContext != "Frontmatter validation context" ||
		!equalStrings(ev.Inbound, []string{"inbound.md"}) || !equalStrings(ev.Siblings, []string{"sib.md"}) {
		t.Fatalf("validation context/version not recorded: %+v", ev)
	}
}

func TestSiblingEvidenceContainsOnlyRenderedNames(t *testing.T) {
	context, paths := siblingContext([]string{"helper.rb", "used.rb", "config.yml"}, "This note uses used.rb.\n")
	if !strings.Contains(context, "helper.rb") || !strings.Contains(context, "config.yml") || strings.Contains(context, "used.rb") {
		t.Fatalf("unexpected sibling context: %q", context)
	}
	if len(paths) != 2 || paths[0] != "helper.rb" || paths[1] != "config.yml" {
		t.Fatalf("sibling evidence=%v, want only rendered names", paths)
	}
}

func TestResultSummarySeparatesDirectAndDependentQueue(t *testing.T) {
	r := Result{Processed: 6, DirectPending: 3, Dependents: 3, Failed: 1}
	got := r.Summary("analysis-model", "fast-model")
	for _, part := range []string{
		"Analyzed 6",
		"queue 3 direct + 3 evidence dependents",
		"failed 1",
	} {
		if !strings.Contains(got, part) {
			t.Fatalf("summary %q missing %q", got, part)
		}
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

func TestLegacyEvidenceRecoversNewInboundLinkAndRestamps(t *testing.T) {
	c, db := setup(t)
	writeDoc(t, c.RepositoryPath, "target.md", "# Target\n\nNotes on harbor seal colonies.\n")
	writeDoc(t, c.RepositoryPath, "linker.md", "# Linker\n\nAside: [Target](target.md).\nFermentation crocks and brine ratios.\n")
	writeDoc(t, c.RepositoryPath, "steady.md", "# Steady\n\nQuiet note about moss gardens.\n")
	writeDoc(t, c.RepositoryPath, "peer.md", "# Peer\n\nAside: [Steady](steady.md).\nCold-frame sowing calendar.\n")
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	firstModel := &fakeLLM{reply: `{"title":"Note","summary":"Summary","tags":["x"],"obsolete":false,"suggestions":[]}`}
	if _, err := RunProgress(t.Context(), c, db, Generators{Analysis: firstModel}, RunOptions{}); err != nil {
		t.Fatalf("initial analysis error=%v", err)
	}
	// Downgrade to the pre-versioned evidence format: candidates and inbound
	// linkers were flattened into one set with no inbound/sibling/context
	// keys. The sweep must recover newly added inbound paths conservatively
	// without treating every flattened path as body evidence.
	legacy := `{"fm":["a.md","linker.md"],"full":["linker.md"]}`
	if _, err := db.Exec(`UPDATE documents SET analysis_evidence_json=? WHERE path='target.md'`, legacy); err != nil {
		t.Fatal(err)
	}
	steadyLegacy := `{"fm":["a.md","peer.md"],"full":["peer.md"]}`
	if _, err := db.Exec(`UPDATE documents SET analysis_evidence_json=? WHERE path='steady.md'`, steadyLegacy); err != nil {
		t.Fatal(err)
	}
	writeDoc(t, c.RepositoryPath, "rivet-log.md", "---\nrelated: [target.md]\n---\n# Rivet log\n\nCopper kettle upkeep with rivets and solder flux.\n")
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	secondModel := &fakeLLM{reply: firstModel.reply}
	second, err := RunProgress(t.Context(), c, db, Generators{Analysis: secondModel}, RunOptions{})
	if err != nil {
		t.Fatalf("run error=%v", err)
	}
	processed := map[string]bool{}
	for _, prompt := range secondModel.prompts {
		for _, line := range strings.Split(prompt, "\n") {
			if strings.HasPrefix(line, "Current path: ") {
				processed[strings.TrimPrefix(line, "Current path: ")] = true
			}
		}
	}
	// The new document is direct work; target.md is an evidence dependent
	// because its inbound set gained a path absent from the flattened record.
	// steady.md's inbound set is fully recorded, so it stays quiet.
	if second.Processed != 2 || second.Dependents != 1 {
		t.Fatalf("expected new doc + one legacy-evidence dependent, got %+v paths=%v", second, processed)
	}
	if !processed["target.md"] || !processed["rivet-log.md"] {
		t.Fatalf("expected rivet-log and target processed, got %v", processed)
	}
	if processed["steady.md"] || processed["linker.md"] || processed["peer.md"] || processed["a.md"] {
		t.Fatalf("unchanged documents re-analyzed on a bounded inbound change: %v", processed)
	}
	// The conversion: a re-analyzed legacy row is restamped with the exact
	// v2 record — its real inbound set, not the flattened approximation.
	var raw string
	if err := db.QueryRow(`SELECT analysis_evidence_json FROM documents WHERE path='target.md'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	ev, ok := parseAnalysisEvidenceRecord(raw)
	if !ok || ev.Version < 2 {
		t.Fatalf("legacy evidence was not upgraded on re-analysis: %q", raw)
	}
	if !slices.Contains(ev.Inbound, "linker.md") || !slices.Contains(ev.Inbound, "rivet-log.md") {
		t.Fatalf("converted evidence lost the inbound set: %+v", ev)
	}
	var steadyRaw string
	if err := db.QueryRow(`SELECT analysis_evidence_json FROM documents WHERE path='steady.md'`).Scan(&steadyRaw); err != nil {
		t.Fatal(err)
	}
	if steadyRaw != steadyLegacy {
		t.Fatalf("unaffected legacy row was rewritten: %q", steadyRaw)
	}
	// The converted row is now exact — a later run with no new input must not
	// sweep it back into the queue.
	thirdModel := &fakeLLM{reply: firstModel.reply}
	third, err := RunProgress(t.Context(), c, db, Generators{Analysis: thirdModel}, RunOptions{})
	if err != nil || third.Processed != 0 || third.Dependents != 0 {
		t.Fatalf("converted evidence should be stable: %+v error=%v", third, err)
	}
}

func TestLegacyEvidenceRemovalStillRefreshesConsumer(t *testing.T) {
	c, db := setup(t)
	writeDoc(t, c.RepositoryPath, "consumer.md", "# Consumer\n\nNotes on tidal gauges.\n")
	writeDoc(t, c.RepositoryPath, "gone.md", "# Gone\n\nBrass fitting catalogue excerpts.\n")
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	firstModel := &fakeLLM{reply: `{"title":"Note","summary":"Summary","tags":["x"],"obsolete":false,"suggestions":[]}`}
	if _, err := RunProgress(t.Context(), c, db, Generators{Analysis: firstModel}, RunOptions{}); err != nil {
		t.Fatalf("initial analysis error=%v", err)
	}
	// A consumer stamped by the pre-versioned recorder: gone.md is part of its
	// flattened evidence, so deleting gone.md must still refresh it.
	legacy := `{"fm":["a.md","gone.md"],"full":["gone.md"]}`
	if _, err := db.Exec(`UPDATE documents SET analysis_evidence_json=? WHERE path='consumer.md'`, legacy); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(c.RepositoryPath, "gone.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	secondModel := &fakeLLM{reply: firstModel.reply}
	second, err := RunProgress(t.Context(), c, db, Generators{Analysis: secondModel}, RunOptions{})
	if err != nil {
		t.Fatalf("run error=%v", err)
	}
	// consumer.md (legacy record) and a.md (exact record) both consumed
	// gone.md's annotation; deletion refreshes both.
	if second.Processed != 2 || second.Dependents != 2 {
		t.Fatalf("deletion should refresh legacy and exact consumers, got %+v", second)
	}
	var raw string
	if err := db.QueryRow(`SELECT analysis_evidence_json FROM documents WHERE path='consumer.md'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if ev, ok := parseAnalysisEvidenceRecord(raw); !ok || ev.Version < 2 {
		t.Fatalf("legacy consumer was not restamped after removal refresh: %q", raw)
	}
}

func TestUnrecordedEvidenceFallsBackToHeuristicSweep(t *testing.T) {
	c, db := setup(t)
	writeDoc(t, c.RepositoryPath, "zebra.md",
		"---\ntitle: Zebra Protocol\n---\n# Zebra Protocol\n\nDefines packet rituals.\n")
	writeDoc(t, c.RepositoryPath, "mentioner.md",
		"# Mentioner\n\nThis note extends the Zebra Protocol work for field kits.\n")
	writeDoc(t, c.RepositoryPath, "sig-source.md",
		"# Sig source\n\nharbor kettle velvet orbit mango cinder brine lattice ember fjord.\n")
	writeDoc(t, c.RepositoryPath, "sig-echo.md",
		"# Sig echo\n\nharbor kettle velvet orbit mango cinder brine lattice ember fjord.\n")
	writeDoc(t, c.RepositoryPath, "plain.md", "# Plain\n\nThrown porcelain glaze recipes.\n")
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	firstModel := &fakeLLM{reply: `{"title":"Note","summary":"Summary","tags":["x"],"obsolete":false,"suggestions":[]}`}
	if _, err := RunProgress(t.Context(), c, db, Generators{Analysis: firstModel}, RunOptions{}); err != nil {
		t.Fatalf("initial analysis error=%v", err)
	}
	// Rows predating the evidence column have no recording at all: the sweep
	// must fall back to the conservative heuristic arms until they restamp.
	if _, err := db.Exec(`UPDATE documents SET analysis_evidence_json='' WHERE path IN ('mentioner.md','sig-echo.md','plain.md')`); err != nil {
		t.Fatal(err)
	}
	writeDoc(t, c.RepositoryPath, "zebra.md",
		"---\ntitle: Zebra Protocol\nsummary: Packet ritual reference\n---\n# Zebra Protocol\n\nDefines packet rituals.\n")
	writeDoc(t, c.RepositoryPath, "sig-source.md",
		"# Sig source\n\nharbor kettle velvet orbit mango cinder brine lattice ember tundra.\n")
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	secondModel := &fakeLLM{reply: firstModel.reply}
	second, err := RunProgress(t.Context(), c, db, Generators{Analysis: secondModel}, RunOptions{})
	if err != nil {
		t.Fatalf("run error=%v", err)
	}
	processed := map[string]bool{}
	for _, prompt := range secondModel.prompts {
		for _, line := range strings.Split(prompt, "\n") {
			if strings.HasPrefix(line, "Current path: ") {
				processed[strings.TrimPrefix(line, "Current path: ")] = true
			}
		}
	}
	// Without a recording, the heuristic arms stand in: the entity mention of
	// zebra.md and the signature overlap with sig-source.md each refresh their
	// unrecorded consumer. plain.md has no evidence connection and stays out.
	if !processed["mentioner.md"] || !processed["sig-echo.md"] {
		t.Fatalf("unrecorded consumers were not refreshed by heuristic arms: %v", processed)
	}
	if processed["plain.md"] {
		t.Fatalf("unrelated unrecorded document re-analyzed: %v", processed)
	}
	if second.Dependents < 2 {
		t.Fatalf("expected at least the two heuristic dependents, got %+v", second)
	}
}

// A new file in a directory is not, by itself, evidence for its neighbors:
// it only enters prompts as a candidate, so relevance decides who refreshes.
// Unrelated additions must not fan out (the old sibling-set equality queued
// every stamped neighbor); an addition the document actually mentions is
// semantically relevant and refreshes exactly that consumer.
func TestNewSiblingRequiresSemanticRelevance(t *testing.T) {
	c, db := setup(t)
	writeFile := func(name, content string) {
		if err := os.WriteFile(filepath.Join(c.RepositoryPath, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("alpha.md", "# Alpha\n\nWe use zebra widgets daily.\n")
	writeFile("beta.md", "# Beta\n\nOrdinary notes about gardens.\n")
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	model := &fakeLLM{reply: `{"title":"Note","summary":"Summary","tags":["x"],"obsolete":false,"suggestions":[]}`}
	if _, err := RunProgress(t.Context(), c, db, Generators{Analysis: model}, RunOptions{}); err != nil {
		t.Fatal(err)
	}

	writeFile("quasar-topology.md", "# Quasar Topology\n\nLattice field notes.\n")
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	est, err := EstimateAnalysis(t.Context(), c, db, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if est.Pending != 1 || est.Dependents != 0 {
		t.Fatalf("unrelated sibling should queue only itself: %+v", est)
	}
	if _, err := RunProgress(t.Context(), c, db, Generators{Analysis: model}, RunOptions{}); err != nil {
		t.Fatal(err)
	}

	writeFile("zebra-widgets.md", "# Zebra Widgets\n\nRegistry of widget shapes and colors.\n")
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	est, err = EstimateAnalysis(t.Context(), c, db, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if est.Pending != 1 || est.Dependents != 1 {
		t.Fatalf("relevant new file should refresh only its consumer: %+v", est)
	}
}

// Dependent queue attribution: each swept doc emits a 'dependent' event
// naming the firing arm and the triggering document, so "why was this
// re-analyzed" is visible in job progress without inspecting the database.
func TestDependentReasonNamesArmAndTrigger(t *testing.T) {
	c, db := setup(t)
	linker := filepath.Join(c.RepositoryPath, "linker.md")
	target := filepath.Join(c.RepositoryPath, "target.md")
	if err := os.WriteFile(linker, []byte("# Linker\n\nSee [Target](target.md).\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("# Target\n\nBody.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	model := &fakeLLM{reply: `{"title":"Note","summary":"Summary","tags":["x"],"obsolete":false,"suggestions":[]}`}
	if _, err := RunProgress(t.Context(), c, db, Generators{Analysis: model}, RunOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	var events []Progress
	if _, err := RunProgress(t.Context(), c, db, Generators{Analysis: model}, RunOptions{
		Progress: func(pr Progress) { events = append(events, pr) },
	}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, pr := range events {
		if pr.Outcome == "dependent" && pr.Path == "linker.md" {
			found = true
			if !strings.Contains(pr.Detail, "target.md") || pr.Detail == "" {
				t.Fatalf("dependent reason must name the trigger: %q", pr.Detail)
			}
			t.Logf("linker.md dependent because: %s", pr.Detail)
		}
	}
	if !found {
		t.Fatalf("linker.md should emit a dependent event with its trigger, got %+v", events)
	}
}

// A recorded sibling that disappears is provable context loss — the prompt
// rendered it as an unmentioned neighbor. The dependent reason must name it.
// Uses a non-document sibling (.rb) so no document-lifecycle arm can fire.
func TestRemovedSiblingRefreshesConsumerWithReason(t *testing.T) {
	c, db := setup(t)
	note := filepath.Join(c.RepositoryPath, "note.md")
	sibling := filepath.Join(c.RepositoryPath, "helper.rb")
	trigger := filepath.Join(c.RepositoryPath, "trigger.md")
	if err := os.WriteFile(note, []byte("# Note\n\nPlain content.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sibling, []byte("puts 'snippet'\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trigger, []byte("# Trigger\n\nV1.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	model := &fakeLLM{reply: `{"title":"Note","summary":"Summary","tags":["x"],"obsolete":false,"suggestions":[]}`}
	if _, err := RunProgress(t.Context(), c, db, Generators{Analysis: model}, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	// A non-document sibling's deletion produces no document seed — the sweep
	// runs because trigger.md changed; note.md fires on the missing sibling.
	if err := os.Remove(sibling); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trigger, []byte("# Trigger\n\nV2 body changed.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	var events []Progress
	if _, err := RunProgress(t.Context(), c, db, Generators{Analysis: model}, RunOptions{
		Progress: func(pr Progress) { events = append(events, pr) },
	}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, pr := range events {
		if pr.Outcome == "dependent" && pr.Path == "note.md" {
			found = true
			if pr.Detail != "sibling removed: helper.rb" {
				t.Fatalf("expected sibling-removal reason, got %q", pr.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("note.md should queue on sibling removal, got %+v", events)
	}
}

// Validation context is prompt-visible evidence: fixing a broken link the
// prompt rendered as an issue changes the context and refreshes the doc —
// with the reason naming the arm.
func TestResolvedLinkIssueRefreshesViaValidationContext(t *testing.T) {
	c, db := setup(t)
	note := filepath.Join(c.RepositoryPath, "note.md")
	gone := filepath.Join(c.RepositoryPath, "gone.md")
	if err := os.WriteFile(note, []byte("# Note\n\nSee [Gone](gone.md).\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	model := &fakeLLM{reply: `{"title":"Note","summary":"Summary","tags":["x"],"obsolete":false,"suggestions":[]}`}
	if _, err := RunProgress(t.Context(), c, db, Generators{Analysis: model}, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	// The missing link target appears — the rendered validation context the
	// note's prompt consumed is now different.
	if err := os.WriteFile(gone, []byte("# Gone\n\nNow exists.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	var events []Progress
	if _, err := RunProgress(t.Context(), c, db, Generators{Analysis: model}, RunOptions{
		Progress: func(pr Progress) { events = append(events, pr) },
	}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, pr := range events {
		if pr.Outcome == "dependent" && pr.Path == "note.md" {
			found = true
			if pr.Detail != "validation context changed" {
				t.Fatalf("expected validation-context reason, got %q", pr.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("note.md should queue on validation-context change, got %+v", events)
	}
}

// RestampEvidence migrates rows analyzed before evidence recording:
// deterministic context is reconstructed into a v2 record and the removal
// generation is acknowledged — no model calls needed.
func TestRestampEvidenceRebuildsV2WithoutModel(t *testing.T) {
	c, db := setup(t)
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "note.md"),
		[]byte("# Note\n\nLine one.\n\nLine two.\n\nLine three.\n\nLine four.\n\nLine five.\n\nLine six.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	model := &fakeLLM{reply: `{"title":"N","summary":"s","tags":["x"],"obsolete":false,"suggestions":[]}`}
	if _, err := RunProgress(t.Context(), c, db, Generators{Analysis: model}, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-v17 row: drop the evidence record and the removal ack.
	if _, err := db.ExecContext(t.Context(),
		`UPDATE documents SET analysis_evidence_json='', seen_removal_generation=0 WHERE path='note.md'`); err != nil {
		t.Fatal(err)
	}
	n, err := RestampEvidence(t.Context(), c, db, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatalf("expected at least one restamped doc, got %d", n)
	}
	var ev string
	var gen int64
	if err := db.QueryRowContext(t.Context(),
		`SELECT analysis_evidence_json, seen_removal_generation FROM documents WHERE path='note.md'`).Scan(&ev, &gen); err != nil {
		t.Fatal(err)
	}
	if parsed, ok := parseAnalysisEvidenceRecord(ev); !ok || parsed.Version < 2 {
		t.Fatalf("expected a v2 evidence record, got %q", ev)
	}
	current, err := db.RemovalGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if gen != current {
		t.Fatalf("seen_removal_generation=%d must acknowledge current %d", gen, current)
	}
}
