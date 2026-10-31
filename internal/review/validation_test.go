package review

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
)

func TestApplyRevalidatesUnsafePreviouslyAcceptedSuggestions(t *testing.T) {
	body := "# Note\n\nUse `value.sub(%r{/+\\z}, '')`.\n\nImportant caveat.\n"
	for _, tt := range []struct {
		name     string
		original string
		action   string
		target   string
		proposed string
	}{
		{name: "missing title", proposed: strings.TrimPrefix(body, "# Note\n\n")},
		{name: "padded loss", proposed: strings.Replace(body, "Important caveat.", strings.Repeat("Unrelated. ", 100), 1)},
		{name: "corrupted regex", proposed: strings.Replace(body, "\\z", "wrong", 1)},
		{name: "invented publication", proposed: "---\npublished: 2024-04-05\n---\n" + body},
		{name: "invented verification", proposed: "---\nreview_status: approved\n---\n" + body},
		{name: "invalid value", proposed: "---\ntags: [GO]\n---\n" + body},
		{name: "traversing link", proposed: "---\nrelated: [dir/../known.md]\n---\n" + body},
		{name: "missing link", proposed: "---\nrelated: [missing.md]\n---\n" + body},
		{name: "non-document link", proposed: "---\nrelated: [config.yml]\n---\n" + body},
		{name: "wrong move extension", action: "move", target: "ci.yml"},
		{name: "expand target", target: "none", proposed: body + "\nMore context.\n"},
		{name: "merge loses target", action: "merge", target: "target.md", proposed: body},
		{
			name: "changed privacy", original: "---\nkb_policy:\n  no_llm_context: false\n---\n" + body,
			proposed: "---\nkb_policy:\n  no_llm_context: true\n---\n" + body,
		},
		{
			name: "removed custom metadata", original: "---\ncustom: keep\n---\n" + body,
			proposed: body + "\nMore context.\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			original := tt.original
			if original == "" {
				original = body
			}
			for path, content := range map[string]string{
				"source.md": original, "target.md": "# Target\n\nUnique target caveat.\n",
				"known.md": "# Known", "config.yml": "key: value\n",
			} {
				if err := os.WriteFile(filepath.Join(repo, path), []byte(content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			hash := sha256.Sum256([]byte(original))
			doc := database.Document{Path: "source.md", SHA256: hex.EncodeToString(hash[:]), Size: int64(len(original)), TagsJSON: "[]"}
			if _, err := db.UpsertScanned(t.Context(), doc, "scan"); err != nil {
				t.Fatal(err)
			}
			action := tt.action
			if action == "" {
				action = "expand"
			}
			suggestion := database.Suggestion{
				Action: action, TargetPath: tt.target, ProposedContent: tt.proposed, Reason: "legacy unvalidated output",
			}
			if err := db.SaveAnalysis(t.Context(), doc, "old-model", "", "", nil, []database.Suggestion{suggestion}); err != nil {
				t.Fatal(err)
			}
			pending, err := db.PendingSuggestions(t.Context())
			if err != nil || len(pending) != 1 {
				t.Fatalf("pending=%v error=%v", pending, err)
			}
			if err := db.Decide(t.Context(), pending[0].ID, "accepted", ""); err != nil {
				t.Fatal(err)
			}
			results, err := Apply(t.Context(), config.Config{RepositoryPath: repo, WriteScope: "all"}, db)
			if err != nil || len(results) != 1 || results[0].Applied || results[0].Status != "failed" {
				t.Fatalf("results=%+v error=%v", results, err)
			}
			got, err := os.ReadFile(filepath.Join(repo, "source.md"))
			if err != nil || string(got) != original {
				t.Fatalf("source changed: %q error=%v", got, err)
			}
			got, err = os.ReadFile(filepath.Join(repo, "target.md"))
			if err != nil || string(got) != "# Target\n\nUnique target caveat.\n" {
				t.Fatalf("target changed: %q error=%v", got, err)
			}
			if _, err := os.Stat(filepath.Join(repo, ".kbm-trash")); !os.IsNotExist(err) {
				t.Fatalf("rejected edit created trash: %v", err)
			}
		})
	}
}

func TestApplyAllowsSafeMetadataPatchAndAdditiveBody(t *testing.T) {
	repo := t.TempDir()
	original := "---\npublished: 2026-10-01\ncustom: keep\n---\n# Note\n\nKeep `\\A` unchanged.\n"
	proposed := "---\npublished: 2026-10-01\ncustom: keep\ntags: [ruby]\nrelated: [known.md]\n---\n# Note\n\nKeep `\\A` unchanged.\n\nMore context.\n"
	for path, content := range map[string]string{"source.md": original, "known.md": "# Known"} {
		if err := os.WriteFile(filepath.Join(repo, path), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := applyOne(config.Config{RepositoryPath: repo, WriteScope: "all"}, database.Suggestion{
		Action: "expand", DocumentPath: "source.md", ProposedContent: proposed,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(repo, "source.md"))
	if err != nil || string(got) != proposed {
		t.Fatalf("content=%q error=%v", got, err)
	}
}

func TestApplyRevalidatesTagsAgainstCurrentTaxonomy(t *testing.T) {
	repo := t.TempDir()
	original := "---\ntags: [framework]\n---\n# Note\n"
	proposed := "---\ntags: [framework, invented]\n---\n# Note\n"
	if err := os.WriteFile(filepath.Join(repo, "taxonomy.yaml"), []byte(`version: 1
kind: [framework, tool, library, database, cms, programming-language]
subject:
  languages: [css, ruby]
function: [testing]
`), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo, "source.md")
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	err := applyOne(config.Config{RepositoryPath: repo, WriteScope: "all"}, database.Suggestion{
		Action: "expand", DocumentPath: "source.md", ProposedContent: proposed,
	})
	if err == nil || !strings.Contains(err.Error(), "invented") {
		t.Fatalf("accepted a tag absent from taxonomy.yaml: %v", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != original {
		t.Fatalf("source changed after rejected taxonomy tag: %q error=%v", got, readErr)
	}
}

func TestApplyRejectsGloballyAllowedTagOutsidePromptScope(t *testing.T) {
	repo := t.TempDir()
	original := "---\ntags: [tool]\n---\n# Note\n"
	proposed := "---\ntags: [tool, jekyll]\n---\n# Note\n\nMore context.\n"
	if err := os.WriteFile(filepath.Join(repo, "taxonomy.yaml"), []byte(`version: 1
kind: [tool, framework, library, database, cms, programming-language]
kind-dependent: [framework, library, cms]
requires-language: [framework, library, cms]
subject:
  platforms: [jekyll]
  languages: [css, ruby]
function: [testing]
`), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo, "tools", "source.md")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	err := applyOne(config.Config{RepositoryPath: repo, WriteScope: "all"}, database.Suggestion{
		Action: "expand", DocumentPath: "tools/source.md", ProposedContent: proposed,
	})
	if err == nil || !strings.Contains(err.Error(), "prompt scope") {
		t.Fatalf("accepted globally allowed but off-scope tag: %v", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != original {
		t.Fatalf("source changed after off-scope tag rejection: %q error=%v", got, readErr)
	}
}

func TestApplyRejectsFreshnessTagChanges(t *testing.T) {
	repo := t.TempDir()
	original := "---\ntags: [tool]\n---\n# Note\n\nA dated claim.\n"
	proposed := "---\ntags: [tool, testing]\n---\n# Note\n\nA dated claim.\n\nAs of 2026, the correction.\n"
	path := filepath.Join(repo, "source.md")
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	err := applyOne(config.Config{RepositoryPath: repo, WriteScope: "all"}, database.Suggestion{
		Action: "expand", DocumentPath: "source.md", Reason: "Freshness correction",
		Origin: "freshness", ProposedContent: proposed,
	})
	if err == nil || !strings.Contains(err.Error(), "preserve existing tags") {
		t.Fatalf("accepted freshness tag change: %v", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != original {
		t.Fatalf("source changed after freshness tag rejection: %q error=%v", got, readErr)
	}
}

func TestApplyRejectsLinkThroughExternalSymlink(t *testing.T) {
	repo, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "private.md"), []byte("# Private"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "outside")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "source.md"), []byte("# Note"), 0644); err != nil {
		t.Fatal(err)
	}
	err := applyOne(config.Config{RepositoryPath: repo, WriteScope: "all"}, database.Suggestion{
		Action: "expand", DocumentPath: "source.md",
		ProposedContent: "---\nrelated: [outside/private.md]\n---\n# Note",
	})
	if err == nil {
		t.Fatal("accepted link escaping repository through symlink")
	}
}

func TestMergeRejectsTargetChangedAfterAnalysis(t *testing.T) {
	repo := t.TempDir()
	source := "# Source\n\nKeep source.\n"
	target := "# Target\n\nKeep target.\n"
	for path, content := range map[string]string{"source.md": source, "target.md": target} {
		if err := os.WriteFile(filepath.Join(repo, path), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	proposed := source + "\n" + target
	currentTarget := target + "\nNew human-written caveat.\n"
	if err := os.WriteFile(filepath.Join(repo, "target.md"), []byte(currentTarget), 0644); err != nil {
		t.Fatal(err)
	}
	err := applyOne(config.Config{RepositoryPath: repo, WriteScope: "all"}, database.Suggestion{
		Action: "merge", DocumentPath: "source.md", TargetPath: "target.md", ProposedContent: proposed,
	})
	if err == nil {
		t.Fatal("merge discarded a new target caveat")
	}
	for path, want := range map[string]string{"source.md": source, "target.md": currentTarget} {
		got, err := os.ReadFile(filepath.Join(repo, path))
		if err != nil || string(got) != want {
			t.Fatalf("%s=%q error=%v", path, got, err)
		}
	}
}
