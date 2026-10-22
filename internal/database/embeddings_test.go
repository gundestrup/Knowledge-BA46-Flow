package database

import (
	"bytes"
	"path/filepath"
	"testing"
)

func embeddingTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestEmbeddingModelChangesAreAlwaysPending(t *testing.T) {
	db := embeddingTestDB(t)
	doc := Document{Path: "a.md", SHA256: "source", TagsJSON: "[]"}
	if _, err := db.UpsertScanned(t.Context(), doc, "scan"); err != nil {
		t.Fatal(err)
	}
	vector := []byte{0, 0, 128, 63}
	if err := db.SaveEmbedding(t.Context(), doc, "first-model", vector); err != nil {
		t.Fatal(err)
	}
	current, err := db.PendingTask(t.Context(), "embed", "first-model", false)
	if err != nil || len(current) != 0 {
		t.Fatalf("current vector is pending: %+v error=%v", current, err)
	}
	changed, err := db.PendingTask(t.Context(), "embed", "second-model", false)
	if err != nil || len(changed) != 1 || !bytes.Equal(changed[0].Embedding, vector) {
		t.Fatalf("model switch needs a new vector without --reanalyze: %+v error=%v", changed, err)
	}
}

func TestSaveEmbeddingRollsBackVectorAndLineageTogether(t *testing.T) {
	db := embeddingTestDB(t)
	doc := Document{Path: "a.md", SHA256: "source", TagsJSON: "[]"}
	if _, err := db.UpsertScanned(t.Context(), doc, "scan"); err != nil {
		t.Fatal(err)
	}
	original := []byte{0, 0, 128, 63}
	if err := db.SaveEmbedding(t.Context(), doc, "first-model", original); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `CREATE TRIGGER reject_embedding_audit
BEFORE INSERT ON stage_results WHEN NEW.stage='embed'
BEGIN SELECT RAISE(ABORT,'audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveEmbedding(t.Context(), doc, "second-model", []byte{0, 0, 0, 0}); err == nil {
		t.Fatal("audit failure should abort the whole embedding save")
	}
	docs, err := db.Documents(t.Context())
	if err != nil || docs[0].EmbeddedModel != "first-model" || docs[0].EmbeddedSHA256 != doc.SHA256 ||
		!bytes.Equal(docs[0].Embedding, original) {
		t.Fatalf("partial embedding update escaped rollback: %+v error=%v", docs, err)
	}
}

func TestSaveEmbeddingRefusesAStaleSourceHash(t *testing.T) {
	db := embeddingTestDB(t)
	doc := Document{Path: "a.md", SHA256: "original", TagsJSON: "[]"}
	if _, err := db.UpsertScanned(t.Context(), doc, "scan1"); err != nil {
		t.Fatal(err)
	}
	changed := doc
	changed.SHA256 = "changed"
	if _, err := db.UpsertScanned(t.Context(), changed, "scan2"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveEmbedding(t.Context(), doc, "model", []byte{0, 0, 128, 63}); err == nil {
		t.Fatal("old content must not stamp a newer document")
	}
	docs, err := db.Documents(t.Context())
	if err != nil || len(docs[0].Embedding) != 0 || docs[0].EmbeddedModel != "" {
		t.Fatalf("stale vector persisted: %+v error=%v", docs, err)
	}
}
