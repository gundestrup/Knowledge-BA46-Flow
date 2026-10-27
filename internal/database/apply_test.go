package database

import (
	"path/filepath"
	"testing"
)

func TestApplyHistoryMigrationPreservesLegacySuggestions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	doc := Document{Path: "note.md", SHA256: "source", Size: 1, TagsJSON: "[]"}
	if _, err := db.UpsertScanned(t.Context(), doc, "scan"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(t.Context(), doc, "fake", "", "", nil, []Suggestion{{
		Action: "expand", ProposedContent: "# Note\n", Reason: "kept",
	}}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`ALTER TABLE suggestions DROP COLUMN apply_batch_id`,
		`ALTER TABLE suggestions DROP COLUMN apply_attempted_at`,
		`PRAGMA user_version=11`,
	} {
		if _, err := db.ExecContext(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	db, err = Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := db.Suggestion(t.Context(), 1)
	if err != nil || s.Reason != "kept" || s.ProposedContent != "# Note\n" || s.ApplyBatchID != "" {
		t.Fatalf("legacy suggestion was not preserved: %+v %v", s, err)
	}
}

func TestAcceptExclusiveAcrossConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	doc := Document{Path: "note.md", SHA256: "source", Size: 1, TagsJSON: "[]"}
	if _, err := db.UpsertScanned(t.Context(), doc, "scan"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(t.Context(), doc, "fake", "", "", nil, []Suggestion{
		{Action: "expand", ProposedContent: "# First\n"},
		{Action: "expand", ProposedContent: "# Second\n"},
	}); err != nil {
		t.Fatal(err)
	}
	other, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	start, done := make(chan struct{}), make(chan error, 2)
	for i, connection := range []*DB{db, other} {
		go func() {
			<-start
			done <- connection.AcceptExclusive(t.Context(), int64(i+1))
		}()
	}
	close(start)
	accepted := 0
	for range 2 {
		if err := <-done; err == nil {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted %d alternatives, want exactly one", accepted)
	}
}
