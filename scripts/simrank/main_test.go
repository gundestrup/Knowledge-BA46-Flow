package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/similarity"
)

func TestRunRanksSignaturePairs(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := database.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	docs := []struct {
		path string
		text string
	}{
		{"languages/ruby.md", strings.Repeat("ruby rails gem bundler rake ", 30)},
		{"languages/jekyll.md", strings.Repeat("ruby rails gem bundler rake ", 28) + "jekyll static site"},
		{"recipes/bread.md", strings.Repeat("flour water salt yeast oven ", 30)},
	}
	for _, d := range docs {
		doc := database.Document{Path: d.path, SHA256: "sha-" + d.path, Size: 1, ModifiedUnix: 1, TagsJSON: "[]"}
		if _, err := db.UpsertScanned(ctx, doc, "scan"); err != nil {
			t.Fatal(err)
		}
		if err := db.SetSignature(ctx, d.path, similarity.Marshal(similarity.Signature(d.text))); err != nil {
			t.Fatal(err)
		}
	}
	// A doc without a signature must be skipped, not crash the ranking.
	if _, err := db.UpsertScanned(ctx, database.Document{
		Path: "unsigned.md", SHA256: "sha-u", Size: 1, ModifiedUnix: 1, TagsJSON: "[]",
	}, "scan"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var out strings.Builder
	if err := run(dbPath, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"docs with signatures: 3",
		"pairs: 3",
		"languages/jekyll.md  <->  languages/ruby.md",
		"=== nearest to languages/ruby.md ===",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	// The probe list section must not rank the unsigned document or panic on
	// the small corpus (fewer than six neighbours).
	if !strings.Contains(got, "=== nearest to languages/jekyll.md ===") {
		t.Errorf("probe section missing:\n%s", got)
	}
}

func TestRunRejectsUnreadableDB(t *testing.T) {
	// A regular file where a directory belongs fails MkdirAll inside Open.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(filepath.Join(blocker, "state.db"), &strings.Builder{}); err == nil {
		t.Fatal("unopenable database path should fail")
	}
}
