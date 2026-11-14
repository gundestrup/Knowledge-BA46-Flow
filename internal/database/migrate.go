// Schema bootstrap and versioned upgrades. migrate() runs on Open and is
// idempotent; each upgrade* helper adds one feature's shape to databases
// created before it landed.
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// migrate creates the schema on first run (idempotent via IF NOT EXISTS).
// documents.status cycles through: pending -> analyzed | error | missing.
// suggestions.status cycles through: pending -> accepted | rejected ->
// applied | failed; 'superseded' means the source doc changed under it.
// The FK on suggestions.document_path uses ON UPDATE/DELETE CASCADE so renamed
// documents keep their suggestions and deleted rows take suggestions with them.
// schemaVersion stamps the shape migrate() produces — bump it on every schema
// change. A database stamped newer than this build is refused at Open so an
// old binary cannot silently half-migrate a future layout.
const schemaVersion = 20

func (d *DB) migrate(ctx context.Context) error {
	var stamped int
	if err := d.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&stamped); err != nil {
		return err
	}
	if stamped > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than this build supports (%d); upgrade KBM-flow", stamped, schemaVersion)
	}
	_, err := d.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS documents (
 path TEXT PRIMARY KEY,
 sha256 TEXT NOT NULL,
 analyzed_sha256 TEXT NOT NULL DEFAULT '',
 analyzed_model TEXT NOT NULL DEFAULT '',
 analyzed_semantic_sha256 TEXT NOT NULL DEFAULT '',
 external_sha256 TEXT NOT NULL DEFAULT '',
 external_model TEXT NOT NULL DEFAULT '',
 external_semantic_sha256 TEXT NOT NULL DEFAULT '',
 enriched_sha256 TEXT NOT NULL DEFAULT '',
 enriched_model TEXT NOT NULL DEFAULT '',
 sig BLOB,
 embedding BLOB,
 size INTEGER NOT NULL,
 modified_unix INTEGER NOT NULL,
 title TEXT NOT NULL DEFAULT '',
 summary TEXT NOT NULL DEFAULT '',
 tags_json TEXT NOT NULL DEFAULT '[]',
 status TEXT NOT NULL DEFAULT 'pending',
 last_error TEXT NOT NULL DEFAULT '',
 first_seen_at TEXT NOT NULL,
 last_seen_at TEXT NOT NULL,
 analyzed_at TEXT,
 frontmatter_json TEXT NOT NULL DEFAULT '',
 frontmatter_issues TEXT NOT NULL DEFAULT '[]',
 link_issues TEXT NOT NULL DEFAULT '[]',
	fm_sha256 TEXT NOT NULL DEFAULT '',
	body_sha256 TEXT NOT NULL DEFAULT '',
 semantic_sha256 TEXT NOT NULL DEFAULT '',
 semantic_frontmatter_sha256 TEXT NOT NULL DEFAULT '',
 semantic_body_sha256 TEXT NOT NULL DEFAULT '',
 analysis_input_sha256 TEXT NOT NULL DEFAULT '',
 enrichment_input_sha256 TEXT NOT NULL DEFAULT '',
 expand_input_sha256 TEXT NOT NULL DEFAULT '',
 freshness_input_sha256 TEXT NOT NULL DEFAULT '',
 freshness_web_verify INTEGER NOT NULL DEFAULT 0,
 freshness_verify_model TEXT NOT NULL DEFAULT '',
 freshness_verify_prompt_sha256 TEXT NOT NULL DEFAULT '',
 seen_fm_sha256 TEXT NOT NULL DEFAULT '',
 seen_body_sha256 TEXT NOT NULL DEFAULT '',
 expand_sha256 TEXT NOT NULL DEFAULT '',
 expand_model TEXT NOT NULL DEFAULT '',
 freshness_sha256 TEXT NOT NULL DEFAULT '',
 freshness_model TEXT NOT NULL DEFAULT '',
 embedded_sha256 TEXT NOT NULL DEFAULT '',
 embedded_model TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS evidence_state (
 id INTEGER PRIMARY KEY CHECK(id=1),
 -- Historical column name; value advances for deletion and reference changes.
 removal_generation INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO evidence_state(id,removal_generation) VALUES(1,0);
CREATE TABLE IF NOT EXISTS suggestions (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 document_path TEXT NOT NULL,
 source_sha256 TEXT NOT NULL,
 action TEXT NOT NULL CHECK(action IN ('none','move','merge','expand','delete')),
 target_path TEXT NOT NULL DEFAULT '',
 reason TEXT NOT NULL,
 confidence REAL NOT NULL DEFAULT 0,
 proposed_content TEXT NOT NULL DEFAULT '',
 origin TEXT NOT NULL DEFAULT 'analysis' CHECK(origin IN ('analysis','enrich','deterministic','expand','freshness','identity')),
 status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','accepted','rejected','applied','failed','superseded')),
 created_at TEXT NOT NULL,
 decided_at TEXT,
 decision_note TEXT NOT NULL DEFAULT '',
 applied_at TEXT,
 error TEXT NOT NULL DEFAULT '',
 FOREIGN KEY(document_path) REFERENCES documents(path) ON UPDATE CASCADE ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_documents_status ON documents(status);
CREATE INDEX IF NOT EXISTS idx_suggestions_status ON suggestions(status);
CREATE TABLE IF NOT EXISTS renames (
 old_path TEXT PRIMARY KEY,
 new_path TEXT NOT NULL,
 moved_at TEXT NOT NULL
);
-- applied_edges records graph edges a human added by accepting a suggestion.
-- The source hash scopes that provenance to the exact applied source version.
CREATE TABLE IF NOT EXISTS applied_edges (
 source_path TEXT NOT NULL,
 target_path TEXT NOT NULL,
 kind TEXT NOT NULL,
 source_sha256 TEXT NOT NULL DEFAULT '',
 applied_at TEXT NOT NULL,
 PRIMARY KEY(source_path, target_path, kind)
);
CREATE TABLE IF NOT EXISTS jobs (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 op TEXT NOT NULL CHECK(op IN ('scan','analyze','run','report','link-check','enrich','embed','expand','freshness','all')),
 status TEXT NOT NULL DEFAULT 'running' CHECK(status IN ('running','done','failed')),
 detail TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL,
 finished_at TEXT,
 heartbeat_at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS repo_report_snapshot (
 id INTEGER PRIMARY KEY CHECK(id = 1),
 generated_at TEXT NOT NULL,
 data_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS repo_report_history (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 generated_at TEXT NOT NULL,
 summary_json TEXT NOT NULL
);
`)
	if err != nil {
		return err
	}
	if err := d.upgradeJobsForReport(ctx); err != nil {
		return err
	}
	if err := d.upgradeJobsHeartbeat(ctx); err != nil {
		return err
	}
	if err := d.upgradeJobsForLinkCheck(ctx); err != nil {
		return err
	}
	if err := d.upgradeJobsForTasks(ctx); err != nil {
		return err
	}
	if err := d.upgradeDocumentColumns(ctx); err != nil {
		return err
	}
	// upgradeTaskConstraints copies every column, so it must run only after
	// the additive column upgrades above have filled any gaps.
	if err := d.upgradeSuggestionColumns(ctx); err != nil {
		return err
	}
	if err := d.upgradeApplyColumns(ctx); err != nil {
		return err
	}
	if err := d.upgradeTaskConstraints(ctx); err != nil {
		return err
	}
	if _, err := d.ExecContext(ctx, stageResultsSchema); err != nil {
		return err
	}
	if _, err := d.ExecContext(ctx, queueSchema); err != nil {
		return err
	}
	if err := d.ensureFTS(ctx); err != nil {
		return err
	}
	if err := d.upgradeIdentitySchema(ctx); err != nil {
		return err
	}
	if err := d.upgradeAppliedEdges(ctx); err != nil {
		return err
	}
	// v11: decided_at means "left the review queue" — rows superseded or failed
	// before that column existed get a synthetic timestamp so the progress
	// graph's backlog series (created − handled) stops counting them open.
	// created_at approximates the supersede day for bulk pipeline runs.
	if stamped < 11 {
		if _, err := d.ExecContext(ctx, `UPDATE suggestions SET decided_at=created_at WHERE decided_at IS NULL AND status<>'pending'`); err != nil {
			return err
		}
	}
	_, err = d.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion))
	return err
}

// upgradeAppliedEdges adds source-version scoping to v18 provenance rows.
// Legacy rows deliberately keep an empty hash and therefore cannot suppress
// invalidation; reconstructing their origin from current files is not provable.
func (d *DB) upgradeAppliedEdges(ctx context.Context) error {
	rows, err := d.QueryContext(ctx, `SELECT name FROM pragma_table_info('applied_edges')`)
	if err != nil {
		return err
	}
	hasSourceHash := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return err
		}
		if name == "source_sha256" {
			hasSourceHash = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if hasSourceHash {
		return nil
	}
	_, err = d.ExecContext(ctx, `ALTER TABLE applied_edges ADD COLUMN source_sha256 TEXT NOT NULL DEFAULT ''`)
	return err
}

// upgradeJobsHeartbeat adds the heartbeat column to databases created before
// cross-process op guarding. Rows without a heartbeat count as dead runners.
func (d *DB) upgradeJobsHeartbeat(ctx context.Context) error {
	var present bool
	rows, err := d.QueryContext(ctx, `SELECT name FROM pragma_table_info('jobs')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == "heartbeat_at" {
			present = true
		}
	}
	if err := rows.Err(); err != nil || present {
		return err
	}
	_, err = d.ExecContext(ctx, `ALTER TABLE jobs ADD COLUMN heartbeat_at TEXT NOT NULL DEFAULT ''`)
	return err
}

// upgradeDocumentColumns adds columns to databases created before their
// features landed. ALTER TABLE ADD COLUMN is a metadata-only change in
// SQLite — existing rows get the column default.
func (d *DB) upgradeDocumentColumns(ctx context.Context) error {
	rows, err := d.QueryContext(ctx, `SELECT name FROM pragma_table_info('documents')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, stmt := range []struct {
		col string
		ddl string
	}{
		{"frontmatter_json", `ALTER TABLE documents ADD COLUMN frontmatter_json TEXT NOT NULL DEFAULT ''`},
		{"frontmatter_issues", `ALTER TABLE documents ADD COLUMN frontmatter_issues TEXT NOT NULL DEFAULT '[]'`},
		{"link_issues", `ALTER TABLE documents ADD COLUMN link_issues TEXT NOT NULL DEFAULT '[]'`},
		{"analyzed_model", `ALTER TABLE documents ADD COLUMN analyzed_model TEXT NOT NULL DEFAULT ''`},
		{"analyzed_semantic_sha256", `ALTER TABLE documents ADD COLUMN analyzed_semantic_sha256 TEXT NOT NULL DEFAULT ''`},
		{"external_sha256", `ALTER TABLE documents ADD COLUMN external_sha256 TEXT NOT NULL DEFAULT ''`},
		{"external_model", `ALTER TABLE documents ADD COLUMN external_model TEXT NOT NULL DEFAULT ''`},
		{"external_semantic_sha256", `ALTER TABLE documents ADD COLUMN external_semantic_sha256 TEXT NOT NULL DEFAULT ''`},
		{"enriched_sha256", `ALTER TABLE documents ADD COLUMN enriched_sha256 TEXT NOT NULL DEFAULT ''`},
		{"enriched_model", `ALTER TABLE documents ADD COLUMN enriched_model TEXT NOT NULL DEFAULT ''`},
		{"sig", `ALTER TABLE documents ADD COLUMN sig BLOB`},
		{"embedding", `ALTER TABLE documents ADD COLUMN embedding BLOB`},
		{"fm_sha256", `ALTER TABLE documents ADD COLUMN fm_sha256 TEXT NOT NULL DEFAULT ''`},
		{"body_sha256", `ALTER TABLE documents ADD COLUMN body_sha256 TEXT NOT NULL DEFAULT ''`},
		{"semantic_sha256", `ALTER TABLE documents ADD COLUMN semantic_sha256 TEXT NOT NULL DEFAULT ''`},
		{"semantic_frontmatter_sha256", `ALTER TABLE documents ADD COLUMN semantic_frontmatter_sha256 TEXT NOT NULL DEFAULT ''`},
		{"semantic_body_sha256", `ALTER TABLE documents ADD COLUMN semantic_body_sha256 TEXT NOT NULL DEFAULT ''`},
		{"analysis_input_sha256", `ALTER TABLE documents ADD COLUMN analysis_input_sha256 TEXT NOT NULL DEFAULT ''`},
		{"enrichment_input_sha256", `ALTER TABLE documents ADD COLUMN enrichment_input_sha256 TEXT NOT NULL DEFAULT ''`},
		{"expand_input_sha256", `ALTER TABLE documents ADD COLUMN expand_input_sha256 TEXT NOT NULL DEFAULT ''`},
		{"freshness_input_sha256", `ALTER TABLE documents ADD COLUMN freshness_input_sha256 TEXT NOT NULL DEFAULT ''`},
		{"freshness_web_verify", `ALTER TABLE documents ADD COLUMN freshness_web_verify INTEGER NOT NULL DEFAULT 0`},
		{"freshness_verify_model", `ALTER TABLE documents ADD COLUMN freshness_verify_model TEXT NOT NULL DEFAULT ''`},
		{"freshness_verify_prompt_sha256", `ALTER TABLE documents ADD COLUMN freshness_verify_prompt_sha256 TEXT NOT NULL DEFAULT ''`},
		{"seen_fm_sha256", `ALTER TABLE documents ADD COLUMN seen_fm_sha256 TEXT NOT NULL DEFAULT ''`},
		{"seen_body_sha256", `ALTER TABLE documents ADD COLUMN seen_body_sha256 TEXT NOT NULL DEFAULT ''`},
		{"analysis_evidence_json", `ALTER TABLE documents ADD COLUMN analysis_evidence_json TEXT NOT NULL DEFAULT ''`},
		{"external_analysis_evidence_json", `ALTER TABLE documents ADD COLUMN external_analysis_evidence_json TEXT NOT NULL DEFAULT ''`},
		{"seen_external_fm_sha256", `ALTER TABLE documents ADD COLUMN seen_external_fm_sha256 TEXT NOT NULL DEFAULT ''`},
		{"seen_external_body_sha256", `ALTER TABLE documents ADD COLUMN seen_external_body_sha256 TEXT NOT NULL DEFAULT ''`},
		{"seen_removal_generation", `ALTER TABLE documents ADD COLUMN seen_removal_generation INTEGER NOT NULL DEFAULT 0`},
		{"seen_external_removal_generation", `ALTER TABLE documents ADD COLUMN seen_external_removal_generation INTEGER NOT NULL DEFAULT 0`},
		{"missing_since", `ALTER TABLE documents ADD COLUMN missing_since TEXT NOT NULL DEFAULT ''`},
		{"missing_generation", `ALTER TABLE documents ADD COLUMN missing_generation INTEGER NOT NULL DEFAULT 0`},
		{"expand_sha256", `ALTER TABLE documents ADD COLUMN expand_sha256 TEXT NOT NULL DEFAULT ''`},
		{"expand_model", `ALTER TABLE documents ADD COLUMN expand_model TEXT NOT NULL DEFAULT ''`},
		{"freshness_sha256", `ALTER TABLE documents ADD COLUMN freshness_sha256 TEXT NOT NULL DEFAULT ''`},
		{"freshness_model", `ALTER TABLE documents ADD COLUMN freshness_model TEXT NOT NULL DEFAULT ''`},
		{"embedded_sha256", `ALTER TABLE documents ADD COLUMN embedded_sha256 TEXT NOT NULL DEFAULT ''`},
		{"embedded_model", `ALTER TABLE documents ADD COLUMN embedded_model TEXT NOT NULL DEFAULT ''`},
	} {
		if !cols[stmt.col] {
			if _, err := d.ExecContext(ctx, stmt.ddl); err != nil {
				return err
			}
		}
	}
	// Prior schema versions recorded the last lane's observed hashes/evidence
	// in the shared columns. Preserve the best available baseline when adding
	// the external-specific fields; this avoids treating every historically
	// external-analyzed row as a never-seen seed on upgrade.
	if _, err := d.ExecContext(ctx, `UPDATE documents
SET seen_external_fm_sha256=seen_fm_sha256,
    seen_external_body_sha256=seen_body_sha256
WHERE external_sha256<>'' AND seen_external_fm_sha256=''`); err != nil {
		return err
	}
	if _, err := d.ExecContext(ctx, `UPDATE documents
SET external_analysis_evidence_json=analysis_evidence_json
WHERE external_sha256<>'' AND external_analysis_evidence_json=''`); err != nil {
		return err
	}
	return nil
}

// upgradeSuggestionColumns adds columns to databases created before their
// features landed: origin (two-pass analyzer; pre-existing rows are
// LLM-produced so 'analysis' is the right backfill default) plus the
// decision/apply bookkeeping columns from the review workflow.
func (d *DB) upgradeSuggestionColumns(ctx context.Context) error {
	rows, err := d.QueryContext(ctx, `SELECT name FROM pragma_table_info('suggestions')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, stmt := range []struct {
		col string
		ddl string
	}{
		{"origin", `ALTER TABLE suggestions ADD COLUMN origin TEXT NOT NULL DEFAULT 'analysis'`},
		{"decided_at", `ALTER TABLE suggestions ADD COLUMN decided_at TEXT`},
		{"decision_note", `ALTER TABLE suggestions ADD COLUMN decision_note TEXT NOT NULL DEFAULT ''`},
		{"applied_at", `ALTER TABLE suggestions ADD COLUMN applied_at TEXT`},
		{"error", `ALTER TABLE suggestions ADD COLUMN error TEXT NOT NULL DEFAULT ''`},
	} {
		if !cols[stmt.col] {
			if _, err := d.ExecContext(ctx, stmt.ddl); err != nil {
				return err
			}
		}
	}
	return nil
}

// upgradeJobsForReport rebuilds the original jobs table to extend its op
// constraint while preserving every existing job and its id. The heartbeat
// column is added afterwards by upgradeJobsHeartbeat.
func (d *DB) upgradeJobsForReport(ctx context.Context) error {
	return d.rebuildJobsForOp(ctx, "'report'", "'scan','analyze','run','report'", false)
}

// upgradeJobsForLinkCheck rebuilds the jobs table to admit the 'link-check'
// op while preserving every existing job, its id and its heartbeat. Runs
// after upgradeJobsHeartbeat so the copy always carries that column.
func (d *DB) upgradeJobsForLinkCheck(ctx context.Context) error {
	return d.rebuildJobsForOp(ctx, "'link-check'", "'scan','analyze','run','report','link-check'", true)
}

// upgradeJobsForTasks admits the per-task ops: an optional fast-model
// enrichment pass, embedding refresh, stub expansion and the external-lane
// freshness check.
func (d *DB) upgradeJobsForTasks(ctx context.Context) error {
	return d.rebuildJobsForOp(ctx, "'expand'",
		"'scan','analyze','run','report','link-check','enrich','embed','expand','freshness','all'", true)
}

// rebuildJobsForOp recreates the jobs table when its CHECK constraint does
// not admit marker. withHeartbeat copies the column upgradeJobsHeartbeat
// adds; the earliest rebuild predates it.
func (d *DB) rebuildJobsForOp(ctx context.Context, marker, opCSV string, withHeartbeat bool) error {
	ddl := `CREATE TABLE jobs_rebuild (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 op TEXT NOT NULL CHECK(op IN (` + opCSV + `)),
 status TEXT NOT NULL DEFAULT 'running' CHECK(status IN ('running','done','failed')),
 detail TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL,
 finished_at TEXT);`
	cols := "id,op,status,detail,error,created_at,finished_at"
	if withHeartbeat {
		ddl = strings.Replace(ddl, "finished_at TEXT);", "finished_at TEXT,\n heartbeat_at TEXT NOT NULL DEFAULT '');", 1)
		cols += ",heartbeat_at"
	}
	return d.rebuildTable(ctx, "jobs", marker, ddl, cols, "")
}

// upgradeTaskConstraints widens the suggestions.origin and stage_results.stage
// CHECK constraints to cover the expand/freshness/embed tasks. 'freshness' is
// the marker: it appears in neither legacy CHECK, while 'expand' already sits
// in the suggestions.action list.
func (d *DB) upgradeTaskConstraints(ctx context.Context) error {
	err := d.rebuildTable(ctx, "suggestions", "'identity'", `CREATE TABLE suggestions_rebuild (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 document_path TEXT NOT NULL,
 source_sha256 TEXT NOT NULL,
 action TEXT NOT NULL CHECK(action IN ('none','move','merge','expand','delete')),
 target_path TEXT NOT NULL DEFAULT '',
 reason TEXT NOT NULL,
 confidence REAL NOT NULL DEFAULT 0,
 proposed_content TEXT NOT NULL DEFAULT '',
 origin TEXT NOT NULL DEFAULT 'analysis' CHECK(origin IN ('analysis','enrich','deterministic','expand','freshness','identity')),
 status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','accepted','rejected','applied','failed','superseded')),
 created_at TEXT NOT NULL,
 decided_at TEXT,
 decision_note TEXT NOT NULL DEFAULT '',
 applied_at TEXT,
 error TEXT NOT NULL DEFAULT '',
 apply_batch_id TEXT NOT NULL DEFAULT '',
 apply_attempted_at TEXT NOT NULL DEFAULT '',
 FOREIGN KEY(document_path) REFERENCES documents(path) ON UPDATE CASCADE ON DELETE CASCADE
);`, "id,document_path,source_sha256,action,target_path,reason,confidence,proposed_content,origin,status,created_at,decided_at,decision_note,applied_at,error,apply_batch_id,apply_attempted_at",
		`CREATE INDEX IF NOT EXISTS idx_suggestions_status ON suggestions(status);`)
	if err != nil {
		return err
	}
	return d.rebuildTable(ctx, "stage_results", "'freshness'", `CREATE TABLE stage_results_rebuild (
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
);`, "id,document_path,source_sha256,stage,model,outcome,patch_outcome,patch_json,derived_fields_json,metadata_json,suggestions,rejected_json,error,duration_ms,created_at",
		`CREATE INDEX IF NOT EXISTS idx_stage_results_document ON stage_results(document_path,stage,id);`)
}

// rebuildTable recreates a table when its schema lacks marker — SQLite cannot
// alter a CHECK constraint, so the table is rebuilt in one transaction,
// preserving every row and id, then post SQL (index recreation) runs in the
// same transaction. All SQL fragments are compile-time constants from the
// callers, never user input.
func (d *DB) rebuildTable(ctx context.Context, table, marker, create, cols, post string) error {
	var schema string
	err := d.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&schema)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if strings.Contains(strings.ToLower(schema), marker) {
		return nil
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, create); err != nil {
		return err
	}
	// #nosec G202 -- cols is a compile-time constant from the callers
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+table+`_rebuild(`+cols+`) SELECT `+cols+` FROM `+table); err != nil { // NOSONAR — table/cols are compile-time constants
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE `+table); err != nil { // NOSONAR — table is a compile-time constant
		return err
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE `+table+`_rebuild RENAME TO `+table); err != nil { // NOSONAR — table is a compile-time constant
		return err
	}
	if post != "" {
		if _, err := tx.ExecContext(ctx, post); err != nil {
			return err
		}
	}
	return tx.Commit()
}
