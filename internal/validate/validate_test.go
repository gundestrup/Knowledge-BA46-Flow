package validate

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/taxonomy"
)

const testTaxonomy = `version: 1
kind: [programming-language, framework, library, database, cms, tool, pattern]
kind-major: [programming-language, database]
kind-dependent: [framework, library, cms, tool]
requires-language: [framework, library, cms]
subject:
  languages: [ruby, css, go, jekyll]
function: [testing, security]
`

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func testConfig(t *testing.T, root string, exempt []string) config.Config {
	t.Helper()
	return config.Config{
		RepositoryPath:   root,
		Extensions:       []string{".md"},
		MaxDocumentBytes: 200000,
		Validate:         config.ValidateConfig{Exempt: exempt},
	}
}

// healthyRepo builds a minimal valid repository: one language, one library
// catalog with one entry, two-way links, and a linked snippet.
func healthyRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "taxonomy.yaml", testTaxonomy)
	writeFile(t, root, "languages/README.md", "---\ntags: [programming-language]\n---\n# Languages\n[ruby](ruby.md)\n")
	writeFile(t, root, "languages/ruby.md",
		"---\ntags: [programming-language, ruby]\n---\n# Ruby\nLibraries: [libraries/ruby](libraries/ruby/README.md)\nCatalog: [Languages](README.md).\n")
	writeFile(t, root, "libraries/ruby/README.md",
		"---\ntags: [library, ruby]\n---\n# Ruby libraries\n- [rspec](rspec.md)\nLanguage: [ruby](../../languages/ruby.md). Tools helper.sh.\n")
	writeFile(t, root, "libraries/ruby/rspec.md",
		"---\ntags: [library, ruby, testing]\n---\n# RSpec\nCatalog: [Ruby libraries](README.md). Language: [Ruby](../../languages/ruby.md). See helper.sh\n")
	writeFile(t, root, "libraries/ruby/helper.sh", "#!/bin/sh\nexit 0\n")
	return root
}

func mustViolations(t *testing.T, root string, exempt []string) []string {
	t.Helper()
	vocabulary, err := taxonomy.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	violations, err := Run(context.Background(), testConfig(t, root, exempt), vocabulary)
	if err != nil {
		t.Fatal(err)
	}
	return violations
}

func hasViolation(violations []string, substr string) bool {
	return slices.ContainsFunc(violations, func(v string) bool { return strings.Contains(v, substr) })
}

func TestRunAcceptsHealthyRepo(t *testing.T) {
	if v := mustViolations(t, healthyRepo(t), nil); len(v) != 0 {
		t.Fatalf("unexpected violations: %v", v)
	}
}

func TestRunDetectsMissingAndInvalidTags(t *testing.T) {
	root := healthyRepo(t)
	writeFile(t, root, "patterns/p1.md",
		"---\ntags: [pattern, nope]\n---\n# P\nCatalog: [Patterns](README.md). see p.sh\n")
	writeFile(t, root, "patterns/README.md", "---\ntags: [pattern]\n---\n# P\n[p1](p1.md) p.sh\n")
	writeFile(t, root, "patterns/p.sh", "x\n")
	v := mustViolations(t, root, nil)
	if !hasViolation(v, "patterns/p1.md: tag(s) not listed") && !hasViolation(v, "nope") {
		t.Fatalf("expected unknown-tag violation, got %v", v)
	}
	writeFile(t, root, "patterns/p2.md", "# no frontmatter\nCatalog: README.md\n")
	writeFile(t, root, "patterns/README.md",
		"---\ntags: [pattern]\n---\n# P\n[p1](p1.md) [p2](p2.md) p.sh\n")
	v = mustViolations(t, root, nil)
	if !hasViolation(v, "patterns/p2.md") {
		t.Fatalf("expected missing-tags violation, got %v", v)
	}
}

func TestRunRequiresLanguageOnEntitiesButNotIndexes(t *testing.T) {
	root := healthyRepo(t)
	writeFile(t, root, "cms/jekyll/README.md",
		"---\ntags: [cms, ruby]\n---\n# Jekyll CMS\n[entry](entry.md)\n")
	writeFile(t, root, "cms/jekyll/entry.md",
		"---\ntags: [cms]\n---\n# Entry\nCatalog: README.md\n")
	v := mustViolations(t, root, nil)
	if !hasViolation(v, "cms/jekyll/entry.md") || !hasViolation(v, "require a subject.languages tag") {
		t.Fatalf("expected requires-language violation, got %v", v)
	}
	writeFile(t, root, "cms/jekyll/entry.md",
		"---\ntags: [cms, ruby]\n---\n# Entry\nCatalog: README.md\n")
	if v := mustViolations(t, root, nil); len(v) != 0 {
		t.Fatalf("unexpected violations after fix: %v", v)
	}
	writeFile(t, root, "cms/jekyll/README.md",
		"---\ntags: [cms]\n---\n# Jekyll CMS index — aggregates subjects\n[entry](entry.md)\n")
	if v := mustViolations(t, root, nil); len(v) != 0 {
		t.Fatalf("index README must be exempt from requires-language, got %v", v)
	}
}

func TestRunDetectsMissingBackLinkAndIndexListing(t *testing.T) {
	root := healthyRepo(t)
	writeFile(t, root, "patterns/orphan-note.md",
		"---\ntags: [pattern]\n---\n# No links anywhere\n")
	v := mustViolations(t, root, nil)
	if !hasViolation(v, "patterns/orphan-note.md: no index back-link") {
		t.Fatalf("expected back-link violation, got %v", v)
	}
	if !hasViolation(v, "no index README in any ancestor") {
		t.Fatalf("expected missing-index violation, got %v", v)
	}
	writeFile(t, root, "patterns/README.md", "---\ntags: [pattern]\n---\n# P\n")
	writeFile(t, root, "patterns/orphan-note.md",
		"---\ntags: [pattern]\n---\n# P\nCatalog: README.md\n")
	v = mustViolations(t, root, nil)
	if !hasViolation(v, "patterns/README.md: does not list orphan-note.md") {
		t.Fatalf("expected index-listing violation, got %v", v)
	}
}

func TestRunDetectsLanguageCatalogEdges(t *testing.T) {
	root := healthyRepo(t)
	writeFile(t, root, "languages/ruby.md",
		"---\ntags: [programming-language, ruby]\n---\n# Ruby\nCatalog: [Languages](README.md).\n")
	v := mustViolations(t, root, nil)
	if !hasViolation(v, "languages/ruby.md: does not link catalog libraries/ruby/README.md") {
		t.Fatalf("expected language->catalog violation, got %v", v)
	}
	writeFile(t, root, "languages/ruby.md",
		"---\ntags: [programming-language, ruby]\n---\n# Ruby\nLibs: libraries/ruby/README.md\nCatalog: [Languages](README.md).\n")
	writeFile(t, root, "libraries/ruby/rspec.md",
		"---\ntags: [library, ruby, testing]\n---\n# RSpec\nCatalog: [Ruby libraries](README.md). helper.sh\n")
	v = mustViolations(t, root, nil)
	if !hasViolation(v, "libraries/ruby/rspec.md: does not link language note") {
		t.Fatalf("expected entry->language violation, got %v", v)
	}
}

func TestRunDetectsOrphanSnippets(t *testing.T) {
	root := healthyRepo(t)
	writeFile(t, root, "tools/x.md",
		"---\ntags: [tool]\n---\n# X\nCatalog: [Tools](README.md)\n")
	writeFile(t, root, "tools/README.md", "---\ntags: [tool]\n---\n# Tools\n[x](x.md)\n")
	writeFile(t, root, "tools/loose.sh", "x\n")
	v := mustViolations(t, root, nil)
	if !hasViolation(v, "tools/loose.sh: non-Markdown file not referenced") {
		t.Fatalf("expected orphan violation, got %v", v)
	}
	writeFile(t, root, "tools/x.md",
		"---\ntags: [tool]\n---\n# X\nCatalog: [Tools](README.md)\nRuns loose.sh\n")
	if v := mustViolations(t, root, nil); len(v) != 0 {
		t.Fatalf("unexpected violations after link: %v", v)
	}
}

func TestRunHonorsExemptScope(t *testing.T) {
	root := healthyRepo(t)
	writeFile(t, root, "temp/draft.md", "# no frontmatter\n")
	writeFile(t, root, "temp/blob.bin", "x\n")
	if v := mustViolations(t, root, nil); len(v) == 0 {
		t.Fatal("expected violations without exempt scope")
	}
	if v := mustViolations(t, root, []string{"temp/**"}); len(v) != 0 {
		t.Fatalf("exempt scope should silence violations, got %v", v)
	}
}

func TestRunWithoutTaxonomyStillChecksLinks(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "notes/README.md", "# index\n")
	writeFile(t, root, "notes/a.md", "no frontmatter\n")
	violations, err := Run(context.Background(), testConfig(t, root, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasViolation(violations, "notes/a.md") {
		t.Fatalf("expected structural violations without taxonomy, got %v", violations)
	}
}
