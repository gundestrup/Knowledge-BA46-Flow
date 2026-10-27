package scanner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
)

func identityScanFixture(t *testing.T) (config.Config, *database.DB, func(string, string)) {
	t.Helper()
	c := config.Config{RepositoryPath: t.TempDir(), Extensions: []string{".md"}}
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	write := func(path, raw string) {
		t.Helper()
		absolute := filepath.Join(c.RepositoryPath, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return c, db, write
}

func TestLiveCopyBeforeOriginalDoesNotBecomeRename(t *testing.T) {
	c, db, write := identityScanFixture(t)
	write("z.md", "# Same content\n")
	if _, err := Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	write("a.md", "# Same content\n") // visited before z.md on the next walk
	if _, err := Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	moves, err := db.Renames(t.Context())
	docs, docsErr := db.Documents(t.Context())
	if err != nil || docsErr != nil || len(moves) != 0 || len(docs) != 2 {
		t.Fatalf("copy stole the original: moves=%v docs=%v err=%v/%v", moves, docs, err, docsErr)
	}
}

func TestDuplicateUUIDDisablesRetargetAndReportsConflict(t *testing.T) {
	c, db, write := identityScanFixture(t)
	id := frontmatter.NewDocumentID()
	raw := "---\ndocument_id: \"" + id + "\"\n---\n# Note\n"
	write("old.md", raw)
	if _, err := Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(c.RepositoryPath, "old.md"), filepath.Join(c.RepositoryPath, "new.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	write("copy.md", raw+"\nCopy\n")
	if _, err := Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.RetargetPath(t.Context(), "old.md"); ok {
		t.Fatal("ambiguous UUID was used as rename evidence")
	}
	docs, _ := db.Documents(t.Context())
	for _, doc := range docs {
		if !strings.Contains(doc.LinkIssues, "duplicate document_id") {
			t.Fatalf("duplicate UUID is not visible: %+v", doc)
		}
	}
}

func TestMovedReferrerReportsChangedOutgoingIdentity(t *testing.T) {
	c, db, write := identityScanFixture(t)
	note := func(id, body string) string { return "---\ndocument_id: \"" + id + "\"\n---\n" + body }
	source := note(frontmatter.NewDocumentID(), "# A\n\n[B](b.md)\n")
	write("old/a.md", source)
	write("old/b.md", note(frontmatter.NewDocumentID(), "# Original B\n"))
	write("new/b.md", note(frontmatter.NewDocumentID(), "# Different B\n"))
	if _, err := Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(c.RepositoryPath, "old/a.md"), filepath.Join(c.RepositoryPath, "new/a.md")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := Scan(t.Context(), c, db); err != nil {
			t.Fatal(err)
		}
		docs, _ := db.Documents(t.Context())
		for _, doc := range docs {
			if doc.Path == "new/a.md" && !strings.Contains(doc.LinkIssues, "link identity changed") {
				t.Fatalf("silent change of outgoing meaning: %+v", doc)
			}
		}
	}
	actual, _ := os.ReadFile(filepath.Join(c.RepositoryPath, "new/a.md"))
	if string(actual) != source {
		t.Fatal("scan rewrote outgoing links without review")
	}
}

func TestScanAssignsMissingDocumentIDInPlace(t *testing.T) {
	c, db, write := identityScanFixture(t)
	raw := "---\ntitle: T\n---\n# Note\n"
	write("a.md", raw)
	result, err := Scan(t.Context(), c, db)
	if err != nil || result.IDsAssigned != 1 {
		t.Fatalf("scan=%+v err=%v", result, err)
	}
	live, err := os.ReadFile(filepath.Join(c.RepositoryPath, "a.md"))
	if err != nil {
		t.Fatal(err)
	}
	fields, body, err := frontmatter.Parse(string(live))
	if err != nil {
		t.Fatal(err)
	}
	id, err := frontmatter.DocumentID(fields)
	if err != nil || id == "" || fields["title"] != "T" || body != "# Note\n" {
		t.Fatalf("assigned file malformed: id=%q fields=%v body=%q err=%v", id, fields, body, err)
	}
	// Assignment must never invalidate pending work: the canonical hash the
	// queue tracks is identical before and after insertion.
	if frontmatter.Canonical(string(live)) != frontmatter.Canonical(raw) {
		t.Fatal("document_id assignment changed the canonical hash")
	}
	// Idempotent: a rescan keeps the existing UUID and assigns nothing.
	result, err = Scan(t.Context(), c, db)
	if err != nil || result.IDsAssigned != 0 {
		t.Fatalf("rescan=%+v err=%v", result, err)
	}
	fields, _, _ = frontmatter.Parse(string(mustRead(t, filepath.Join(c.RepositoryPath, "a.md"))))
	kept, _ := frontmatter.DocumentID(fields)
	if kept != id {
		t.Fatalf("rescan replaced the ID: %q -> %q", id, kept)
	}
}

func TestScanSkipsDocumentIDWhenMalformedOrDenied(t *testing.T) {
	c, db, write := identityScanFixture(t)
	bad := "---\n  broken: [unclosed\n---\n# Note\n"
	write("bad.md", bad)
	write("denied/denied.md", "# Plain\n")
	write("denied/.kb-policy.json", `{"mutations":"deny"}`)
	result, err := Scan(t.Context(), c, db)
	if err != nil || result.IDsAssigned != 0 {
		t.Fatalf("scan=%+v err=%v", result, err)
	}
	for path, want := range map[string]string{"bad.md": bad, "denied/denied.md": "# Plain\n"} {
		live, err := os.ReadFile(filepath.Join(c.RepositoryPath, filepath.FromSlash(path)))
		if err != nil || string(live) != want {
			t.Fatalf("%s was written despite the guard: %v", path, err)
		}
	}
	docs, _ := db.Documents(t.Context())
	var found int
	for _, doc := range docs {
		if doc.Path == "bad.md" && strings.Contains(doc.FrontmatterIssues, "malformed") {
			found++
		}
		if doc.Path == "denied/denied.md" && strings.Contains(doc.FrontmatterIssues, "document_id assignment denied") {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("guard issues not recorded: %+v", docs)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
