package analyzer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/scanner"
	"knowledge-ba46-flow/internal/taxonomy"
)

func TestLocalFirstDerivesClearMetadataWithoutCallingFastModel(t *testing.T) {
	c, db := setup(t)
	writeTestTaxonomy(t, c.RepositoryPath)
	c.OllamaModelFast = "fast"
	c.EnrichmentMode = config.EnrichmentModeLocalFirst
	dir := filepath.Join(c.RepositoryPath, "frameworks", "rails")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "README.md")
	content := "# Ruby on Rails\n\nA web framework for Ruby applications.\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	fast := &fakeLLM{reply: `{"title":"Alpha","summary":"Fast summary","tags":["fixture"]}`}
	full := generatorFunc(func(_ context.Context, prompt string, _ json.RawMessage) (string, error) {
		if strings.Contains(prompt, "Current path: frameworks/rails/README.md\n") {
			return `{"title":"Ruby on Rails","summary":"Full summary","tags":["rails"],"obsolete":false,
				"frontmatter_patch":{"set":{"related":["a.md"]},"remove":[]},"suggestions":[]}`, nil
		}
		return `{"title":"Alpha","summary":"Full summary","tags":["alpha"],"obsolete":false,"suggestions":[]}`, nil
	})
	result, err := RunProgress(t.Context(), c, db, Generators{Enrich: fast, Analysis: full}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Processed != 2 || result.Enriched != 1 || result.LocalEnriched != 1 {
		t.Fatalf("result=%+v", result)
	}
	if len(fast.prompts) != 1 || !strings.Contains(fast.prompts[0], "Current path: a.md\n") {
		t.Fatalf("fast model should only see the ambiguous root document: %v", fast.prompts)
	}
	suggestions, err := db.PendingSuggestions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var local database.Suggestion
	localCount := 0
	for _, suggestion := range suggestions {
		if suggestion.DocumentPath == "frameworks/rails/README.md" {
			local = suggestion
			localCount++
		}
	}
	if local.ID == 0 || local.Origin != "analysis" || localCount != 1 {
		t.Fatalf("local metadata proposal=%+v, suggestions=%+v", local, suggestions)
	}
	fields, body, err := frontmatter.Parse(local.ProposedContent)
	if err != nil || fields["title"] != "Ruby on Rails" || body != "# Ruby on Rails\n\nA web framework for Ruby applications.\n" {
		t.Fatalf("local proposal fields=%+v body=%q error=%v", fields, body, err)
	}
	if got := frontmatter.StringList(fields, "tags"); !slices.Equal(got, []string{"framework", "rails"}) {
		t.Fatalf("local path taxonomy tags=%v", got)
	}
	if got := frontmatter.StringList(fields, "related"); !slices.Equal(got, []string{"a.md"}) {
		t.Fatalf("model relation and local metadata were not combined: %v", got)
	}
	var stageModel, patchOutcome, derived string
	var suggestionsCount int
	err = db.QueryRow(`SELECT model,patch_outcome,derived_fields_json,suggestions FROM stage_results
WHERE document_path='frameworks/rails/README.md' AND stage='enrich'`).Scan(&stageModel, &patchOutcome, &derived, &suggestionsCount)
	if err != nil || stageModel != "deterministic" || patchOutcome != "suggested" || derived != `["title","tags"]` || suggestionsCount != 1 {
		t.Fatalf("local stage model=%q patch=%q derived=%s suggestions=%d error=%v", stageModel, patchOutcome, derived, suggestionsCount, err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || frontmatter.Canonical(string(unchanged)) != frontmatter.Canonical(content) {
		t.Fatalf("local analysis mutated source: %q error=%v", unchanged, err)
	}
}

func TestLocalMetadataSuggestionSurvivesFullAnalysisFailure(t *testing.T) {
	c, db := setup(t)
	writeTestTaxonomy(t, c.RepositoryPath)
	c.OllamaModelFast = "fast"
	c.EnrichmentMode = config.EnrichmentModeLocalFirst
	dir := filepath.Join(c.RepositoryPath, "languages")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ruby.md")
	content := "# Ruby\n\nA programming language.\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	fast := &fakeLLM{err: errors.New("ambiguous document fast failure")}
	result, err := RunProgress(t.Context(), c, db, Generators{
		Enrich:   fast,
		Analysis: &fakeLLM{err: errors.New("analysis failure")},
	}, RunOptions{})
	if err != nil || result.LocalEnriched != 1 || result.Failed != 2 || result.EnrichmentFailed != 1 {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if len(fast.prompts) != 1 || !strings.Contains(fast.prompts[0], "Current path: a.md\n") {
		t.Fatalf("only the ambiguous note should use the fast model: %v", fast.prompts)
	}
	suggestions, err := db.PendingSuggestions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(suggestions) != 1 || suggestions[0].DocumentPath != "languages/ruby.md" ||
		suggestions[0].Origin != "deterministic" {
		t.Fatalf("deterministic proposal lost after model failure: %+v", suggestions)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || frontmatter.Canonical(string(unchanged)) != frontmatter.Canonical(content) {
		t.Fatalf("analysis failure mutated source: %q error=%v", unchanged, err)
	}
}

func TestLocalFirstFillsMissingTitleWithoutOverwritingCuratedTags(t *testing.T) {
	c, db := setup(t)
	writeTestTaxonomy(t, c.RepositoryPath)
	c.OllamaModelFast = "fast"
	c.EnrichmentMode = config.EnrichmentModeLocalFirst
	dir := filepath.Join(c.RepositoryPath, "frameworks", "css")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "uikit.md")
	content := "---\ntags: [framework, css, less, scss]\n---\n\n# UIkit\n\nA modular CSS framework.\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	fast := &fakeLLM{reply: `{"title":"Alpha","summary":"Fast","tags":["fixture"]}`}
	full := generatorFunc(func(_ context.Context, prompt string, _ json.RawMessage) (string, error) {
		if strings.Contains(prompt, "Current path: frameworks/css/uikit.md\n") {
			return `{"title":"UIkit","summary":"Full","tags":["framework","css"],"obsolete":false,"suggestions":[]}`, nil
		}
		return `{"title":"Alpha","summary":"Full","tags":["alpha"],"obsolete":false,"suggestions":[]}`, nil
	})
	result, err := RunProgress(t.Context(), c, db, Generators{Enrich: fast, Analysis: full}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.LocalEnriched != 1 || result.Enriched != 1 || len(fast.prompts) != 1 ||
		!strings.Contains(fast.prompts[0], "Current path: a.md\n") {
		t.Fatalf("result=%+v fast prompts=%v", result, fast.prompts)
	}
	suggestions, err := db.PendingSuggestions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, suggestion := range suggestions {
		if suggestion.DocumentPath != "frameworks/css/uikit.md" {
			continue
		}
		fields, _, err := frontmatter.Parse(suggestion.ProposedContent)
		if err != nil {
			t.Fatal(err)
		}
		if fields["title"] != "UIkit" || !slices.Equal(frontmatter.StringList(fields, "tags"), []string{"framework", "css", "less", "scss"}) {
			t.Fatalf("locally composed patch overwrote curated frontmatter: %+v", fields)
		}
		return
	}
	t.Fatal("no UIkit title proposal found")
}

func TestLocalTaxonomyTagsIgnoreStoredModelTags(t *testing.T) {
	c, _ := setup(t)
	writeTestTaxonomy(t, c.RepositoryPath)
	vocabulary, err := taxonomy.Load(c.RepositoryPath)
	if err != nil {
		t.Fatal(err)
	}
	doc := database.Document{Path: "languages/ruby.md", TagsJSON: `["languages","ruby","invented-model-tag"]`}
	if got := localTaxonomyTags(doc, vocabulary); !slices.Equal(got, []string{"programming-language", "ruby"}) {
		t.Fatalf("local taxonomy tags used stored model output: %v", got)
	}
}

func TestTaxonomyRejectsUnlistedTagsFromModelPatches(t *testing.T) {
	c, _ := setup(t)
	writeTestTaxonomy(t, c.RepositoryPath)
	vocabulary, err := taxonomy.Load(c.RepositoryPath)
	if err != nil {
		t.Fatal(err)
	}
	doc := database.Document{Path: "a.md"}
	patch := metadataPatch{Set: map[string]any{
		"title": "Alpha",
		"tags":  []string{"invented-framework"},
	}}
	suggestion, rejected := patchSuggestion(doc, nil, patch, "# Alpha\n", nil, "analysis", vocabulary)
	if suggestion == nil || !slices.ContainsFunc(rejected, func(reason string) bool {
		return strings.Contains(reason, "invented-framework") && strings.Contains(reason, "taxonomy.yaml")
	}) {
		t.Fatalf("unapproved tag was not rejected independently: suggestion=%+v rejected=%v", suggestion, rejected)
	}
	fields, _, err := frontmatter.Parse(suggestion.ProposedContent)
	if err != nil {
		t.Fatal(err)
	}
	if fields["title"] != "Alpha" || len(frontmatter.StringList(fields, "tags")) != 0 {
		t.Fatalf("unknown tag leaked into proposal: %+v", fields)
	}
}

func TestTaxonomyRejectsGloballyAllowedTagsOutsidePromptScope(t *testing.T) {
	c, _ := setup(t)
	writeTestTaxonomy(t, c.RepositoryPath)
	vocabulary, err := taxonomy.Load(c.RepositoryPath)
	if err != nil {
		t.Fatal(err)
	}
	doc := database.Document{Path: "tools/x.md"}
	patch := metadataPatch{Set: map[string]any{
		"title": "Alpha",
		"tags":  []string{"tool", "rails"},
	}}
	suggestion, rejected := patchSuggestion(doc, nil, patch, "# Alpha\n", nil, "analysis", vocabulary)
	if suggestion == nil || !slices.ContainsFunc(rejected, func(reason string) bool {
		return strings.Contains(reason, "rails") && strings.Contains(reason, "prompt scope")
	}) {
		t.Fatalf("off-scope tag was not rejected independently: suggestion=%+v rejected=%v", suggestion, rejected)
	}
	fields, _, err := frontmatter.Parse(suggestion.ProposedContent)
	if err != nil {
		t.Fatal(err)
	}
	if fields["title"] != "Alpha" || len(frontmatter.StringList(fields, "tags")) != 0 {
		t.Fatalf("off-scope tag leaked into proposal: %+v", fields)
	}
}

func TestLocalTaxonomyTagsReplaceUnapprovedModelTagsInCombinedPatch(t *testing.T) {
	c, db := setup(t)
	writeTestTaxonomy(t, c.RepositoryPath)
	c.OllamaModelFast = "fast"
	c.EnrichmentMode = config.EnrichmentModeLocalFirst
	dir := filepath.Join(c.RepositoryPath, "languages")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ruby.md")
	if err := os.WriteFile(path, []byte("# Ruby\n\nA programming language.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	fast := &fakeLLM{reply: `{"title":"Alpha","summary":"Fast","tags":["fixture"]}`}
	full := generatorFunc(func(_ context.Context, prompt string, _ json.RawMessage) (string, error) {
		if strings.Contains(prompt, "Current path: languages/ruby.md\n") {
			return `{"title":"Ruby","summary":"Full","tags":["ruby"],"obsolete":false,
				"frontmatter_patch":{"set":{"related":["a.md"],"tags":["invented-framework"]},"remove":[]},"suggestions":[]}`, nil
		}
		return `{"title":"Alpha","summary":"Full","tags":["alpha"],"obsolete":false,"suggestions":[]}`, nil
	})
	result, err := RunProgress(t.Context(), c, db, Generators{Enrich: fast, Analysis: full}, RunOptions{})
	if err != nil || result.LocalEnriched != 1 {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	var proposal database.Suggestion
	pending, err := db.PendingSuggestions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, suggestion := range pending {
		if suggestion.DocumentPath == "languages/ruby.md" {
			proposal = suggestion
			break
		}
	}
	fields, _, err := frontmatter.Parse(proposal.ProposedContent)
	if err != nil || proposal.ID == 0 {
		t.Fatalf("combined proposal=%+v error=%v", proposal, err)
	}
	if got := frontmatter.StringList(fields, "tags"); !slices.Equal(got, []string{"programming-language", "ruby"}) {
		t.Fatalf("unapproved model tag displaced local taxonomy tags: %v", got)
	}
	if got := frontmatter.StringList(fields, "related"); !slices.Equal(got, []string{"a.md"}) {
		t.Fatalf("valid model relation was lost: %v", got)
	}
}

func TestLocalFirstRunsWithoutConfiguredFastModel(t *testing.T) {
	c, db := setup(t)
	writeTestTaxonomy(t, c.RepositoryPath)
	c.EnrichmentMode = config.EnrichmentModeLocalFirst
	dir := filepath.Join(c.RepositoryPath, "languages")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ruby.md")
	content := "# Ruby\n\nA programming language.\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	full := &fakeLLM{reply: `{"title":"Ruby","summary":"Language","tags":["ruby"],"obsolete":false,"suggestions":[]}`}
	result, err := RunProgress(t.Context(), c, db, Generators{Analysis: full}, RunOptions{})
	if err != nil || result.LocalEnriched != 1 || result.Enriched != 0 || len(full.prompts) != 2 {
		t.Fatalf("result=%+v full prompts=%d error=%v", result, len(full.prompts), err)
	}
	suggestions, err := db.PendingSuggestions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, suggestion := range suggestions {
		if suggestion.DocumentPath == "languages/ruby.md" && suggestion.Origin == "deterministic" {
			return
		}
	}
	t.Fatalf("local metadata proposal missing without fast model: %+v", suggestions)
}

func writeTestTaxonomy(t *testing.T, repository string) {
	t.Helper()
	const contents = `version: 1
kind: [programming-language, framework, library, database, cms, tool]
subject:
  languages: [css, less, scss, ruby]
  frameworks: [rails]
function: [testing, security]
`
	if err := os.WriteFile(filepath.Join(repository, "taxonomy.yaml"), []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestMarkdownH1IgnoresIndentedCodeAndUnescapesPunctuation(t *testing.T) {
	content := "    # Code comment, not title\n\n# C\\# reference\n"
	if got := markdownH1(content); got != "C# reference" {
		t.Fatalf("H1=%q, want C# reference", got)
	}
}

func TestEnrichmentBuildsMissingMetadataAndRetainsStageOutputs(t *testing.T) {
	c, db := setup(t)
	c.OllamaModelFast = "fast"
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	fast := &fakeLLM{reply: `{"title":"Alpha","summary":"Extracted summary","tags":["Ruby"," testing ","ruby"]}`}
	full := &fakeLLM{reply: `{"title":"Deep title","summary":"Deep summary","tags":["deep"],"suggestions":[]}`}
	result, err := RunProgress(t.Context(), c, db, Generators{Enrich: fast, Analysis: full}, RunOptions{})
	if err != nil || result.Enriched != 1 || result.Processed != 1 {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	suggestions, err := db.PendingSuggestions(t.Context())
	if err != nil || len(suggestions) != 1 || suggestions[0].Origin != "enrich" {
		t.Fatalf("suggestions=%+v error=%v", suggestions, err)
	}
	fields, body, err := frontmatter.Parse(suggestions[0].ProposedContent)
	if err != nil || fields["title"] != "Alpha" || body != "# Alpha doc" {
		t.Fatalf("fields=%+v body=%q error=%v", fields, body, err)
	}
	if got := frontmatter.StringList(fields, "tags"); !slices.Equal(got, []string{"ruby", "testing"}) {
		t.Fatalf("unnormalized proposal tags=%v", got)
	}
	docs, err := db.Documents(t.Context())
	if err != nil || docs[0].Title != "Deep title" || !strings.Contains(docs[0].TagsJSON, `"ruby"`) {
		t.Fatalf("analysis did not retain refreshed enrichment tags: %+v error=%v", docs, err)
	}
	var metadata, patchOutcome, patch, derived string
	err = db.QueryRow(`SELECT metadata_json,patch_outcome,patch_json,derived_fields_json
FROM stage_results WHERE stage='enrich'`).Scan(&metadata, &patchOutcome, &patch, &derived)
	if err != nil || patchOutcome != "suggested" || patch != "null" || derived != `["title","tags"]` {
		t.Fatalf("audit=%s %s %s error=%v", patchOutcome, patch, derived, err)
	}
	var retained struct {
		Title   string   `json:"title"`
		Summary string   `json:"summary"`
		Tags    []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(metadata), &retained); err != nil || retained.Title != "Alpha" || retained.Summary != "Extracted summary" || !slices.Equal(retained.Tags, []string{"Ruby", " testing ", "ruby"}) {
		t.Fatalf("Phi output was overwritten: %s error=%v", metadata, err)
	}
	original, err := os.ReadFile(filepath.Join(c.RepositoryPath, "a.md"))
	if err != nil || frontmatter.Canonical(string(original)) != "# Alpha doc" {
		t.Fatalf("analysis mutated source: %q error=%v", original, err)
	}
}

func TestEnrichmentDistinguishesPatchOutcomesWithoutDefaultingCuratedFields(t *testing.T) {
	content := "---\ntitle: Curated\ntags: [curated]\ncustom: keep\n---\n# Alpha\n"
	fields, _, err := frontmatter.Parse(content)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		patch   string
		outcome string
	}{
		{"omitted", "", "missing"},
		{"empty", `,"frontmatter_patch":{"set":{},"remove":[]}`, "empty"},
		{"same value", `,"frontmatter_patch":{"set":{"title":"Curated","tags":["curated"]},"remove":[]}`, "unchanged"},
		{"protected field", `,"frontmatter_patch":{"set":{"published":"2026-10-01"},"remove":[]}`, "rejected"},
		{"correction", `,"frontmatter_patch":{"set":{"title":"Corrected"},"remove":[]}`, "suggested"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gen := &fakeLLM{reply: `{"title":"Extracted","summary":"Summary","tags":["extracted"]` + tc.patch + `}`}
			result, err := runEnrich(t.Context(), gen, database.Document{Path: "a.md"}, fields, nil, content, "prompt")
			if err != nil || result.audit.PatchOutcome != tc.outcome || len(result.audit.DerivedFields) != 0 {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if tc.outcome != "suggested" && result.suggestion != nil {
				t.Fatalf("curated fields were overwritten by extraction: %+v", result.suggestion)
			}
		})
	}
}

func TestMalformedEnrichmentFallsThroughToAnalysis(t *testing.T) {
	for _, reply := range []string{
		`not JSON`, `{}`, `{"title":"A","summary":"S","tags":null}`,
		`{"title":42,"summary":"S","tags":[]}`,
		`{"title":" ","summary":"S","tags":[]}`,
	} {
		t.Run(reply, func(t *testing.T) {
			c, db := setup(t)
			c.OllamaModelFast = "fast"
			if _, err := scanner.Scan(t.Context(), c, db); err != nil {
				t.Fatal(err)
			}
			full := &fakeLLM{reply: `{"title":"A","summary":"S","tags":[],"suggestions":[]}`}
			result, err := RunProgress(t.Context(), c, db, Generators{
				Enrich: &fakeLLM{reply: reply}, Analysis: full,
			}, RunOptions{})
			if err != nil || result.EnrichmentFailed != 1 || result.Failed != 0 || result.Processed != 1 {
				t.Fatalf("result=%+v error=%v", result, err)
			}
		})
	}
}

func TestEnrichmentHasIndependentDeadline(t *testing.T) {
	c, db := setup(t)
	c.OllamaModelFast = "fast"
	c.RequestTimeoutSeconds = 600
	c.EnrichmentTimeoutSeconds = 1
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	fast := generatorFunc(func(ctx context.Context, _ string, _ json.RawMessage) (string, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > time.Second {
			t.Error("missing independent enrichment deadline")
		}
		<-ctx.Done()
		return "", ctx.Err()
	})
	full := &fakeLLM{reply: `{"title":"A","summary":"S","tags":[],"suggestions":[]}`}
	result, err := RunProgress(t.Context(), c, db, Generators{Enrich: fast, Analysis: full}, RunOptions{})
	if err != nil || result.EnrichmentFailed != 1 || result.Processed != 1 || result.Failed != 0 {
		t.Fatalf("deadline did not fall through: result=%+v error=%v", result, err)
	}
}

func TestParentCancellationDoesNotFallThroughToAnalysis(t *testing.T) {
	c, db := setup(t)
	c.OllamaModelFast = "fast"
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fast := generatorFunc(func(context.Context, string, json.RawMessage) (string, error) {
		cancel()
		return "", context.Canceled
	})
	full := &fakeLLM{}
	result, err := RunProgress(ctx, c, db, Generators{Enrich: fast, Analysis: full}, RunOptions{})
	if !errors.Is(err, context.Canceled) || len(full.prompts) != 0 || result.EnrichmentFailed != 0 {
		t.Fatalf("cancellation was treated as optional failure: %+v %v", result, err)
	}
}

func TestFallbackRechecksLivePrivacy(t *testing.T) {
	c, db := setup(t)
	c.OllamaModelFast = "fast"
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	fast := generatorFunc(func(context.Context, string, json.RawMessage) (string, error) {
		if err := os.WriteFile(filepath.Join(c.RepositoryPath, "a.md"), []byte("---\nkb_policy:\n  no_llm_context: true\n---\nPrivate now\n"), 0644); err != nil {
			t.Fatal(err)
		}
		return "", errors.New("optional generation failed")
	})
	full := &fakeLLM{}
	result, err := RunProgress(t.Context(), c, db, Generators{Enrich: fast, Analysis: full}, RunOptions{})
	if err != nil || result.EnrichmentFailed != 1 || result.Skipped != 1 || len(full.prompts) != 0 {
		t.Fatalf("privacy bypass through fallback: result=%+v calls=%d error=%v", result, len(full.prompts), err)
	}
}

func TestSourceReadAndParseFailuresNeverFallThrough(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(string) error
	}{
		{"missing file", os.Remove},
		{"invalid frontmatter", func(path string) error {
			return os.WriteFile(path, []byte("---\ntitle: [unclosed\n---\n# Invalid\n"), 0644)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, db := setup(t)
			c.OllamaModelFast = "fast"
			if _, err := scanner.Scan(t.Context(), c, db); err != nil {
				t.Fatal(err)
			}
			if err := tc.edit(filepath.Join(c.RepositoryPath, "a.md")); err != nil {
				t.Fatal(err)
			}
			fast, full := &fakeLLM{}, &fakeLLM{}
			result, err := RunProgress(t.Context(), c, db, Generators{Enrich: fast, Analysis: full}, RunOptions{})
			if err != nil || result.Failed != 1 || result.EnrichmentFailed != 0 || len(full.prompts) != 0 || len(fast.prompts) != 0 {
				t.Fatalf("source failure reached a model: result=%+v error=%v", result, err)
			}
		})
	}
}

func TestMissingEnrichmentPromptIsSystemic(t *testing.T) {
	c, db := setup(t)
	c.OllamaModelFast = "fast"
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(c.PromptDirectory, "enrich.md")); err != nil {
		t.Fatal(err)
	}
	fast, full := &fakeLLM{}, &fakeLLM{}
	result, err := RunProgress(t.Context(), c, db, Generators{Enrich: fast, Analysis: full}, RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "enrich.md") || result.EnrichmentFailed != 0 || len(full.prompts) != 0 {
		t.Fatalf("misconfiguration treated as optional model failure: result=%+v error=%v", result, err)
	}
}
