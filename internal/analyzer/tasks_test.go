package analyzer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/scanner"
)

// taskSetup builds the shared fixture plus the task prompt files expand and
// freshness need — ComposeTask fails closed when a task prompt is missing.
func taskSetup(t *testing.T) (config.Config, *database.DB) {
	t.Helper()
	c, db := setup(t)
	for _, name := range []string{"expand.md", "freshness.md"} {
		if err := os.WriteFile(filepath.Join(c.PromptDirectory, name), []byte(strings.ToUpper(name)), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return c, db
}

func writeDoc(t *testing.T, repo, rel, content string) {
	t.Helper()
	full := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// docByPath finds one scanned document.
func docByPath(t *testing.T, db *database.DB, path string) database.Document {
	t.Helper()
	docs, err := db.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		if d.Path == path {
			return d
		}
	}
	t.Fatalf("document %s not found", path)
	return database.Document{}
}

func TestExpandProducesReviewableSuggestion(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	writeDoc(t, c.RepositoryPath, "languages/STUB.md", "# <name>\n\n## What it is\n\n## Pitfalls\n")
	writeDoc(t, c.RepositoryPath, "languages/go-stub.md",
		"---\ntags: [stub, go]\n---\n# Go\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: `{"title":"Go","summary":"Go systems language.","tags":["go","programming-language"],
		"reason":"Filled the stub following the languages template.","confidence":0.8,
		"proposed_content":"---\ntags: [go, programming-language]\n---\n# Go\n\n## What it is\nGo.\n\n## Pitfalls\nNone.\n"}`}
	r, err := RunTask(ctx, c, db, Generators{Analysis: llm}, config.TaskExpand, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Processed != 1 || r.Suggestions != 1 {
		t.Fatalf("expected one expansion suggestion: %+v", r)
	}
	sugs, err := db.Suggestions(ctx, "pending")
	if err != nil || len(sugs) != 1 {
		t.Fatalf("pending: %v %v", sugs, err)
	}
	if sugs[0].Action != "expand" || sugs[0].Origin != "expand" || sugs[0].ProposedContent == "" {
		t.Fatalf("unexpected suggestion: %+v", sugs[0])
	}
	doc := docByPath(t, db, "languages/go-stub.md")
	if doc.ExpandSHA256 != doc.SHA256 || doc.ExpandModel == "" {
		t.Fatalf("expand lineage not stamped: %+v", doc)
	}
	if !strings.Contains(llm.prompt, "What it is") {
		t.Fatalf("prompt should carry the template skeleton: %s", llm.prompt)
	}
}

func TestExpandRefusesWithoutTemplate(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	writeDoc(t, c.RepositoryPath, "tools/stub.md", "---\ntags: [stub]\n---\n# T\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: `{}`}
	r, err := RunTask(ctx, c, db, Generators{Analysis: llm}, config.TaskExpand, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.NoTemplate != 1 || llm.prompt != "" {
		t.Fatalf("template-less stub must not reach the model: %+v prompt=%q", r, llm.prompt)
	}
}

func TestExpandUsesNearestAncestorTemplate(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	writeDoc(t, c.RepositoryPath, "STUB.md", "ROOT-TEMPLATE")
	writeDoc(t, c.RepositoryPath, "databases/STUB.md", "DB-TEMPLATE-SECTIONS")
	writeDoc(t, c.RepositoryPath, "databases/pg.md", "---\ntags: [stub]\n---\n# PG\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: `{"title":"PG","summary":"s","tags":["pg"],"reason":"r","confidence":0.5,
		"proposed_content":"---\ntags: [pg]\n---\n# PG\n\nExpanded.\n"}`}
	if _, err := RunTask(ctx, c, db, Generators{Analysis: llm}, config.TaskExpand, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(llm.prompt, "DB-TEMPLATE-SECTIONS") || strings.Contains(llm.prompt, "ROOT-TEMPLATE") {
		t.Fatalf("nearest ancestor template should win: %s", llm.prompt)
	}
}

func TestExpandSkipsPrivateAndNonStub(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	writeDoc(t, c.RepositoryPath, "STUB.md", "TEMPLATE")
	writeDoc(t, c.RepositoryPath, "private.md", "---\ntags: [stub]\nkb_policy: {no_llm_context: true}\n---\n# Secret\n")
	writeDoc(t, c.RepositoryPath, "grown.md", "---\ntags: [go]\n---\n# Grown\n\nThis document has real content already.\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: `{}`}
	r, err := RunTask(ctx, c, db, Generators{Analysis: llm}, config.TaskExpand, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Skipped != 3 || llm.prompt != "" {
		t.Fatalf("private and non-stub docs must be skipped without an LLM call: %+v", r)
	}
}

func TestFreshnessWritesManagedBlockWithoutHashChurn(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	writeDoc(t, c.RepositoryPath, "tools/curl.md",
		"---\ntags: [tool, curl]\n---\n# curl\n\nThe latest curl release is 8.4.0 and supports HTTP/3.\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	before := docByPath(t, db, "tools/curl.md")
	llm := &fakeLLM{reply: `{"current":false,"summary":"Version pin may be stale.",
		"flags":[{"claim":"latest curl release is 8.4.0","concern":"release claim, not verified"}],
		"suggestions":[]}`}
	r, err := RunTask(ctx, c, db, Generators{Analysis: llm}, config.TaskFreshness, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// a.md from setup is also a non-stub and is processed alongside.
	if r.Processed != 2 {
		t.Fatalf("expected two freshness checks: %+v", r)
	}
	raw, err := os.ReadFile(filepath.Join(c.RepositoryPath, "tools/curl.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "freshness") || !strings.Contains(string(raw), "release claim, not verified") {
		t.Fatalf("kbm.freshness block not written:\n%s", raw)
	}
	// The managed block must not invalidate the content hash or requeue analysis.
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	after := docByPath(t, db, "tools/curl.md")
	if after.SHA256 != before.SHA256 || after.FmSHA256 != before.FmSHA256 || after.BodySHA256 != before.BodySHA256 {
		t.Fatalf("managed freshness write changed the content hash")
	}
	if after.FreshnessSHA256 != after.BodySHA256 {
		t.Fatalf("freshness lineage should stamp the body hash: %+v", after)
	}
	pending, err := db.Pending(ctx, c.OllamaModel, c.FastModel(), false)
	if err != nil {
		t.Fatal(err)
	}
	// Neither doc was ever analyzed, so both remain pending — the kbm write
	// must not add to or change that set.
	if len(pending) != 2 {
		t.Fatalf("kbm write must not change the pending set: %+v", pending)
	}
}

func TestFreshnessSkipsStubsAndPrivate(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	writeDoc(t, c.RepositoryPath, "stub.md", "---\ntags: [stub]\n---\n# S\n")
	writeDoc(t, c.RepositoryPath, "private.md",
		"---\ntags: [tool]\nkb_policy: {no_llm_context: true}\n---\n# P\n\nA private note with real content.\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	// a.md (the setup fixture) is the only LLM-processed document; stub.md and
	// private.md must be skipped before any model call.
	llm := &fakeLLM{reply: `{"current":true,"summary":"ok","flags":[],"suggestions":[]}`}
	r, err := RunTask(ctx, c, db, Generators{Analysis: llm}, config.TaskFreshness, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Skipped != 2 || r.Processed != 1 {
		t.Fatalf("stubs and private docs must be skipped: %+v", r)
	}
	if strings.Contains(llm.prompt, "private.md") || strings.Contains(llm.prompt, "A private note") {
		t.Fatal("private document leaked into a prompt")
	}
}

func TestFreshnessConstrainsAndRejectsNonEmptyTargetPath(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	writeDoc(t, c.RepositoryPath, "tools/curl.md",
		"---\ntags: [tool, curl]\n---\n# curl\n\nA current note with enough body lines.\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: `{"current":false,"summary":"review","flags":[],
		"suggestions":[{"action":"expand","target_path":"other.md","reason":"correction",
		"confidence":0.8,"proposed_content":"replacement"}]}`}
	r, err := RunTask(ctx, c, db, Generators{Analysis: llm}, config.TaskFreshness, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Processed != 2 || r.Suggestions != 0 || r.Rejected != 2 {
		t.Fatalf("non-empty freshness target paths must be rejected: %+v", r)
	}

	var schema struct {
		Properties map[string]struct {
			Items struct {
				Properties map[string]struct {
					Enum []string `json:"enum"`
				} `json:"properties"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(llm.format, &schema); err != nil {
		t.Fatalf("decode freshness schema %s: %v", llm.format, err)
	}
	allowed := schema.Properties["suggestions"].Items.Properties["target_path"].Enum
	if len(allowed) != 1 || allowed[0] != "" {
		t.Fatalf("freshness schema must constrain target_path to the current document: %q", allowed)
	}

	suggestions, err := db.Suggestions(ctx, "pending")
	if err != nil {
		t.Fatal(err)
	}
	if len(suggestions) != 0 {
		t.Fatalf("rejected freshness edits must not enter the review queue: %+v", suggestions)
	}
}

func TestFreshnessMalformedJSONFailsClosedAfterOneRetry(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	if err := os.Remove(filepath.Join(c.RepositoryPath, "a.md")); err != nil {
		t.Fatal(err)
	}
	writeDoc(t, c.RepositoryPath, "tools/curl.md",
		"---\ntags: [tool, curl]\n---\n# curl\n\nA current note with enough body lines.\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	bad := `{"current":false,"summary":"bad escape \C","flags":[],"suggestions":[]}`
	llm := &fakeLLM{queue: []string{bad, bad}}
	r, err := RunTask(ctx, c, db, Generators{Analysis: llm}, config.TaskFreshness, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Failed != 1 || r.Processed != 0 || len(llm.prompts) != 2 {
		t.Fatalf("malformed freshness JSON should fail after one retry: result=%+v calls=%d", r, len(llm.prompts))
	}
	if !strings.Contains(llm.prompts[1], "failed JSON parsing") {
		t.Fatalf("retry prompt should identify the parse error: %s", llm.prompts[1])
	}
	doc := docByPath(t, db, "tools/curl.md")
	if doc.FreshnessSHA256 != "" || !strings.Contains(doc.LastError, "invalid JSON") {
		t.Fatalf("malformed output must not stamp freshness or lose its error: %+v", doc)
	}
	raw, err := os.ReadFile(filepath.Join(c.RepositoryPath, "tools/curl.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "kbm:") {
		t.Fatalf("failed JSON must not write managed freshness metadata:\n%s", raw)
	}
}

func TestRunEmbedStampsLineage(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	writeDoc(t, c.RepositoryPath, "docs.md", "---\ntags: [tool]\n---\n# Docs\n\nEmbeddable content.\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	c.OllamaEmbedModel = "test-model"
	embedder := &fakeEmbedder{}
	r, err := RunEmbed(ctx, c, db, Generators{Embed: embedder}, RunOptions{Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	// setup's a.md plus docs.md are embeddable.
	if r.Processed != 2 || len(embedder.texts) != 2 {
		t.Fatalf("expected 2 embeddings: %+v calls=%d", r, len(embedder.texts))
	}
	pending, err := db.PendingTask(ctx, config.TaskEmbed, "test-model", false)
	if err != nil || len(pending) != 0 {
		t.Fatalf("embed lineage should clear pending: %+v err=%v", pending, err)
	}
}

func TestSplitHashesDistinguishFrontmatterAndBodyEdits(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	writeDoc(t, c.RepositoryPath, "doc.md", "---\ntags: [tool]\n---\n# Doc\n\nBody words here stay fixed.\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	first := docByPath(t, db, "doc.md")
	if first.FmSHA256 == "" || first.BodySHA256 == "" || first.FmSHA256 == first.BodySHA256 {
		t.Fatalf("split hashes should be populated and distinct: %+v", first)
	}
	// Frontmatter-only edit: fm hash moves, body hash holds.
	writeDoc(t, c.RepositoryPath, "doc.md", "---\ntags: [tool, extra]\n---\n# Doc\n\nBody words here stay fixed.\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	second := docByPath(t, db, "doc.md")
	if second.FmSHA256 == first.FmSHA256 {
		t.Fatal("frontmatter edit must change fm_sha256")
	}
	if second.BodySHA256 != first.BodySHA256 {
		t.Fatal("frontmatter edit must not change body_sha256")
	}
	// Body-only edit: body hash moves, fm hash holds.
	writeDoc(t, c.RepositoryPath, "doc.md", "---\ntags: [tool, extra]\n---\n# Doc\n\nBody words changed now.\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	third := docByPath(t, db, "doc.md")
	if third.BodySHA256 == second.BodySHA256 || third.FmSHA256 != second.FmSHA256 {
		t.Fatal("body edit must change only body_sha256")
	}
}

func TestManagedKBMWriteLeavesSplitHashesUntouched(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	writeDoc(t, c.RepositoryPath, "doc.md", "---\ntags: [tool]\n---\n# Doc\n\nBody.\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	before := docByPath(t, db, "doc.md")
	root, err := os.OpenRoot(c.RepositoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := frontmatter.WriteManaged(root, "doc.md", "link_check", map[string]any{"at": "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	after := docByPath(t, db, "doc.md")
	if after.SHA256 != before.SHA256 || after.FmSHA256 != before.FmSHA256 || after.BodySHA256 != before.BodySHA256 {
		t.Fatal("kbm-only write must leave canonical, fm and body hashes identical")
	}
	var fm map[string]any
	if err := json.Unmarshal([]byte(after.FrontmatterJSON), &fm); err != nil {
		t.Fatal(err)
	}
}

func TestTaskRetriesOnceOnInvalidJSON(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	writeDoc(t, c.RepositoryPath, "STUB.md", "TEMPLATE")
	// Named like a real entity stub — "stub.md" collides with "STUB.md" on
	// case-insensitive filesystems.
	writeDoc(t, c.RepositoryPath, "entity.md", "---\ntags: [stub]\n---\n# S\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{queue: []string{
		`{"title": "S", bad json`,
		`{"title":"S","summary":"s","tags":["t"],"reason":"r","confidence":0.5,
			"proposed_content":"---\ntags: [t]\n---\n# S\n\nExpanded text.\n"}`,
	}}
	r, err := RunTask(ctx, c, db, Generators{Analysis: llm}, config.TaskExpand, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Processed != 1 || len(llm.prompts) != 2 {
		t.Fatalf("one retry should produce the expansion: %+v calls=%d", r, len(llm.prompts))
	}
	if !strings.Contains(llm.prompts[1], "failed JSON parsing") {
		t.Fatal("the retry prompt should name the parse failure")
	}
}

func TestTaskFailRecordsErrorForVanishedDocument(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	writeDoc(t, c.RepositoryPath, "STUB.md", "TEMPLATE")
	writeDoc(t, c.RepositoryPath, "ghost-stub.md", "---\ntags: [stub]\n---\n# Ghost\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	// Delete after the scan — the pending row outlives the file.
	if err := os.Remove(filepath.Join(c.RepositoryPath, "ghost-stub.md")); err != nil {
		t.Fatal(err)
	}
	r, err := RunTask(ctx, c, db, Generators{Analysis: &fakeLLM{}}, config.TaskExpand, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Failed != 1 {
		t.Fatalf("a vanished document should count as a task failure: %+v", r)
	}
	doc := docByPath(t, db, "ghost-stub.md")
	if doc.LastError == "" {
		t.Fatal("task failure should be recorded on the document row")
	}
}

func TestTaskResultSummaryContent(t *testing.T) {
	r := TaskResult{Processed: 3, Skipped: 2, NoTemplate: 1, Failed: 1, Rejected: 4, Suggestions: 5}
	got := r.Summary("expand", "m1")
	for _, part := range []string{"expand", "3 processed", "2 skipped", "1 without template", "1 failed", "5 suggestion(s)", "4 rejected", "m1"} {
		if !strings.Contains(got, part) {
			t.Fatalf("summary %q missing %q", got, part)
		}
	}
}

func TestEnrichOnlyStampsEnrichLaneWithoutAnalysis(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	writeDoc(t, c.RepositoryPath, "doc.md", "---\ntags: [tool]\n---\n# Doc\n\nReal content here.\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: `{"title":"Doc","summary":"s","tags":["tool"],"patch":null}`}
	r, err := RunProgress(ctx, c, db, Generators{Analysis: llm, Enrich: llm}, RunOptions{EnrichOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Enriched == 0 {
		t.Fatalf("expected enrichment: %+v", r)
	}
	doc := docByPath(t, db, "doc.md")
	if doc.EnrichedSHA256 != doc.SHA256 {
		t.Fatalf("enrich stamp missing: %+v", doc)
	}
	if doc.AnalyzedSHA256 != "" {
		t.Fatal("enrich-only run must not stamp the analysis lane")
	}
	// And the doc is still analysis-pending — the two lanes are independent.
	pending, err := db.Pending(ctx, c.OllamaModel, c.FastModel(), false)
	if err != nil || len(pending) == 0 {
		t.Fatalf("analysis should still see the doc as pending: %+v", pending)
	}
}

func TestReanalyzeReprocessesModelDrift(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	writeDoc(t, c.RepositoryPath, "doc.md", "---\ntags: [tool]\n---\n# Doc\n\nReal content here.\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	reply := `{"title":"Doc","summary":"s","tags":["tool"],"obsolete":false,"suggestions":[]}`
	llm := &fakeLLM{reply: reply}
	if _, err := RunProgress(ctx, c, db, Generators{Analysis: llm, Enrich: llm}, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	calls := len(llm.prompts)
	// Same model, no reanalyze: nothing pending.
	if _, err := RunProgress(ctx, c, db, Generators{Analysis: llm, Enrich: llm}, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(llm.prompts) != calls {
		t.Fatal("unchanged doc should not be reprocessed")
	}
	// A different configured model + --reanalyze: drift puts it back.
	c.OllamaModel = "newer-model"
	if _, err := RunProgress(ctx, c, db, Generators{Analysis: llm, Enrich: llm}, RunOptions{Reanalyze: true}); err != nil {
		t.Fatal(err)
	}
	if len(llm.prompts) == calls {
		t.Fatal("model drift + reanalyze should reprocess the document")
	}
	doc := docByPath(t, db, "doc.md")
	if doc.AnalyzedModel != "newer-model" {
		t.Fatalf("lineage should record the new model: %+v", doc)
	}
}

func TestAddLocalMetadataToProposalMergesMissingFields(t *testing.T) {
	// An expand proposal lacking frontmatter fields the deterministic pass
	// already derived gets them merged in — the proposal keeps its own text.
	content := "---\ntags: [go]\n---\n# Go\n\nBody.\n"
	patch := metadataPatch{Set: map[string]any{"title": "Go", "tags": []string{"go", "programming-language"}}}
	merged, added, err := addLocalMetadataToProposal(content, patch)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) == 0 {
		t.Fatal("expected merged fields")
	}
	fields, body, err := frontmatter.Parse(merged)
	if err != nil {
		t.Fatal(err)
	}
	if fields["title"] != "Go" {
		t.Fatalf("merged proposal should carry the derived title: %v", fields)
	}
	if !strings.Contains(body, "Body.") {
		t.Fatal("body must pass through verbatim")
	}
	// Malformed proposals fail closed rather than silently merging.
	if _, _, err := addLocalMetadataToProposal("---\nbad: [unclosed\n---\nx", patch); err == nil {
		t.Fatal("malformed proposed frontmatter must fail")
	}
}
