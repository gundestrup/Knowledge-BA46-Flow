package analyzer

import (
	"slices"
	"strings"
	"testing"

	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/similarity"
)

func TestEntityIndexUsesCuratedFrameworkCatalogAndVisibleTargets(t *testing.T) {
	visible := map[string]bool{
		"frameworks/README.md":       true,
		"frameworks/rails/README.md": true,
		"languages/ruby.md":          true,
		"tools/rspec.md":             true,
	}
	index := newEntityIndex()
	index.addDocument("frameworks/README.md", nil, `# Framework landscape

## Languages and their dominant frameworks

### Ruby — web applications
- Ruby on Rails — full-stack web framework.
- RSpec — behavior-driven testing framework.
- Django — batteries-included Python framework.

## Database environments
- PostgreSQL - relational database.
`)
	index.addDocument("frameworks/rails/README.md", nil, "# Ruby on Rails\n\nA web framework.\n")
	index.addDocument("languages/ruby.md", nil, "# Ruby\n\nA programming language.\n")
	index.addDocument("tools/rspec.md", nil, "# RSpec\n\nA Ruby test framework.\n")
	index.addFrameworkCatalog(`## Languages and their dominant frameworks

### Ruby — web applications
- Ruby on Rails — full-stack web framework.
- RSpec — behavior-driven testing framework.
- Django — batteries-included Python framework.
`, visible)

	matches := index.matches("notes/deploy.md", `Ruby on Rails supports this workflow.
Django is listed in the framework landscape.

`+"```ruby"+`
RSpec.describe "do not use code-only matches" do
end
`+"```"+`
`)
	got := make(map[string][]string)
	for _, match := range matches {
		got[match.path] = append(got[match.path], match.term)
	}
	if !slices.Contains(got["frameworks/rails/README.md"], "Ruby on Rails") {
		t.Fatalf("catalog alias did not resolve to the framework note: %+v", matches)
	}
	if !slices.Contains(got["languages/ruby.md"], "ruby") {
		t.Fatalf("language heading was not indexed: %+v", matches)
	}
	if !slices.Contains(got["frameworks/README.md"], "Django") {
		t.Fatalf("catalog-only framework term was not retained as a weak candidate: %+v", matches)
	}
	if slices.Contains(got["tools/rspec.md"], "RSpec") {
		t.Fatalf("code-fence mention became a candidate: %+v", matches)
	}
}

func TestFrameworkCatalogResolvesLanguageLibrariesUnderLibrariesRoot(t *testing.T) {
	visible := map[string]bool{
		"frameworks/README.md":    true,
		"libraries/ruby/rspec.md": true,
	}
	index := newEntityIndex()
	index.addDocument("libraries/ruby/rspec.md", nil, "# RSpec\n\nRuby testing library.\n")
	index.addFrameworkCatalog(`## Languages and their dominant frameworks

### Ruby — web applications
- RSpec — behavior-driven testing library.
`, visible)
	matches := index.matches("notes/testing.md", "The RSpec suite covers this Ruby project.")
	if !slices.ContainsFunc(matches, func(match entityMention) bool {
		return match.path == "libraries/ruby/rspec.md"
	}) {
		t.Fatalf("catalog entry did not resolve to the language-scoped library note: %+v", matches)
	}
}

func TestEntityIndexUsesExplicitCatalogLinksWithoutBroadcastingCategories(t *testing.T) {
	visible := map[string]bool{
		"frameworks/css/README.md": true,
		"frameworks/css/uikit.md":  true,
		"frameworks/css/bulma.md":  true,
		"languages/css.md":         true,
	}
	catalog := `---
tags: [framework, css]
---
# CSS frameworks

Catalog of CSS frameworks.

- [UIkit](uikit.md) — Modular CSS framework.
- [Bulma](bulma.md) — Flexbox CSS framework.
`
	catalog += "Literal syntax example: `[Example UI](uikit.md)`.\n"
	index := newEntityIndex()
	index.addDocument("frameworks/css/README.md", map[string]any{"tags": []any{"framework", "css"}}, catalog)
	index.addDocument("frameworks/css/uikit.md", nil, "# UIkit\n\nA modular CSS framework.\n")
	index.addDocument("frameworks/css/bulma.md", nil, "# Bulma\n\nA Flexbox framework.\n")
	index.addDocument("languages/css.md", nil, "# CSS\n\nA style-sheet language.\n")
	index.addExplicitMarkdownLinks("frameworks/css/README.md", catalog)
	index.resolveExplicitMarkdownLinks(visible)

	mentioned := index.matches("notes/ui-choice.md", "Use UIkit for this interface.")
	if !slices.ContainsFunc(mentioned, func(match entityMention) bool {
		return match.path == "frameworks/css/uikit.md"
	}) {
		t.Fatalf("explicit catalog link did not resolve UIkit: %+v", mentioned)
	}
	if slices.ContainsFunc(mentioned, func(match entityMention) bool {
		return match.path == "frameworks/css/bulma.md"
	}) {
		t.Fatalf("a sibling in the same CSS directory became a name match: %+v", mentioned)
	}
	codeExample := index.matches("notes/example.md", "Example UI is only shown as syntax.")
	if slices.ContainsFunc(codeExample, func(match entityMention) bool {
		return match.path == "frameworks/css/uikit.md"
	}) {
		t.Fatalf("inline-code link syntax became a catalog alias: %+v", codeExample)
	}
	cssMentioned := index.matches("notes/css.md", "CSS needs a reset.")
	if slices.ContainsFunc(cssMentioned, func(match entityMention) bool {
		return strings.HasPrefix(match.path, "frameworks/css/") && match.path != "frameworks/css/README.md"
	}) {
		t.Fatalf("category name CSS flooded child framework candidates: %+v", cssMentioned)
	}
}

func TestEntityIndexPreservesPunctuationInTechnicalNames(t *testing.T) {
	index := newEntityIndex()
	index.addDocument("languages/cpp.md", nil, "# C++\n\nCompiled language.\n")
	index.addDocument("languages/csharp.md", nil, "# C\\#\n\nManaged language.\n")
	index.addDocument("languages/fsharp.md", nil, "# F#\n\nFunctional language.\n")
	for text, path := range map[string]string{
		"C++": "languages/cpp.md",
		"C#":  "languages/csharp.md",
		"F#":  "languages/fsharp.md",
	} {
		matches := index.matches("notes/compiler.md", "This uses "+text+" for the implementation.")
		if !slices.ContainsFunc(matches, func(match entityMention) bool { return match.path == path }) {
			t.Errorf("technical name %q did not match %s: %+v", text, path, matches)
		}
	}
}

func TestEntityEvidenceIncludesProseAfterMatchedHeading(t *testing.T) {
	got := entityExcerpts("# UIkit\n\nA modular front-end framework with CSS components.\n", []entityMention{
		{path: "frameworks/css/uikit.md", term: "UIkit"},
	})
	if len(got) != 1 || !strings.Contains(got[0], "modular front-end framework") || strings.Contains(got[0], "# UIkit") {
		t.Fatalf("evidence excerpt=%v", got)
	}
}

func TestEntityIndexShortLanguageNamesRequireExactCapitalization(t *testing.T) {
	index := newEntityIndex()
	index.addDocument("languages/go.md", nil, "# Go toolchain\n\nGo is compiled.\n")

	if got := index.matches("notes/mistake.md", "We should go to the next step."); len(got) != 0 {
		t.Fatalf("common verb matched the Go language: %+v", got)
	}
	if got := index.matches("notes/language.md", "The tool is written in Go."); len(got) == 0 || got[0].path != "languages/go.md" {
		t.Fatalf("capitalized Go mention was not matched: %+v", got)
	}
}

func TestRankCandidatesPrefersExactEntityEvidence(t *testing.T) {
	documents := []database.Document{
		{Path: "frameworks/rails/README.md"},
		{Path: "patterns/other.md"},
	}
	signature := similarity.Signature("same words describe a generic deployment")
	sigs := map[string][]uint64{
		"frameworks/rails/README.md": similarity.Signature("rails and ruby framework"),
		"patterns/other.md":          signature,
	}
	docEmbedding := []float32{1, 0}
	embeddings := map[string][]float32{
		"frameworks/rails/README.md": {0, 1},
		"patterns/other.md":          {1, 0},
	}
	mentions := map[string][]entityMention{
		"frameworks/rails/README.md": {{path: "frameworks/rails/README.md", term: "Rails"}},
	}
	got := rankCandidates("notes/deploy.md", nil, documents, signature, sigs, docEmbedding, embeddings, mentions, 2)
	if len(got) != 2 || got[0].doc.Path != "frameworks/rails/README.md" {
		t.Fatalf("exact entity mention should outrank generic similarity: %+v", got)
	}
}
