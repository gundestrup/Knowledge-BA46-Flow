package analyzer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/scanner"
)

func TestAnalysisMetadataPatchPreservesBodyAndProvenance(t *testing.T) {
	c, db := setup(t)
	original := "---\npublished: 2026-10-01\ncustom: keep\n---\n# Regex\n\nUse `value.sub(%r{/+\\z}, '')`.\n"
	for name, content := range map[string]string{"a.md": original, "b.md": "# Related note\n"} {
		if err := os.WriteFile(filepath.Join(c.RepositoryPath, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	docs, err := db.Documents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range docs {
		if doc.Path == "b.md" {
			if err := db.SaveAnalysis(t.Context(), doc, c.OllamaModel, "Related", "", nil, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	llm := &fakeLLM{reply: `{"title":"Regex","summary":"S","tags":["ruby"],"obsolete":false,
		"frontmatter_patch":{"set":{"related":["b.md"],"tags":["ruby"]},"remove":[]},"suggestions":[]}`}
	result, err := Run(t.Context(), c, db, llm)
	if err != nil || result.Processed != 1 || result.Rejected != 0 {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	sugs, err := db.PendingSuggestions(t.Context())
	if err != nil || len(sugs) != 1 {
		t.Fatalf("suggestions=%+v error=%v", sugs, err)
	}
	fields, body, err := frontmatter.Parse(sugs[0].ProposedContent)
	_, originalBody, _ := frontmatter.Parse(original)
	if err != nil || body != originalBody || fields["custom"] != "keep" || fields["published"] == nil {
		t.Fatalf("metadata=%v body=%q error=%v", fields, body, err)
	}
	if got, err := os.ReadFile(filepath.Join(c.RepositoryPath, "a.md")); err != nil || string(got) != original {
		t.Fatalf("analysis changed source: %q error=%v", got, err)
	}
}

func TestAnalysisRejectsUnsafeProposalsAndReportsReasons(t *testing.T) {
	c, db := setup(t)
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{reply: `{"title":"A","summary":"S","tags":[],"obsolete":false,
		"frontmatter_patch":{"set":{"published":"2024-04-05","related":["missing.md"]},"remove":[]},
		"suggestions":[
			{"action":"move","target_path":"ci.yml","reason":"confused file","proposed_content":""},
			{"action":"expand","target_path":"","reason":"missing H1","proposed_content":"Alpha doc"},
			{"action":"expand","target_path":"","reason":"invented date","proposed_content":"---\npublished: 2024-04-05\n---\n# Alpha doc"},
			{"action":"expand","target_path":"","reason":"invalid link","proposed_content":"---\nrelated: [dir/../a.md]\n---\n# Alpha doc"}
		]}`}
	var events []Progress
	result, err := RunProgress(t.Context(), c, db, Generators{Analysis: llm}, RunOptions{
		Progress: func(p Progress) { events = append(events, p) },
	})
	if err != nil || result.Rejected != 6 || result.Processed != 1 {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	sugs, err := db.PendingSuggestions(t.Context())
	if err != nil || len(sugs) != 0 {
		t.Fatalf("unsafe proposals reached review: %+v error=%v", sugs, err)
	}
	if !strings.Contains(events[len(events)-1].Detail, "rejected 6 unsafe edit(s)") {
		t.Fatalf("events=%+v", events)
	}
}

func TestValidateSuggestionRejectsConflictingOrIncompatibleMoves(t *testing.T) {
	c, _ := setup(t)
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "existing.md"), []byte("# Existing"), 0644); err != nil {
		t.Fatal(err)
	}
	p := &pipeline{c: c}
	for _, tt := range []struct {
		target  string
		content string
		moves   bool
	}{
		{target: "ci.yml"}, {target: "test-helpers.sh"},
		{target: "existing.md"}, {target: "../outside.md"},
		{target: "b.md", content: "Copy the helper"},
		{target: "b.md", moves: true},
	} {
		t.Run(tt.target+tt.content, func(t *testing.T) {
			s := database.Suggestion{
				Action: "move", DocumentPath: "a.md", TargetPath: tt.target,
				Reason: "move", ProposedContent: tt.content,
			}
			if err := p.validateSuggestion(s, "# Alpha doc", nil, tt.moves); err == nil {
				t.Fatal("unsafe move accepted")
			}
		})
	}
}

func TestMergeRequiresCompleteTargetAndRetainsBothSources(t *testing.T) {
	c, _ := setup(t)
	target := "# Target\n\nKeep the target's unique caveat.\n"
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "b.md"), []byte(target), 0644); err != nil {
		t.Fatal(err)
	}
	p := &pipeline{c: c}
	ranked := []candidate{{doc: database.Document{Path: "b.md", Size: int64(len(target))}, sim: 0.9}}
	context, bodies := p.mergeContext(ranked)
	if !strings.Contains(context, target) || len(bodies) != 1 {
		t.Fatalf("context=%q bodies=%v", context, bodies)
	}
	source := "# Source\n\nKeep the source.\n"
	s := database.Suggestion{Action: "merge", DocumentPath: "a.md", TargetPath: "b.md", Reason: "overlap"}
	s.ProposedContent = source
	if err := p.validateSuggestion(s, source, bodies, false); err == nil {
		t.Fatal("merge discarded target body")
	}
	s.ProposedContent = source + "\n" + target
	if err := p.validateSuggestion(s, source, nil, false); err == nil {
		t.Fatal("merge allowed without a fully supplied target")
	}
	if err := p.validateSuggestion(s, source, bodies, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "b.md"), []byte("---\nkb_policy:\n  no_llm_context: true\n---\nSecret\n"), 0644); err != nil {
		t.Fatal(err)
	}
	context, bodies = p.mergeContext(ranked)
	if context != "" || len(bodies) != 0 {
		t.Fatalf("private merge target leaked: %q", context)
	}
}

func TestEnrichmentSchemaHasOnlyTitleAndTagsPatchFields(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			Properties map[string]struct {
				Properties           map[string]any `json:"properties"`
				AdditionalProperties bool           `json:"additionalProperties"`
			} `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(enrichSchema, &schema); err != nil {
		t.Fatal(err)
	}
	set := schema.Properties["frontmatter_patch"].Properties["set"]
	if len(set.Properties) != 2 || set.Properties["title"] == nil || set.Properties["tags"] == nil || set.AdditionalProperties {
		t.Fatalf("patch schema=%+v", set)
	}
}
