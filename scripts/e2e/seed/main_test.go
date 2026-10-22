package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge-ba46-flow/internal/database"
)

func TestRunSeedsSuggestions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "state.db")

	db, err := database.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"a.md", "b.md", "c.md"} {
		if err := os.WriteFile(filepath.Join(repo, rel), []byte("---\ntitle: doc\n---\nbody\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		doc := database.Document{Path: rel, SHA256: "sha-" + rel, Size: 1, ModifiedUnix: 1, TagsJSON: "[]"}
		if _, err := db.UpsertScanned(ctx, doc, "scan"); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	if err := run(dbPath, repo); err != nil {
		t.Fatal(err)
	}

	db, err = database.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sugs, err := db.PendingSuggestions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sugs) != 3 {
		t.Fatalf("suggestions=%d, want 3", len(sugs))
	}
	origins := map[string]bool{}
	for _, s := range sugs {
		origins[s.Origin] = true
	}
	for _, want := range []string{"analysis", "enrich", "deterministic"} {
		if !origins[want] {
			t.Errorf("no suggestion with origin %q", want)
		}
	}
	// The expand suggestion carries the doc body plus the apply marker.
	var expand *database.Suggestion
	for i := range sugs {
		if sugs[i].Action == "expand" {
			expand = &sugs[i]
		}
	}
	if expand == nil || !strings.Contains(expand.ProposedContent, "apply marker") {
		t.Errorf("expand suggestion missing apply marker: %+v", expand)
	}
}

func TestRunRejectsThinFixture(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	db, err := database.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := run(dbPath, dir); err == nil || !strings.Contains(err.Error(), "at least 3") {
		t.Fatalf("expected thin-fixture error, got %v", err)
	}
}
