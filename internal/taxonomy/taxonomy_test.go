package taxonomy

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testTaxonomy = `version: 1
kind:
  - programming-language
  - framework
  - library
  - database
  - cms
  - tool
kind-major: [programming-language, database]
kind-dependent: [framework, library, cms, tool]
requires-language: [framework, library, cms]
subject:
  languages:
    - css
    - cpp
    - ruby
    - javascript
    - shell
  frameworks:
    - jekyll
  database-categories:
    - relational
function:
  - testing
  - security
`

func TestTagsForPathUsesOnlyApprovedKindAndSubject(t *testing.T) {
	t.Parallel()
	vocabulary := mustParse(t, []byte(testTaxonomy))
	for path, want := range map[string][]string{
		"languages/ruby.md":                  {"programming-language", "ruby"},
		"frameworks/css/uikit.md":            {"framework", "css"},
		"frameworks/jekyll/README.md":        nil,
		"libraries/ruby/rspec.md":            {"library", "ruby"},
		"databases/relational/postgresql.md": {"database", "relational"},
		"tools/sonarqube.md":                 {"tool"},
		"tools/shell/README.md":              {"tool", "shell"},
		"languages/README.md":                nil,
		"lessons-learned/local-llm.md":       nil,
	} {
		if got := vocabulary.TagsForPath(path); !slices.Equal(got, want) {
			t.Errorf("%s tags=%v, want %v", path, got, want)
		}
	}
}

func TestRequiresLanguageRuleRejectsUnscopedDependentKinds(t *testing.T) {
	t.Parallel()
	vocabulary := mustParse(t, []byte(testTaxonomy))
	if invalid := vocabulary.InvalidTagSet([]string{"framework", "testing"}); len(invalid) == 0 {
		t.Fatal("framework without a language subject passed")
	}
	if invalid := vocabulary.InvalidTagSet([]string{"framework", "css", "testing"}); len(invalid) != 0 {
		t.Fatalf("framework with a language subject was rejected: %v", invalid)
	}
	if invalid := vocabulary.InvalidTagSet([]string{"framework", "library", "ruby"}); len(invalid) == 0 {
		t.Fatal("multiple kind tags passed")
	}
	if invalid := vocabulary.InvalidTagSet([]string{"ruby", "testing"}); len(invalid) == 0 {
		t.Fatal("tag set without a kind passed")
	}
}

func TestTaxonomyValidatesNewTagsButPreservesExistingHumanValues(t *testing.T) {
	t.Parallel()
	vocabulary := mustParse(t, []byte(testTaxonomy))
	original := "---\ntags: [framework, legacy-custom]\n---\n# Note\n"
	if err := vocabulary.ValidateTagChanges(original, "---\ntags: [framework, legacy-custom, css]\n---\n# Note\n"); err != nil {
		t.Fatalf("allowed new tag rejected: %v", err)
	}
	if err := vocabulary.ValidateTagChanges(original, "---\ntags: [framework, legacy-custom, invented]\n---\n# Note\n"); err == nil || !strings.Contains(err.Error(), "invented") {
		t.Fatalf("unapproved tag was accepted: %v", err)
	}
}

func TestPromptTagsExcludeStoredUnapprovedTags(t *testing.T) {
	t.Parallel()
	vocabulary := mustParse(t, []byte(testTaxonomy))
	got := vocabulary.PromptTags("languages/ruby.md", []string{"ruby", "invented-model-tag"})
	if !slices.Contains(got, "programming-language") || !slices.Contains(got, "testing") ||
		slices.Contains(got, "invented-model-tag") {
		t.Fatalf("prompt tags=%v", got)
	}
}

func TestLoadFromRootIsOptionalAndFailsClosedOnMalformedTaxonomy(t *testing.T) {
	t.Parallel()
	rootPath := t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	vocabulary, err := LoadFromRoot(root)
	if err != nil || vocabulary != nil {
		t.Fatalf("missing taxonomy should disable controlled tags: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, fileName), []byte("version: [bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFromRoot(root); err == nil {
		t.Fatal("malformed taxonomy was accepted")
	}
	root.Close()
}

func TestLoadReadsTaxonomyFromRepository(t *testing.T) {
	t.Parallel()
	// Missing taxonomy.yaml is not an error — a repository may legitimately
	// carry no vocabulary and callers treat nil as "unrestricted".
	dir := t.TempDir()
	v, err := Load(dir)
	if err != nil || v != nil {
		t.Fatalf("empty dir: v=%v err=%v", v, err)
	}
	if _, err := Load(filepath.Join(dir, "no-such-dir")); err == nil {
		t.Error("unreadable repository path should fail")
	}
	if err := os.WriteFile(filepath.Join(dir, "taxonomy.yaml"), []byte(testTaxonomy), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Allows("ruby") {
		t.Error("loaded vocabulary should allow declared subject")
	}
	// Non-regular and oversized files fail closed.
	if err := os.Remove(filepath.Join(dir, "taxonomy.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "taxonomy.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("taxonomy.yaml as a directory should fail")
	}
}

func TestParseValidatesWithoutFilesystem(t *testing.T) {
	t.Parallel()
	v, err := Parse([]byte(testTaxonomy))
	if err != nil {
		t.Fatal(err)
	}
	if !v.Allows("framework") {
		t.Error("parsed vocabulary should allow declared kind")
	}
	for _, bad := range []string{
		"version: 2\nkind: [tool]\nsubject: {x: [y]}\n",
		"version: 1\nkind: [tool]\n", // missing subject
		"version: 1\nkind: [Bad Tag]\nsubject: {x: [y]}\n",
		"version: 1\nkind: [tool]\nkind-major: [undeclared]\nsubject: {x: [y]}\n",
		"version: 1\nkind: [tool]\nrequires-language: [tool]\nsubject: {x: [y]}\n", // requires-language kind must be kind-dependent
		"::: not yaml",
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("malformed taxonomy should fail: %q", bad)
		}
	}
}

func TestInvalidTagsForPathExemptsIndexReadme(t *testing.T) {
	t.Parallel()
	vocabulary := mustParse(t, []byte(testTaxonomy))
	// A dependent kind without a language subject violates requires-language —
	// except on an aggregating index README. jekyll is a framework subject,
	// so it does not satisfy the language requirement.
	if invalid := vocabulary.InvalidTagsForPath("frameworks/css/uikit.md", []string{"framework", "jekyll"}); len(invalid) == 0 {
		t.Error("regular page should report requires-language violation")
	}
	if invalid := vocabulary.InvalidTagsForPath("frameworks/css/README.md", []string{"framework", "jekyll"}); len(invalid) != 0 {
		t.Errorf("index README should be exempt, got %v", invalid)
	}
	// Unknown and kind violations still apply on a README.
	if invalid := vocabulary.InvalidTagsForPath("README.md", []string{"bogus-tag"}); len(invalid) == 0 {
		t.Error("unknown tag on README should still be reported")
	}
	// Non-list values are ignored, not violations.
	if invalid := vocabulary.InvalidTagsForPath("a.md", 42); invalid != nil {
		t.Errorf("non-list value: %v", invalid)
	}
	if v := (*Vocabulary)(nil); v.InvalidTagsForPath("a.md", []string{"framework"}) != nil {
		t.Error("nil vocabulary should not report")
	}
}

func mustParse(t *testing.T, data []byte) *Vocabulary {
	t.Helper()
	vocabulary, err := parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return vocabulary
}

func TestAllowedTagsReturnsSortedApprovedSet(t *testing.T) {
	t.Parallel()
	var nilVocab *Vocabulary
	if got := nilVocab.AllowedTags(); got != nil {
		t.Fatalf("nil vocabulary should have no tags: %v", got)
	}
	v := mustParse(t, []byte(testTaxonomy))
	got := v.AllowedTags()
	if len(got) == 0 || !slices.IsSorted(got) {
		t.Fatalf("expected a sorted non-empty tag set: %v", got)
	}
	if !slices.Contains(got, "ruby") || !slices.Contains(got, "tool") {
		t.Fatalf("approved subject/kind tags missing: %v", got)
	}
}

// FuzzParse pins the fail-closed contract on arbitrary taxonomy bytes: parse
// is deterministic, never panics, and a nil error always yields a usable
// vocabulary whose AllowedTags are exactly the tags Allows accepts.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		testTaxonomy,
		"version: 1\nkind: [tool]\nsubject: {x: [y]}\n",
		"version: 2\nkind: [tool]\nsubject: {x: [y]}\n",
		"version: 1\nkind: [Bad Tag]\nsubject: {x: [y]}\n",
		"::: not yaml",
		"",
		"\xff\xfe binary",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data string) {
		v1, err1 := Parse([]byte(data))
		v2, err2 := Parse([]byte(data))
		if (err1 == nil) != (err2 == nil) {
			t.Fatalf("Parse not deterministic: %v vs %v", err1, err2)
		}
		if err1 != nil {
			return
		}
		if v1 == nil || v2 == nil {
			t.Fatalf("successful parse returned nil vocabulary for %q", data)
		}
		for _, tag := range v1.AllowedTags() {
			if !v1.Allows(tag) {
				t.Fatalf("AllowedTags lists %q but Allows rejects it", tag)
			}
			if !slices.Contains(v2.AllowedTags(), tag) {
				t.Fatalf("AllowedTags not deterministic for %q", tag)
			}
		}
		// Fuzzed vocabularies must stay safe under the path/tag queries.
		_ = v1.TagsForPath("frameworks/css/x.md")
		_ = v1.InvalidTagSet([]string{"framework", "bogus"})
	})
}
