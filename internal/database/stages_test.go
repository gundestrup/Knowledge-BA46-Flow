package database

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestStageAuditCommitsAtomicallyWithPass(t *testing.T) {
	for _, stage := range []string{"enrich", "analysis"} {
		t.Run(stage, func(t *testing.T) {
			db, err := Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			doc := Document{Path: "a.md", SHA256: "hash", TagsJSON: "[]"}
			if _, err := db.UpsertScanned(t.Context(), doc, "scan"); err != nil {
				t.Fatal(err)
			}
			save := db.SaveAnalysis
			if stage == "enrich" {
				save = db.SaveEnrichment
			}
			audit := StageResult{Outcome: "invalid", PatchOutcome: "suggested"}
			suggestions := []Suggestion{{Action: "expand", Origin: stage}}
			err = save(t.Context(), doc, "model", "Title", "Summary", []string{"tag"}, suggestions, audit)
			if err == nil {
				t.Fatal("invalid audit outcome should roll back the entire pass")
			}
			docs, err := db.Documents(t.Context())
			if err != nil || docs[0].Title != "" || docs[0].AnalyzedSHA256 != "" || docs[0].EnrichedSHA256 != "" {
				t.Fatalf("partial metadata commit: %+v error=%v", docs, err)
			}
			pending, err := db.PendingSuggestions(t.Context())
			if err != nil || len(pending) != 0 {
				t.Fatalf("partial suggestion commit: %+v error=%v", pending, err)
			}
			audit.Outcome = "success"
			audit.Patch = json.RawMessage(`{"set":{"title":"Title"},"remove":[]}`)
			audit.DerivedFields = []string{"tags"}
			audit.Rejected = []string{"protected field"}
			if err := save(t.Context(), doc, "model", "Title", "Summary", []string{"tag"}, suggestions, audit); err != nil {
				t.Fatal(err)
			}
			var gotStage, outcome, patch, derived, rejected, metadata, hash string
			err = db.QueryRow(`SELECT stage,outcome,patch_json,derived_fields_json,rejected_json,metadata_json,source_sha256 FROM stage_results`).
				Scan(&gotStage, &outcome, &patch, &derived, &rejected, &metadata, &hash)
			if err != nil || gotStage != stage || outcome != "success" || derived != `["tags"]` || rejected != `["protected field"]` || hash != doc.SHA256 {
				t.Fatalf("stage audit=%s %s %s %s %s error=%v", gotStage, outcome, derived, rejected, hash, err)
			}
			if !json.Valid([]byte(patch)) || !json.Valid([]byte(metadata)) {
				t.Fatalf("invalid audit JSON: %s %s", patch, metadata)
			}
			if err := db.SaveStageError(t.Context(), doc, stage, "model", "timeout", time.Second); err != nil {
				t.Fatal(err)
			}
			var total int
			if err := db.QueryRow(`SELECT count(*) FROM stage_results`).Scan(&total); err != nil || total != 2 {
				t.Fatalf("failure overwrote successful history: total=%d error=%v", total, err)
			}
		})
	}
}

func TestAnalysisKeepsLocalMetadataWhenItOnlySuggestsMoving(t *testing.T) {
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	doc := Document{Path: "languages/ruby.md", SHA256: "hash", TagsJSON: `["languages","ruby"]`}
	if _, err := db.UpsertScanned(t.Context(), doc, "scan"); err != nil {
		t.Fatal(err)
	}
	local := Suggestion{
		DocumentPath: "languages/ruby.md", Action: "expand", Origin: "deterministic",
		Reason:          "Add locally derived title and tags.",
		ProposedContent: "---\ntags: [ruby]\ntitle: Ruby\n---\n# Ruby\n",
	}
	if err := db.SaveEnrichment(t.Context(), doc, "deterministic", "Ruby", "", []string{"ruby"}, []Suggestion{local}); err != nil {
		t.Fatal(err)
	}
	move := Suggestion{
		DocumentPath: "languages/ruby.md", Action: "move", Origin: "analysis",
		TargetPath: "languages/ruby-language.md", Reason: "Move to the canonical location.",
	}
	if err := db.SaveAnalysis(t.Context(), doc, "analysis-model", "Ruby", "", []string{"ruby"}, []Suggestion{move}); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingSuggestions(t.Context())
	if err != nil || len(pending) != 2 {
		t.Fatalf("move should not erase independent local metadata review: %+v error=%v", pending, err)
	}

	expand := Suggestion{
		DocumentPath: "languages/ruby.md", Action: "expand", Origin: "analysis",
		Reason:          "Consolidated metadata and content proposal.",
		ProposedContent: "---\ntags: [ruby]\ntitle: Ruby\n---\n# Ruby\n\nAdditional evidence.\n",
	}
	if err := db.SaveAnalysis(t.Context(), doc, "analysis-model", "Ruby", "", []string{"ruby"}, []Suggestion{expand},
		StageResult{DerivedFields: []string{"title", "tags"}}); err != nil {
		t.Fatal(err)
	}
	pending, err = db.PendingSuggestions(t.Context())
	if err != nil || len(pending) != 1 || pending[0].Origin != "analysis" {
		t.Fatalf("consolidated content proposal should replace local duplicate: %+v error=%v", pending, err)
	}
}

func TestStageAuditMigrationPreservesLegacyState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`
CREATE TABLE documents (
 path TEXT PRIMARY KEY, sha256 TEXT NOT NULL, analyzed_sha256 TEXT NOT NULL DEFAULT '',
 analyzed_model TEXT NOT NULL DEFAULT '', enriched_sha256 TEXT NOT NULL DEFAULT '',
 enriched_model TEXT NOT NULL DEFAULT '', sig BLOB, embedding BLOB, size INTEGER NOT NULL,
 modified_unix INTEGER NOT NULL, title TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL DEFAULT '',
 tags_json TEXT NOT NULL DEFAULT '[]', status TEXT NOT NULL DEFAULT 'pending',
 last_error TEXT NOT NULL DEFAULT '', first_seen_at TEXT NOT NULL, last_seen_at TEXT NOT NULL,
 analyzed_at TEXT, frontmatter_json TEXT NOT NULL DEFAULT '', frontmatter_issues TEXT NOT NULL DEFAULT '[]'
);
INSERT INTO documents(path,sha256,analyzed_sha256,enriched_sha256,size,modified_unix,title,sig,embedding,first_seen_at,last_seen_at)
VALUES('a.md','hash','hash','hash',123,456,'Retained',x'0102',x'0304','before','before');
CREATE TABLE suggestions (
 id INTEGER PRIMARY KEY AUTOINCREMENT, document_path TEXT NOT NULL, source_sha256 TEXT NOT NULL,
 action TEXT NOT NULL, target_path TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT '',
 confidence REAL NOT NULL DEFAULT 0, proposed_content TEXT NOT NULL DEFAULT '',
 origin TEXT NOT NULL DEFAULT 'analysis', status TEXT NOT NULL DEFAULT 'pending', created_at TEXT NOT NULL
);
INSERT INTO suggestions(document_path,source_sha256,action,proposed_content,created_at)
VALUES('a.md','hash','expand','Keep this proposal','before');
PRAGMA user_version=3;
`)
	if err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	legacy.Close()
	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	docs, err := db.Documents(t.Context())
	if err != nil || len(docs) != 1 || docs[0].Title != "Retained" || docs[0].AnalyzedSHA256 != "hash" || docs[0].EnrichedSHA256 != "hash" || len(docs[0].Embedding) != 2 || len(docs[0].Signature) != 2 {
		t.Fatalf("migration lost populated state: %+v error=%v", docs, err)
	}
	suggestions, err := db.PendingSuggestions(t.Context())
	if err != nil || len(suggestions) != 1 || suggestions[0].ProposedContent != "Keep this proposal" {
		t.Fatalf("migration lost review state: %+v error=%v", suggestions, err)
	}
	var count, version int
	if err := db.QueryRow(`SELECT count(*) FROM stage_results`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("migration fabricated historical stage results: %d error=%v", count, err)
	}
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("migration version=%d error=%v", version, err)
	}
}
