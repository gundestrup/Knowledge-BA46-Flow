package analyzer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/review"
	"knowledge-ba46-flow/internal/scanner"
)

func TestLiveRelativeLinksProduceNoDeterministicRepair(t *testing.T) {
	c, db := taskSetup(t)
	writeDoc(t, c.RepositoryPath, "frameworks/css/README.md", "# CSS catalog\n")
	writeDoc(t, c.RepositoryPath, "languages/scss.md",
		"---\ntags: [scss]\n---\n# SCSS\n\n[CSS](../frameworks/css/README.md)\n")
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	known, err := db.Documents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	content, err := scanner.ReadDocument(c, "languages/scss.md")
	if err != nil {
		t.Fatal(err)
	}
	fields, _, _ := frontmatter.Parse(content)
	s, ok := deterministicFix("languages/scss.md", content, fields, known,
		func(path string) (string, bool) { return db.RetargetPath(t.Context(), path) })
	if ok {
		t.Fatalf("no rename occurred; no repair should exist: %+v", s)
	}
}

func TestBodyOnlyDocumentCanReceiveVerifiedLinkRepair(t *testing.T) {
	repair, ok := deterministicFix("sub/note.md", "[Note](../old.md)\n", nil,
		[]database.Document{{Path: "archive/new.md"}},
		func(path string) (string, bool) { return "archive/new.md", path == "old.md" })
	if !ok || repair.ProposedContent != "[Note](../archive/new.md)\n" {
		t.Fatalf("body-only repair=%+v ok=%v", repair, ok)
	}
}

func TestVerifiedDeterministicRepairAppliesEndToEnd(t *testing.T) {
	c, db := taskSetup(t)
	writeDoc(t, c.RepositoryPath, "old.md", "# Destination\n")
	writeDoc(t, c.RepositoryPath, "sub/note.md",
		"---\ntags: [go]\nrelated: [old.md]\n---\n# Note\n\n[Destination](../old.md#part)\n\n`[example](../old.md)`\n")
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(c.RepositoryPath, "archive"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(c.RepositoryPath, "old.md"), filepath.Join(c.RepositoryPath, "archive/new.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	known, err := db.Documents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	content, _ := scanner.ReadDocument(c, "sub/note.md")
	fields, _, _ := frontmatter.Parse(content)
	s, ok := deterministicFix("sub/note.md", content, fields, known,
		func(path string) (string, bool) { return db.RetargetPath(t.Context(), path) })
	if !ok || !strings.Contains(s.ProposedContent, "](../archive/new.md#part)") {
		t.Fatalf("expected document-relative repair: %+v", s)
	}
	doc := docByPath(t, db, "sub/note.md")
	if err := db.SaveLinkFix(t.Context(), doc, []database.Suggestion{s}); err != nil {
		t.Fatal(err)
	}
	rows, _ := db.PendingSuggestions(t.Context())
	if len(rows) != 1 {
		t.Fatalf("pending=%+v", rows)
	}
	if err := db.Decide(t.Context(), rows[0].ID, "accepted", ""); err != nil {
		t.Fatal(err)
	}
	results, err := review.Apply(t.Context(), c, db)
	if err != nil || len(results) != 1 || !results[0].Applied {
		t.Fatalf("apply=%+v err=%v", results, err)
	}
	got, _ := os.ReadFile(filepath.Join(c.RepositoryPath, "sub/note.md"))
	if !strings.Contains(string(got), "`[example](../old.md)`") {
		t.Fatalf("code changed: %s", got)
	}
}
