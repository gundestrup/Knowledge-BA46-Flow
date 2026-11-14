package database

import (
	"testing"
)

func TestSearchCandidateRanksFollowSourceVersion(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, t.TempDir()+"/search.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.IndexSearchDocument(ctx, "quartz.md", "v1", "Quartz Widget", "Tidal gauge", "fieldwork", "Quartz widgets measure harbor currents."); err != nil {
		t.Fatal(err)
	}
	if err := db.IndexSearchDocument(ctx, "garden.md", "v1", "Garden", "", "", "Raised beds and spring vegetables."); err != nil {
		t.Fatal(err)
	}
	if err := db.IndexSearchDocument(ctx, "quartz-two.md", "v1", "Quartz Widget Field Manual", "", "", "A second guide for quartz instruments."); err != nil {
		t.Fatal(err)
	}
	ranks, err := db.SearchCandidateRanks(ctx, "quartz widget harbor currents", "query.md", 8)
	if err != nil {
		t.Fatal(err)
	}
	if ranks["quartz.md"] == 0 || ranks["garden.md"] != 0 {
		t.Fatalf("lexical candidates=%v, want quartz only", ranks)
	}
	limited, err := db.SearchCandidateRanks(ctx, "quartz widget", "query.md", 1)
	if err != nil || len(limited) != 1 {
		t.Fatalf("candidate limit not respected: ranks=%v err=%v", limited, err)
	}
	excluded, err := db.SearchCandidateRanks(ctx, "quartz widget", "quartz.md", 8)
	if err != nil || excluded["quartz.md"] != 0 {
		t.Fatalf("current document was not excluded: ranks=%v err=%v", excluded, err)
	}

	// The source hash is the update key; unchanged scans do not churn FTS rows.
	if err := db.IndexSearchDocument(ctx, "quartz.md", "v1", "Garden", "", "", "Changed text."); err != nil {
		t.Fatal(err)
	}
	ranks, err = db.SearchCandidateRanks(ctx, "quartz widget", "query.md", 8)
	if err != nil || ranks["quartz.md"] == 0 {
		t.Fatalf("same-version scan unexpectedly replaced lexical row ranks=%v err=%v", ranks, err)
	}

	if err := db.IndexSearchDocument(ctx, "quartz.md", "v2", "Garden", "", "", "Raised beds and spring vegetables."); err != nil {
		t.Fatal(err)
	}
	ranks, err = db.SearchCandidateRanks(ctx, "quartz widget", "query.md", 8)
	if err != nil {
		t.Fatal(err)
	}
	if ranks["quartz.md"] != 0 {
		t.Fatalf("stale lexical terms remained after source update: %v", ranks)
	}
	ranks, err = db.SearchCandidateRanks(ctx, "spring vegetables", "query.md", 8)
	if err != nil || ranks["quartz.md"] == 0 {
		t.Fatalf("updated lexical terms missing ranks=%v err=%v", ranks, err)
	}
}

func TestSearchIndexFallsBackWhenFTSTableIsUnavailable(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, t.TempDir()+"/search.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP TABLE document_fts`); err != nil {
		t.Fatal(err)
	}
	if err := db.IndexSearchDocument(ctx, "note.md", "v1", "Title", "", "", "Body text."); err != nil {
		t.Fatalf("indexing should degrade safely without FTS5: %v", err)
	}
	ranks, err := db.SearchCandidateRanks(ctx, "title body", "query.md", 8)
	if err != nil || len(ranks) != 0 {
		t.Fatalf("search fallback ranks=%v err=%v", ranks, err)
	}
}
