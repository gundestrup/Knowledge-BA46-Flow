package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

const stageResultsSchema = `
CREATE TABLE IF NOT EXISTS stage_results (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 document_path TEXT NOT NULL,
 source_sha256 TEXT NOT NULL,
 stage TEXT NOT NULL CHECK(stage IN ('enrich','analysis','expand','freshness','embed')),
 model TEXT NOT NULL,
 outcome TEXT NOT NULL CHECK(outcome IN ('success','error','skipped')),
 patch_outcome TEXT NOT NULL DEFAULT '',
 patch_json TEXT NOT NULL DEFAULT 'null',
 derived_fields_json TEXT NOT NULL DEFAULT '[]',
 metadata_json TEXT NOT NULL DEFAULT '{}',
 suggestions INTEGER NOT NULL DEFAULT 0,
 rejected_json TEXT NOT NULL DEFAULT '[]',
 error TEXT NOT NULL DEFAULT '',
 duration_ms INTEGER NOT NULL DEFAULT 0,
 created_at TEXT NOT NULL,
 FOREIGN KEY(document_path) REFERENCES documents(path) ON UPDATE CASCADE ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_stage_results_document ON stage_results(document_path,stage,id);
`

// StageResult retains a compact parsed response and its disposition, not the
// prompt or raw document. Success records commit with the pass's suggestions.
// PatchOutcome is missing, empty, unchanged, derived, suggested or rejected;
// derived fields identify metadata computed locally rather than by a model.
type StageResult struct {
	Outcome       string
	PatchOutcome  string
	Patch         json.RawMessage
	Metadata      json.RawMessage
	DerivedFields []string
	Rejected      []string
	Error         string
	DurationMS    int64
}

func insertStageResult(ctx context.Context, tx *sql.Tx, doc Document, stage, model, title, summary string, tags []string, suggestions int, result StageResult) error {
	metadata := result.Metadata
	if len(metadata) == 0 {
		metadata, _ = json.Marshal(struct {
			Title   string   `json:"title"`
			Summary string   `json:"summary"`
			Tags    []string `json:"tags"`
		}{title, summary, tags})
	}
	derived, _ := json.Marshal(append([]string{}, result.DerivedFields...))
	rejected, _ := json.Marshal(append([]string{}, result.Rejected...))
	patch := result.Patch
	if len(patch) == 0 {
		patch = json.RawMessage(`null`)
	}
	if result.Outcome == "" {
		result.Outcome = "success"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO stage_results(
document_path,source_sha256,stage,model,outcome,patch_outcome,patch_json,
derived_fields_json,metadata_json,suggestions,rejected_json,error,duration_ms,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		doc.Path, doc.SHA256, stage, model, result.Outcome, result.PatchOutcome, string(patch),
		string(derived), string(metadata), suggestions, string(rejected), result.Error,
		result.DurationMS, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// SaveStageError preserves failures even when a later stage succeeds and clears
// documents.last_error. It deliberately does not stamp a successful pass hash.
func (d *DB) SaveStageError(ctx context.Context, doc Document, stage, model, message string, elapsed time.Duration) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result := StageResult{Outcome: "error", Error: message, DurationMS: elapsed.Milliseconds()}
	if err := insertStageResult(ctx, tx, doc, stage, model, "", "", nil, 0, result); err != nil {
		return err
	}
	return tx.Commit()
}
