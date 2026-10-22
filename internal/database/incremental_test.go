package database

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestScanInvalidatesEmbeddingOnlyWhenContentChanges(t *testing.T) {
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	doc := Document{Path: "a.md", SHA256: "first", TagsJSON: "[]"}
	if _, err := db.UpsertScanned(t.Context(), doc, "scan1"); err != nil {
		t.Fatal(err)
	}
	vector := []byte{1, 2, 3, 4}
	if err := db.SetEmbedding(t.Context(), doc.Path, vector); err != nil {
		t.Fatal(err)
	}
	if changed, err := db.UpsertScanned(t.Context(), doc, "scan2"); err != nil || changed {
		t.Fatalf("unchanged scan changed=%v error=%v", changed, err)
	}
	docs, err := db.Documents(t.Context())
	if err != nil || !bytes.Equal(docs[0].Embedding, vector) {
		t.Fatalf("unchanged document lost its embedding: %+v error=%v", docs, err)
	}
	doc.SHA256 = "second"
	if changed, err := db.UpsertScanned(t.Context(), doc, "scan3"); err != nil || !changed {
		t.Fatalf("changed scan changed=%v error=%v", changed, err)
	}
	docs, err = db.Documents(t.Context())
	if err != nil || len(docs[0].Embedding) != 0 {
		t.Fatalf("changed document retained a stale embedding: %+v error=%v", docs, err)
	}
}
