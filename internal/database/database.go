// Package database owns the SQLite schema and all persistence for documents
// (what is on disk) and suggestions (what the LLM proposed).
// Uses modernc.org/sqlite, a pure-Go SQLite driver — no cgo required.
package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" database/sql driver
)

// DB embeds *sql.DB so callers get the full database/sql API plus the helpers below.
type DB struct{ *sql.DB }

// Document mirrors a row of the documents table: a markdown file plus its
// analysis metadata. AnalyzedSHA256 records which content hash was last sent
// to the LLM and AnalyzedModel which model produced it, so a changed hash OR
// a changed model means "needs (re)analysis" — different models produce
// different suggestions. EnrichedSHA256/EnrichedModel track the cheaper
// enrichment pass (ollama_model_fast) the same way. Signature is a marshaled
// similarity sketch used to rank merge/relation candidates by text overlap;
// Embedding is a marshaled float32 vector from the optional embed model,
// used to rank candidates by topical relatedness.
type Document struct {
	Path           string `json:"path"`
	SHA256         string `json:"sha256"`
	AnalyzedSHA256 string `json:"analyzed_sha256"`
	AnalyzedModel  string `json:"analyzed_model"`
	// ExternalSHA256/ExternalModel form the optional external lane: an
	// enhanced run (ollama_model_external, e.g. a cloud model) stamps these
	// instead of analyzed_*, so the local analysis lineage — and therefore
	// the next normal run's pending/drift view — is left untouched.
	ExternalSHA256 string `json:"external_sha256"`
	ExternalModel  string `json:"external_model"`
	EnrichedSHA256 string `json:"enriched_sha256"`
	EnrichedModel  string `json:"enriched_model"`
	// FmSHA256/BodySHA256 split the canonical hash at the frontmatter
	// boundary: a frontmatter-only edit is distinguishable from a body edit,
	// so freshness checks key on BodySHA256 and the UI can label a changed
	// document as a metadata-only touch.
	FmSHA256   string `json:"fm_sha256"`
	BodySHA256 string `json:"body_sha256"`
	// SeenFmSHA256/SeenBodySHA256 record what the document looked like when
	// the last analysis of either lane ran — the reference for the
	// changed-frontmatter-only vs changed-body breakdown.
	SeenFmSHA256   string `json:"seen_fm_sha256"`
	SeenBodySHA256 string `json:"seen_body_sha256"`
	// Expand/Freshness/Embedded lineage keep the optional LLM task runs
	// incremental the same way the analysis lanes do: expand stamps the
	// canonical hash, freshness the body hash (frontmatter churn is not a
	// freshness concern), embed the hash of the embedded canonical text.
	ExpandSHA256    string `json:"expand_sha256"`
	ExpandModel     string `json:"expand_model"`
	FreshnessSHA256 string `json:"freshness_sha256"`
	FreshnessModel  string `json:"freshness_model"`
	EmbeddedSHA256  string `json:"embedded_sha256"`
	EmbeddedModel   string `json:"embedded_model"`
	Title           string `json:"title"`
	Summary         string `json:"summary"`
	TagsJSON        string `json:"tags_json"`
	Status          string `json:"status"`
	LastError       string `json:"last_error"`
	Size            int64  `json:"size"`
	ModifiedUnix    int64  `json:"modified_unix"`
	Signature       []byte `json:"-"`
	Embedding       []byte `json:"-"`
	// FrontmatterJSON holds the document's parsed YAML frontmatter as a JSON
	// object ('' when absent); FrontmatterIssues holds a JSON array of local
	// validation findings produced during scan.
	FrontmatterJSON   string `json:"frontmatter_json"`
	FrontmatterIssues string `json:"frontmatter_issues"`
	// LinkIssues holds a JSON array of unresolved internal body-link findings,
	// refreshed by every scan — the mandatory, always-local link check.
	LinkIssues string `json:"link_issues"`
}

// Suggestion mirrors a row of the suggestions table: a proposed action
// (move/merge/expand/delete) awaiting human review. Origin records which
// stage produced it: 'analysis' (LLM), 'enrich' (fast LLM) or 'deterministic'
// (computed in Go without a model) — each stage supersedes only the pending
// suggestions it regenerates, so a re-run of one pass can't wipe the other's.
type Suggestion struct {
	ID              int64   `json:"id"`
	DocumentPath    string  `json:"document_path"`
	SourceSHA256    string  `json:"source_sha256,omitempty"`
	Action          string  `json:"action"`
	TargetPath      string  `json:"target_path"`
	Reason          string  `json:"reason"`
	ProposedContent string  `json:"proposed_content,omitempty"`
	Status          string  `json:"status"`
	Confidence      float64 `json:"confidence"`
	Origin          string  `json:"origin"`
	CreatedAt       string  `json:"created_at"`
	// DecisionNote carries the reviewer's "why" on reject/accept decisions —
	// the ground-truth feedback the analysis prompt shows back to the model.
	DecisionNote string `json:"decision_note,omitempty"`
}

// KnowledgeDashboard summarizes current analysis work and the most recent
// successful full run cycle.
type KnowledgeDashboard struct {
	ChangedFiles int `json:"changed_files"`
	// ModelDrift counts documents whose last analysis ran under a model other
	// than the configured one — candidates for an explicit re-analyze run.
	ModelDrift  int    `json:"model_drift"`
	LastCycleAt string `json:"last_cycle_at"`
}

// Open creates the parent directory if needed, opens the SQLite file and runs
// migrations. MaxOpenConns(1) serialises writers; WAL + a busy timeout keep
// reads/writes from tripping over each other.
func Open(ctx context.Context, path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return nil, err
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	raw.SetMaxOpenConns(1)
	d := &DB{raw}
	if _, err = d.ExecContext(ctx, `PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;`); err != nil {
		_ = raw.Close()
		return nil, err
	}
	if err = d.migrate(ctx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return d, nil
}

// migrate creates the schema on first run (idempotent via IF NOT EXISTS).
// documents.status cycles through: pending -> analyzed | error | missing.
// suggestions.status cycles through: pending -> accepted | rejected ->
// applied | failed; 'superseded' means the source doc changed under it.
// The FK on suggestions.document_path uses ON UPDATE/DELETE CASCADE so renamed
// documents keep their suggestions and deleted rows take suggestions with them.
// schemaVersion stamps the shape migrate() produces — bump it on every schema
// change. A database stamped newer than this build is refused at Open so an
// old binary cannot silently half-migrate a future layout.
const schemaVersion = 11

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
 external_sha256 TEXT NOT NULL DEFAULT '',
 external_model TEXT NOT NULL DEFAULT '',
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
 seen_fm_sha256 TEXT NOT NULL DEFAULT '',
 seen_body_sha256 TEXT NOT NULL DEFAULT '',
 expand_sha256 TEXT NOT NULL DEFAULT '',
 expand_model TEXT NOT NULL DEFAULT '',
 freshness_sha256 TEXT NOT NULL DEFAULT '',
 freshness_model TEXT NOT NULL DEFAULT '',
 embedded_sha256 TEXT NOT NULL DEFAULT '',
 embedded_model TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS suggestions (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 document_path TEXT NOT NULL,
 source_sha256 TEXT NOT NULL,
 action TEXT NOT NULL CHECK(action IN ('none','move','merge','expand','delete')),
 target_path TEXT NOT NULL DEFAULT '',
 reason TEXT NOT NULL,
 confidence REAL NOT NULL DEFAULT 0,
 proposed_content TEXT NOT NULL DEFAULT '',
 origin TEXT NOT NULL DEFAULT 'analysis' CHECK(origin IN ('analysis','enrich','deterministic','expand','freshness')),
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
	if err := d.upgradeTaskConstraints(ctx); err != nil {
		return err
	}
	if _, err := d.ExecContext(ctx, stageResultsSchema); err != nil {
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
		{"external_sha256", `ALTER TABLE documents ADD COLUMN external_sha256 TEXT NOT NULL DEFAULT ''`},
		{"external_model", `ALTER TABLE documents ADD COLUMN external_model TEXT NOT NULL DEFAULT ''`},
		{"enriched_sha256", `ALTER TABLE documents ADD COLUMN enriched_sha256 TEXT NOT NULL DEFAULT ''`},
		{"enriched_model", `ALTER TABLE documents ADD COLUMN enriched_model TEXT NOT NULL DEFAULT ''`},
		{"sig", `ALTER TABLE documents ADD COLUMN sig BLOB`},
		{"embedding", `ALTER TABLE documents ADD COLUMN embedding BLOB`},
		{"fm_sha256", `ALTER TABLE documents ADD COLUMN fm_sha256 TEXT NOT NULL DEFAULT ''`},
		{"body_sha256", `ALTER TABLE documents ADD COLUMN body_sha256 TEXT NOT NULL DEFAULT ''`},
		{"seen_fm_sha256", `ALTER TABLE documents ADD COLUMN seen_fm_sha256 TEXT NOT NULL DEFAULT ''`},
		{"seen_body_sha256", `ALTER TABLE documents ADD COLUMN seen_body_sha256 TEXT NOT NULL DEFAULT ''`},
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
	err := d.rebuildTable(ctx, "suggestions", "'freshness'", `CREATE TABLE suggestions_rebuild (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 document_path TEXT NOT NULL,
 source_sha256 TEXT NOT NULL,
 action TEXT NOT NULL CHECK(action IN ('none','move','merge','expand','delete')),
 target_path TEXT NOT NULL DEFAULT '',
 reason TEXT NOT NULL,
 confidence REAL NOT NULL DEFAULT 0,
 proposed_content TEXT NOT NULL DEFAULT '',
 origin TEXT NOT NULL DEFAULT 'analysis' CHECK(origin IN ('analysis','enrich','deterministic','expand','freshness')),
 status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','accepted','rejected','applied','failed','superseded')),
 created_at TEXT NOT NULL,
 decided_at TEXT,
 decision_note TEXT NOT NULL DEFAULT '',
 applied_at TEXT,
 error TEXT NOT NULL DEFAULT '',
 FOREIGN KEY(document_path) REFERENCES documents(path) ON UPDATE CASCADE ON DELETE CASCADE
);`, "id,document_path,source_sha256,action,target_path,reason,confidence,proposed_content,origin,status,created_at,decided_at,decision_note,applied_at,error",
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+table+`_rebuild(`+cols+`) SELECT `+cols+` FROM `+table); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE `+table); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE `+table+`_rebuild RENAME TO `+table); err != nil {
		return err
	}
	if post != "" {
		if _, err := tx.ExecContext(ctx, post); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpsertScanned inserts a freshly scanned file or updates its hash/size/mtime.
// When the sha256 changed (or the row is new): tags are replaced, status resets
// to 'pending', last_error clears, and pending or accepted suggestions for that
// document are marked 'superseded' — they were generated from stale content.
// A new path whose hash matches exactly one not-seen-this-scan row is a
// rename: that row is re-keyed instead (renameByHash) and reports unchanged.
// Returns whether the file was new or changed (i.e. needs re-analysis).
func (d *DB) UpsertScanned(ctx context.Context, doc Document, seen string) (bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var old string
	err = tx.QueryRowContext(ctx, `SELECT sha256 FROM documents WHERE path=?`, doc.Path).Scan(&old)
	newPath := err == sql.ErrNoRows
	changed := newPath || old != doc.SHA256
	if err != nil && !newPath {
		return false, err
	}
	if newPath {
		// No row at this path — identical content elsewhere may be a rename:
		// re-key that row so its analysis lineage, embedding and suggestions
		// (document_path FK cascades) follow instead of re-analyzing.
		renamed, rerr := renameByHash(ctx, tx, doc, seen)
		if rerr != nil {
			return false, rerr
		}
		if renamed {
			changed = false // the entity moved; its content did not change
		}
	}
	// The CASE expressions preserve tags/status/last_error/frontmatter when the
	// hash is unchanged — only the timestamps refresh on an unmodified file.
	_, err = tx.ExecContext(ctx, `INSERT INTO documents(path,sha256,size,modified_unix,tags_json,frontmatter_json,frontmatter_issues,sig,status,first_seen_at,last_seen_at,fm_sha256,body_sha256)
VALUES(?,?,?,?,?,?,?,?,'pending',?,?,?,?)
ON CONFLICT(path) DO UPDATE SET sha256=excluded.sha256,fm_sha256=excluded.fm_sha256,body_sha256=excluded.body_sha256,size=excluded.size,modified_unix=excluded.modified_unix,
tags_json=CASE WHEN documents.sha256<>excluded.sha256 THEN excluded.tags_json ELSE documents.tags_json END,
frontmatter_json=CASE WHEN documents.sha256<>excluded.sha256 THEN excluded.frontmatter_json ELSE documents.frontmatter_json END,
frontmatter_issues=CASE WHEN documents.sha256<>excluded.sha256 THEN excluded.frontmatter_issues ELSE documents.frontmatter_issues END,
sig=CASE WHEN documents.sha256<>excluded.sha256 THEN excluded.sig ELSE documents.sig END,
embedding=CASE WHEN documents.sha256<>excluded.sha256 THEN NULL ELSE documents.embedding END,
last_seen_at=excluded.last_seen_at,
status=CASE WHEN documents.sha256<>excluded.sha256 THEN 'pending' WHEN documents.status='missing' THEN CASE WHEN documents.sha256=documents.analyzed_sha256 THEN 'analyzed' ELSE 'pending' END ELSE documents.status END,
last_error=CASE WHEN documents.sha256<>excluded.sha256 THEN '' ELSE documents.last_error END`,
		doc.Path, doc.SHA256, doc.Size, doc.ModifiedUnix, doc.TagsJSON, doc.FrontmatterJSON, doc.FrontmatterIssues, doc.Signature, seen, seen, doc.FmSHA256, doc.BodySHA256)
	if err != nil {
		return false, err
	}
	if changed {
		if _, err = tx.ExecContext(ctx, `UPDATE suggestions SET status='superseded',decided_at=COALESCE(decided_at,?) WHERE document_path=? AND status IN ('pending','accepted')`, time.Now().UTC().Format(time.RFC3339), doc.Path); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return changed, nil
}

// renameByHash detects a moved file inside upsertScanned's transaction: the
// same canonical hash shows up at a new path while exactly one row carrying
// that hash was not visited this scan. That row is re-keyed — the FK cascade
// re-paths its suggestions — so stamps, embeddings and review decisions
// survive the move. Ambiguous matches (several vanished files with identical
// content) and live duplicates (the old path still exists) return false and
// the caller inserts a fresh row instead.
func renameByHash(ctx context.Context, tx *sql.Tx, doc Document, seen string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT path FROM documents WHERE sha256=? AND path<>? AND last_seen_at<>?`, doc.SHA256, doc.Path, seen)
	if err != nil {
		return false, err
	}
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			_ = rows.Close()
			return false, err
		}
		paths = append(paths, p)
	}
	_ = rows.Close() // rows.Err() below is the meaningful signal
	if err := rows.Err(); err != nil || len(paths) != 1 {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE documents SET path=? WHERE path=?`, doc.Path, paths[0]); err != nil {
		return false, err
	}
	// Remember the move so links elsewhere pointing at the old path can be
	// retargeted deterministically — no model needed. Latest move wins.
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO renames(old_path,new_path,moved_at) VALUES(?,?,?)`, paths[0], doc.Path, seen); err != nil {
		return false, err
	}
	// A live merge into the renamed file follows it — the target names the
	// entity, which now lives at the new path. Move destinations stay: the
	// vacated slot remains a legitimate place to land.
	if _, err := tx.ExecContext(ctx, `UPDATE suggestions SET target_path=? WHERE target_path=? AND action='merge' AND status IN ('pending','accepted')`, doc.Path, paths[0]); err != nil {
		return false, err
	}
	return true, nil
}

// Renames returns every recorded move as old_path → new_path — the prefilter
// map for finding documents whose references need retargeting.
func (d *DB) Renames(ctx context.Context) (map[string]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT old_path,new_path FROM renames`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var o, n string
		if err := rows.Scan(&o, &n); err != nil {
			return nil, err
		}
		out[o] = n
	}
	return out, rows.Err()
}

// RetargetPath resolves where a path went after renames: it follows the
// renames map (a→b→c collapses to the live endpoint) and returns false when
// the trail ends at a missing or unknown document — a deleted file offers no
// retarget. Bounded hops keep a corrupted map from looping forever.
func (d *DB) RetargetPath(ctx context.Context, path string) (string, bool) {
	cur := path
	// Follow the renames map to the end of the chain — intermediate hops no
	// longer exist as document rows (the row was re-keyed onward), so only
	// the final endpoint is checked for liveness. The hop bound protects
	// against a corrupted cyclic map.
	for i := 0; i < 8; i++ {
		var next string
		if err := d.QueryRowContext(ctx, `SELECT new_path FROM renames WHERE old_path=?`, cur).Scan(&next); err != nil {
			break
		}
		cur = next
	}
	var status string
	if err := d.QueryRowContext(ctx, `SELECT status FROM documents WHERE path=?`, cur).Scan(&status); err != nil || status == "missing" {
		return "", false
	}
	return cur, true
}

// SaveLinkFix records deterministic rename-retarget repairs without stamping
// any lineage — a link fix-up is maintenance, not an analysis or enrichment
// pass, so no *_sha256/*_model column moves. Pending deterministic suggestions
// for the document are superseded first: the freshly computed repair replaces
// them (e.g. a retarget replacing a dead-link removal).
func (d *DB) SaveLinkFix(ctx context.Context, doc Document, suggestions []Suggestion) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE suggestions SET status='superseded',decided_at=COALESCE(decided_at,?) WHERE document_path=? AND status='pending' AND origin='deterministic'`, time.Now().UTC().Format(time.RFC3339), doc.Path); err != nil {
		return err
	}
	if err := insertSuggestions(ctx, tx, doc, suggestions); err != nil {
		return err
	}
	return tx.Commit()
}

// SetLinkIssues stores the scan-time internal-link findings for a document —
// a JSON array of strings, '[]' when clean. Column changes are derived state,
// so the update deliberately does not touch hash, status or suggestions.
func (d *DB) SetLinkIssues(ctx context.Context, path, issuesJSON string) error {
	_, err := d.ExecContext(ctx, `UPDATE documents SET link_issues=? WHERE path=?`, issuesJSON, path)
	return err
}

// RefreshFrontmatter replaces the stored parse of a document after a managed
// (kbm-block) file update. Only the parsed metadata changes — sha256 is the
// canonical content hash, which a kbm update cannot change.
func (d *DB) RefreshFrontmatter(ctx context.Context, path, fmJSON, fmIssues string) error {
	_, err := d.ExecContext(ctx, `UPDATE documents SET frontmatter_json=?, frontmatter_issues=? WHERE path=?`, fmJSON, fmIssues, path)
	return err
}

// MarkMissing flags every document not touched by the current scan timestamp as
// 'missing' instead of deleting it — a missing file may reappear, and this keeps
// its analysis history. Queue-facing queries filter missing documents out (see
// suggestions()), and UpsertScanned restores the status on resurrection.
func (d *DB) MarkMissing(ctx context.Context, seen string) error {
	_, err := d.ExecContext(ctx, `UPDATE documents SET status='missing' WHERE last_seen_at<>?`, seen)
	return err
}

// documentColumns is the shared SELECT list for Document rows.
const documentColumns = `path,sha256,analyzed_sha256,analyzed_model,external_sha256,external_model,enriched_sha256,enriched_model,fm_sha256,body_sha256,seen_fm_sha256,seen_body_sha256,expand_sha256,expand_model,freshness_sha256,freshness_model,embedded_sha256,embedded_model,size,modified_unix,title,summary,tags_json,status,last_error,frontmatter_json,frontmatter_issues,link_issues,sig,embedding`

// scanDocument reads one Document row in documentColumns order.
func scanDocument(rows *sql.Rows) (Document, error) {
	var x Document
	err := rows.Scan(&x.Path, &x.SHA256, &x.AnalyzedSHA256, &x.AnalyzedModel, &x.ExternalSHA256, &x.ExternalModel, &x.EnrichedSHA256, &x.EnrichedModel, &x.FmSHA256, &x.BodySHA256, &x.SeenFmSHA256, &x.SeenBodySHA256, &x.ExpandSHA256, &x.ExpandModel, &x.FreshnessSHA256, &x.FreshnessModel, &x.EmbeddedSHA256, &x.EmbeddedModel, &x.Size, &x.ModifiedUnix, &x.Title, &x.Summary, &x.TagsJSON, &x.Status, &x.LastError, &x.FrontmatterJSON, &x.FrontmatterIssues, &x.LinkIssues, &x.Signature, &x.Embedding)
	return x, err
}

// Pending returns documents that need (re)processing in the local lane: the
// content hash differs from the last analysis hash (analysis pass) or the
// last enrichment hash (fast-model pass), and the file is not missing. The
// enrichment condition only applies when fastModel is a genuinely different
// model — a single configured model never schedules the cheaper pass
// separately. With reanalyze, documents last processed under a different
// model are included. forceAll explicitly includes every non-missing
// document, for example after a prompt or taxonomy policy change; both
// options stay opt-in.
func (d *DB) Pending(ctx context.Context, analysisModel, fastModel string, reanalyze bool, forceAll ...bool) ([]Document, error) {
	return d.pending(ctx, "analyzed_sha256", "analyzed_model", analysisModel, fastModel, reanalyze, forceAll...)
}

// PendingExternal is Pending for the external lane: an enhanced run
// (ollama_model_external) is incremental against its own external_sha256/
// external_model markers instead of the local analyzed_* ones. The
// enrichment stage is shared between lanes, so enrichOn reports whether the
// local config has a distinct fast pass — not whether fastModel differs from
// the external model.
func (d *DB) PendingExternal(ctx context.Context, analysisModel, fastModel string, enrichOn, reanalyze bool, forceAll ...bool) ([]Document, error) {
	twoModel := enrichOn && fastModel != ""
	cond := `sha256<>external_sha256`
	var args []any
	if twoModel {
		cond += ` OR sha256<>enriched_sha256`
	}
	if reanalyze {
		cond += ` OR external_model<>?`
		args = append(args, analysisModel)
		if twoModel {
			cond += ` OR enriched_model<>?`
			args = append(args, fastModel)
		}
	}
	return d.documentsWhere(ctx, `(`+cond+`)`, args)
}

func (d *DB) pending(ctx context.Context, shaCol, modelCol, analysisModel, fastModel string, reanalyze bool, forceAll ...bool) ([]Document, error) {
	if len(forceAll) > 0 && forceAll[0] {
		return d.documentsWhere(ctx, `1=1`, nil)
	}
	twoModel := fastModel != "" && fastModel != analysisModel
	cond := `sha256<>` + shaCol
	var args []any
	if twoModel {
		cond += ` OR sha256<>enriched_sha256`
	}
	if reanalyze {
		cond += ` OR ` + modelCol + `<>?`
		args = append(args, analysisModel)
		if twoModel {
			cond += ` OR enriched_model<>?`
			args = append(args, fastModel)
		}
	}
	return d.documentsWhere(ctx, `(`+cond+`)`, args)
}

// PendingEnrich returns documents needing the fast enrichment pass only:
// the hash differs from the last enrichment stamp, or (with reanalyze) the
// stamp came from another model.
func (d *DB) PendingEnrich(ctx context.Context, fastModel string, reanalyze bool, forceAll ...bool) ([]Document, error) {
	if len(forceAll) > 0 && forceAll[0] {
		return d.documentsWhere(ctx, `1=1`, nil)
	}
	cond := `sha256<>enriched_sha256`
	var args []any
	if reanalyze {
		cond += ` OR enriched_model<>?`
		args = append(args, fastModel)
	}
	return d.documentsWhere(ctx, `(`+cond+`)`, args)
}

// taskColumns maps an optional LLM task to its lineage columns and the
// current-state column the stamp compares against: freshness tracks the body
// hash (frontmatter churn never re-dates content), expand and embed the full
// canonical hash.
func taskColumns(task string) (cur, sha, model string, err error) {
	switch task {
	case "expand":
		return "sha256", "expand_sha256", "expand_model", nil
	case "freshness":
		return "COALESCE(NULLIF(body_sha256,''),sha256)", "freshness_sha256", "freshness_model", nil
	case "embed":
		return "sha256", "embedded_sha256", "embedded_model", nil
	}
	return "", "", "", fmt.Errorf("unknown task %q", task)
}

// taskCurrent returns the document field matching taskColumns' current column.
func taskCurrent(doc Document, task string) string {
	if task == "freshness" && doc.BodySHA256 != "" {
		return doc.BodySHA256
	}
	return doc.SHA256
}

// PendingTask returns documents whose task lineage is behind the current
// content — the task run is incremental, exactly like the analysis lanes.
func (d *DB) PendingTask(ctx context.Context, task, model string, reanalyze bool, forceAll ...bool) ([]Document, error) {
	cur, sha, modelCol, err := taskColumns(task)
	if err != nil {
		return nil, err
	}
	if len(forceAll) > 0 && forceAll[0] {
		return d.documentsWhere(ctx, `1=1`, nil)
	}
	cond := cur + `<>` + sha
	var args []any
	if reanalyze || task == "embed" {
		cond += ` OR ` + modelCol + `<>?`
		args = append(args, model)
	}
	return d.documentsWhere(ctx, `(`+cond+`)`, args) // #nosec G202 -- columns are compile-time constants from taskColumns
}

// ModelStamp is the dominant recorded model for one lineage column — the
// "what ran last time" the UI compares run selections against.
type ModelStamp struct {
	Model string `json:"model"`
	Docs  int    `json:"docs"`
}

// ModelStamps reports the most common recorded model per lineage column
// (analyzed, external, enriched, embedded, expand, freshness) together with
// how many documents carry it. Columns with no stamps are absent.
func (d *DB) ModelStamps(ctx context.Context) (map[string]ModelStamp, error) {
	out := map[string]ModelStamp{}
	for _, col := range []string{"analyzed_model", "external_model", "enriched_model", "embedded_model", "expand_model", "freshness_model"} {
		var stamp ModelStamp
		err := d.QueryRowContext(ctx, `SELECT `+col+`,count(*) FROM documents WHERE `+col+`<>'' AND status<>'missing' GROUP BY `+col+` ORDER BY 2 DESC LIMIT 1`).Scan(&stamp.Model, &stamp.Docs) // #nosec G202 -- columns are compile-time constants
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		name, _ := strings.CutSuffix(col, "_model")
		out[name] = stamp
	}
	return out, nil
}

// SaveTask records a completed task run: stamps the task's lineage columns,
// supersedes the pending suggestions this task owns (origin = task name) and
// inserts the new ones. Status/title stay untouched — only analysis decides
// those.
func (d *DB) SaveTask(ctx context.Context, doc Document, task, model string, suggestions []Suggestion, results ...StageResult) error {
	_, shaCol, modelCol, err := taskColumns(task)
	if err != nil {
		return err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE documents SET `+shaCol+`=?,`+modelCol+`=? WHERE path=?`, // #nosec G202 -- columns are compile-time constants
		taskCurrent(doc, task), model, doc.Path); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE suggestions SET status='superseded',decided_at=COALESCE(decided_at,?) WHERE document_path=? AND status='pending' AND origin=?`, time.Now().UTC().Format(time.RFC3339), doc.Path, task); err != nil {
		return err
	}
	if err := insertSuggestions(ctx, tx, doc, suggestions); err != nil {
		return err
	}
	for _, result := range results {
		if err := insertStageResult(ctx, tx, doc, task, model, "", "", nil, len(suggestions), result); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// StampTask advances a task's lineage without recording a stage result — for
// documents the task deliberately skipped (e.g. non-stubs on an expand run)
// so they do not stay pending forever.
func (d *DB) StampTask(ctx context.Context, doc Document, task, model string) error {
	_, shaCol, modelCol, err := taskColumns(task)
	if err != nil {
		return err
	}
	_, err = d.ExecContext(ctx, `UPDATE documents SET `+shaCol+`=?,`+modelCol+`=? WHERE path=?`, // #nosec G202 -- columns are compile-time constants
		taskCurrent(doc, task), model, doc.Path)
	return err
}

func (d *DB) documentsWhere(ctx context.Context, condition string, args []any) ([]Document, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+documentColumns+` FROM documents WHERE `+condition+` AND status<>'missing' ORDER BY path`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Document{} // non-nil so JSON responses are [] rather than null
	for rows.Next() {
		x, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// CandidatePaths returns up to `limit` other document paths as merge/relation
// context for the prompt. Deliberately simple: alphabetical path list, not
// semantic search (the README calls this out as an MVP limitation).
func (d *DB) CandidatePaths(ctx context.Context, exclude string, limit int) ([]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT path FROM documents WHERE status<>'missing' AND path<>? ORDER BY path LIMIT ?`, exclude, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{} // non-nil so JSON responses are [] rather than null
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// InboundLinks returns the paths of documents whose frontmatter `related` or
// `contradicted_by` lists path — the "who links here" view used to detect
// missing backlinks. The LIKE prefilter may over-match; the Go verification
// on the parsed frontmatter keeps only exact list entries.
func (d *DB) InboundLinks(ctx context.Context, path string) ([]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT path,frontmatter_json FROM documents
WHERE status<>'missing' AND path<>? AND frontmatter_json LIKE '%' || ? || '%'`, path, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var p, fm string
		if err := rows.Scan(&p, &fm); err != nil {
			return nil, err
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(fm), &fields); err != nil {
			continue
		}
		if fmLinksTo(fields, path) {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// fmLinksTo reports whether fields["related"] or fields["contradicted_by"]
// contains target as an exact string entry (scalar or list).
func fmLinksTo(fields map[string]any, target string) bool {
	for _, key := range []string{"related", "contradicted_by"} {
		switch v := fields[key].(type) {
		case string:
			if v == target {
				return true
			}
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok && s == target {
					return true
				}
			}
		}
	}
	return false
}

// truncateRunes bounds a free-text field at max runes — reviewer notes and
// error strings stay small enough to quote in a prompt or a job detail.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// insertSuggestions appends pending suggestion rows inside tx, skipping
// 'none' actions — and anything matching a suggestion the reviewer already
// rejected, so a re-run cannot resurrect a refused proposal. Empty origin
// defaults to 'analysis'.
func insertSuggestions(ctx context.Context, tx *sql.Tx, doc Document, suggestions []Suggestion) error {
	rejected, err := rejectedSuggestionKeys(ctx, tx, doc.Path)
	if err != nil {
		return err
	}
	for _, s := range suggestions {
		if s.Action == "none" || slices.ContainsFunc(suggestionKeys(s), func(k string) bool { return rejected[k] }) {
			continue
		}
		origin := s.Origin
		if origin == "" {
			origin = "analysis"
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO suggestions(document_path,source_sha256,action,target_path,reason,confidence,proposed_content,origin,status,created_at) VALUES(?,?,?,?,?,?,?,?,'pending',?)`, doc.Path, doc.SHA256, s.Action, s.TargetPath, s.Reason, s.Confidence, s.ProposedContent, origin, time.Now().UTC().Format(time.RFC3339))
		if err != nil {
			return err
		}
	}
	return nil
}

// suggestionKeys derives the identity keys a proposal shares with a rejected
// one: targeted actions match on (action, target); document-level actions
// (expand) match on a normalized reason or identical proposed content — so a
// differently-phrased proposal still reaches review while an exact repeat of
// a refused one is suppressed.
func suggestionKeys(s Suggestion) []string {
	if s.TargetPath != "" {
		return []string{s.Action + "|" + s.TargetPath}
	}
	keys := []string{}
	if s.Reason != "" {
		keys = append(keys, s.Action+"|reason|"+normalizeSuggestionText(s.Reason))
	}
	if s.ProposedContent != "" {
		keys = append(keys, s.Action+"|content|"+normalizeSuggestionText(s.ProposedContent))
	}
	return keys
}

// normalizeSuggestionText folds case and whitespace so a rephrased or
// reformatted re-proposal still matches what a reviewer already refused.
func normalizeSuggestionText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// rejectedSuggestionKeys loads the identity keys of every rejected suggestion
// on a document — one small query per save, before the insert loop.
func rejectedSuggestionKeys(ctx context.Context, tx *sql.Tx, path string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT action,target_path,reason,proposed_content FROM suggestions WHERE document_path=? AND status='rejected'`, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := map[string]bool{}
	for rows.Next() {
		var s Suggestion
		if err := rows.Scan(&s.Action, &s.TargetPath, &s.Reason, &s.ProposedContent); err != nil {
			return nil, err
		}
		for _, k := range suggestionKeys(s) {
			keys[k] = true
		}
	}
	return keys, rows.Err()
}

// RejectedSuggestions returns a document's most recent rejected suggestions —
// review feedback for the prompt: the rejected proposal plus the reviewer's
// note on why, newest first.
func (d *DB) RejectedSuggestions(ctx context.Context, path string, limit int) ([]Suggestion, error) {
	rows, err := d.QueryContext(ctx, `SELECT action,target_path,reason,decision_note FROM suggestions WHERE document_path=? AND status='rejected' ORDER BY id DESC LIMIT ?`, path, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Suggestion{}
	for rows.Next() {
		var s Suggestion
		if err := rows.Scan(&s.Action, &s.TargetPath, &s.Reason, &s.DecisionNote); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SaveAnalysis atomically records a successful analysis — title, summary, merged
// tags, analyzed_sha256, status='analyzed' — and inserts any actionable
// suggestions as 'pending'. 'none' suggestions are dropped. The transaction
// makes it all-or-nothing: a crash can't leave half-saved state.
func (d *DB) SaveAnalysis(ctx context.Context, doc Document, model, title, summary string, tags []string, suggestions []Suggestion, results ...StageResult) error {
	return d.saveAnalysis(ctx, doc, "analyzed_sha256", "analyzed_model", model, title, summary, tags, suggestions, results...)
}

// SaveExternalAnalysis is SaveAnalysis for the external lane: an enhanced run
// stamps external_sha256/external_model and leaves the local analyzed_*
// markers untouched, so the next normal run does not see the whole corpus as
// new or model-drifted. Suggestion handling stays identical — pending
// 'analysis' suggestions are superseded and the new ones are inserted.
func (d *DB) SaveExternalAnalysis(ctx context.Context, doc Document, model, title, summary string, tags []string, suggestions []Suggestion, results ...StageResult) error {
	return d.saveAnalysis(ctx, doc, "external_sha256", "external_model", model, title, summary, tags, suggestions, results...)
}

func (d *DB) saveAnalysis(ctx context.Context, doc Document, shaCol, modelCol, model, title, summary string, tags []string, suggestions []Suggestion, results ...StageResult) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	tj, _ := json.Marshal(tags)
	if _, err = tx.ExecContext(ctx, `UPDATE documents SET `+shaCol+`=?,`+modelCol+`=?,title=?,summary=?,tags_json=?,status='analyzed',last_error='',analyzed_at=?,seen_fm_sha256=?,seen_body_sha256=? WHERE path=?`, // #nosec G202 -- shaCol/modelCol are compile-time lane constants, never user input
		doc.SHA256, model, title, summary, string(tj), time.Now().UTC().Format(time.RFC3339), doc.FmSHA256, doc.BodySHA256, doc.Path); err != nil {
		return err
	}
	// Retire the pending analysis suggestions this pass replaces. Keep a
	// standalone deterministic metadata suggestion when analysis proposes only
	// a move; the person may reject the move and still want the local metadata.
	// Accepted/rejected decisions and enrichment-owned suggestions are retained.
	if _, err = tx.ExecContext(ctx, `UPDATE suggestions SET status='superseded',decided_at=COALESCE(decided_at,?) WHERE document_path=? AND status='pending' AND origin='analysis'`, time.Now().UTC().Format(time.RFC3339), doc.Path); err != nil {
		return err
	}
	if analysisReplacesDeterministic(suggestions, results) {
		if _, err = tx.ExecContext(ctx, `UPDATE suggestions SET status='superseded',decided_at=COALESCE(decided_at,?) WHERE document_path=? AND status='pending' AND origin='deterministic'`, time.Now().UTC().Format(time.RFC3339), doc.Path); err != nil {
			return err
		}
	}
	if err := insertSuggestions(ctx, tx, doc, suggestions); err != nil {
		return err
	}
	for _, result := range results {
		if err := insertStageResult(ctx, tx, doc, "analysis", model, title, summary, tags, len(suggestions), result); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func analysisReplacesDeterministic(suggestions []Suggestion, results []StageResult) bool {
	for _, suggestion := range suggestions {
		switch suggestion.Action {
		case "expand", "merge", "delete":
			return true
		}
		if suggestion.Origin == "deterministic" {
			return true
		}
	}
	for _, result := range results {
		if len(result.DerivedFields) > 0 {
			return true
		}
	}
	return false
}

// SaveEnrichment is the fast-model counterpart of SaveAnalysis: it records the
// enrichment pass markers plus title/summary/tags, and replaces the pending
// 'enrich' and 'deterministic' suggestions this pass regenerates. It does not
// touch status or analyzed_* — those belong to the full analysis pass. A late
// enrichment-only retry also preserves metadata from an already-current analysis;
// the separate stage result still retains the new enrichment metadata.
func (d *DB) SaveEnrichment(ctx context.Context, doc Document, model, title, summary string, tags []string, suggestions []Suggestion, results ...StageResult) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	tj, _ := json.Marshal(tags)
	if _, err = tx.ExecContext(ctx, `UPDATE documents SET enriched_sha256=?,enriched_model=?,
title=CASE WHEN analyzed_sha256=sha256 THEN title ELSE ? END,
summary=CASE WHEN analyzed_sha256=sha256 THEN summary ELSE ? END,
tags_json=CASE WHEN analyzed_sha256=sha256 THEN tags_json ELSE ? END WHERE path=?`,
		doc.SHA256, model, title, summary, string(tj), doc.Path); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE suggestions SET status='superseded',decided_at=COALESCE(decided_at,?) WHERE document_path=? AND status='pending' AND origin IN ('enrich','deterministic')`, time.Now().UTC().Format(time.RFC3339), doc.Path); err != nil {
		return err
	}
	if err := insertSuggestions(ctx, tx, doc, suggestions); err != nil {
		return err
	}
	for _, result := range results {
		if err := insertStageResult(ctx, tx, doc, "enrich", model, title, summary, tags, len(suggestions), result); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetSignature stores a computed similarity sketch for a document — used to
// backfill signatures for rows scanned before the index existed.
func (d *DB) SetSignature(ctx context.Context, path string, sig []byte) error {
	_, err := d.ExecContext(ctx, `UPDATE documents SET sig=? WHERE path=?`, sig, path)
	return err
}

// SetEmbedding stores a marshaled embedding vector for a document — backfilled
// lazily by the analyzer when ollama_embed_model is configured.
func (d *DB) SetEmbedding(ctx context.Context, path string, emb []byte) error {
	_, err := d.ExecContext(ctx, `UPDATE documents SET embedding=? WHERE path=?`, emb, path)
	return err
}

// SaveError marks a document 'error' with a message. The hash mismatch remains,
// so the file is retried on the next analyze run.
func (d *DB) SaveError(ctx context.Context, path, msg string) error {
	_, err := d.ExecContext(ctx, `UPDATE documents SET status='error',last_error=? WHERE path=?`, msg, path)
	return err
}

// PendingSuggestions lists suggestions awaiting review (status='pending').
func (d *DB) PendingSuggestions(ctx context.Context) ([]Suggestion, error) {
	return d.suggestions(ctx, "pending")
}

// Suggestions lists suggestions with an arbitrary status — used by the HTTP API
// for ?status= filtering (pending, accepted, rejected, applied, failed, superseded).
func (d *DB) Suggestions(ctx context.Context, status string) ([]Suggestion, error) {
	return d.suggestions(ctx, status)
}

// Suggestion fetches a single suggestion by id (full row incl. proposed_content).
// Returns sql.ErrNoRows if the id does not exist.
func (d *DB) Suggestion(ctx context.Context, id int64) (Suggestion, error) {
	var s Suggestion
	err := d.QueryRowContext(ctx, `SELECT id,document_path,source_sha256,action,target_path,reason,confidence,proposed_content,status,origin,created_at,decision_note FROM suggestions WHERE id=?`, id).
		Scan(&s.ID, &s.DocumentPath, &s.SourceSHA256, &s.Action, &s.TargetPath, &s.Reason, &s.Confidence, &s.ProposedContent, &s.Status, &s.Origin, &s.CreatedAt, &s.DecisionNote)
	return s, err
}

// Documents lists every tracked document, ordered by path — used by the HTTP API.
func (d *DB) Documents(ctx context.Context) ([]Document, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+documentColumns+` FROM documents ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Document{} // non-nil so JSON responses are [] rather than null
	for rows.Next() {
		x, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// KnowledgeDashboard returns documents that still differ from their last
// successful analysis, how many were analysed under a different model than
// configured (drift — re-runable via analyze's reanalyze option), and the
// latest completed `run` cycle.
func (d *DB) KnowledgeDashboard(ctx context.Context, model string) (KnowledgeDashboard, error) {
	var overview KnowledgeDashboard
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents
WHERE sha256<>analyzed_sha256 AND status<>'missing'`).Scan(&overview.ChangedFiles); err != nil {
		return KnowledgeDashboard{}, err
	}
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents
WHERE analyzed_model<>? AND sha256=analyzed_sha256 AND status<>'missing'`, model).Scan(&overview.ModelDrift); err != nil {
		return KnowledgeDashboard{}, err
	}
	err := d.QueryRowContext(ctx, `SELECT COALESCE((
SELECT finished_at FROM jobs
WHERE op='run' AND status='done' AND finished_at IS NOT NULL
ORDER BY id DESC LIMIT 1
), '')`).Scan(&overview.LastCycleAt)
	return overview, err
}

// suggestions is the shared query helper behind PendingSuggestions and
// Accepted. Suggestions whose source document is missing are excluded —
// MarkMissing retires the document row but keeps it (and its suggestions)
// for resurrection, so a vanished file must not keep feeding the review and
// apply queues. If the file reappears its suggestions resurface still valid.
func (d *DB) suggestions(ctx context.Context, status string) ([]Suggestion, error) {
	rows, err := d.QueryContext(ctx, `SELECT id,document_path,source_sha256,action,target_path,reason,confidence,proposed_content,status,origin,created_at,decision_note FROM suggestions WHERE status=? AND EXISTS(SELECT 1 FROM documents d WHERE d.path=suggestions.document_path AND d.status<>'missing') ORDER BY id`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Suggestion{} // non-nil so JSON responses are [] rather than null
	for rows.Next() {
		var s Suggestion
		if err := rows.Scan(&s.ID, &s.DocumentPath, &s.SourceSHA256, &s.Action, &s.TargetPath, &s.Reason, &s.Confidence, &s.ProposedContent, &s.Status, &s.Origin, &s.CreatedAt, &s.DecisionNote); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Decide transitions a pending suggestion to 'accepted' or 'rejected'. The
// WHERE ... AND status='pending' guard plus the RowsAffected check means it
// errors cleanly if the id doesn't exist or was already decided. note is the
// reviewer's optional "why" — bounded and stored for prompt feedback.
func (d *DB) Decide(ctx context.Context, id int64, status, note string) error {
	// The note is reviewer feedback for future prompts — only rejections
	// carry it; "accepted" needs no explanation and would pollute context.
	if status != "rejected" {
		note = ""
	}
	r, err := d.ExecContext(ctx, `UPDATE suggestions SET status=?,decided_at=?,decision_note=? WHERE id=? AND status='pending'`,
		status, time.Now().UTC().Format(time.RFC3339), truncateRunes(strings.TrimSpace(note), 500), id)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("suggestion %d is not pending or does not exist", id)
	}
	return nil
}

// Accepted lists suggestions approved in review, ready for `KBM-flow apply`.
func (d *DB) Accepted(ctx context.Context) ([]Suggestion, error) {
	return d.suggestions(ctx, "accepted")
}

// MarkApplied records a successfully executed suggestion.
func (d *DB) MarkApplied(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `UPDATE suggestions SET status='applied',applied_at=?,error='' WHERE id=?`, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// MarkApplyFailed records an apply failure; the suggestion is kept (status='failed') for inspection.
func (d *DB) MarkApplyFailed(ctx context.Context, id int64, msg string) error {
	_, err := d.ExecContext(ctx, `UPDATE suggestions SET status='failed',error=?,decided_at=COALESCE(decided_at,?) WHERE id=?`, msg, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// MarkSuperseded prevents an accepted suggestion from applying after its source
// document has changed since the analysis was generated.
func (d *DB) MarkSuperseded(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `UPDATE suggestions SET status='superseded',decided_at=COALESCE(decided_at,?) WHERE id=? AND status IN ('pending','accepted')`, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// Job mirrors a row of the jobs table: a background scan/analyze/run/report
// operation started via the HTTP API. Persisted so job history survives restarts.
type Job struct {
	ID       int64  `json:"id"`
	Op       string `json:"op"`
	Status   string `json:"status"` // running | done | failed
	Detail   string `json:"detail,omitempty"`
	Error    string `json:"error,omitempty"`
	Started  string `json:"started"`
	Finished string `json:"finished,omitempty"`
}

// Job heartbeats prove a runner is alive across processes: a CLI analyze and
// an HTTP op share the same SQLite file, so a 'running' row alone cannot tell
// a live worker from a killed one. Runners refresh heartbeat_at every
// jobHeartbeatInterval; a beat older than jobStaleAfter counts as dead and the
// job is swept to 'failed' by the next RunningJob check.
const (
	jobHeartbeatInterval = 15 * time.Second
	jobStaleAfter        = 90 * time.Second
	// jobHistoryKeep bounds the jobs table: every CreateJob drops finished
	// rows beyond the newest jobHistoryKeep so history cannot grow forever.
	jobHistoryKeep = 200
)

// CreateJob inserts a 'running' job and returns it.
func (d *DB) CreateJob(ctx context.Context, op string) (Job, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	r, err := d.ExecContext(ctx, `INSERT INTO jobs(op,status,created_at,heartbeat_at) VALUES(?,'running',?,?)`, op, now, now)
	if err != nil {
		return Job{}, err
	}
	id, err := r.LastInsertId()
	// Retention is best-effort: a failed prune must not block the new job.
	_, _ = d.PruneFinishedJobs(ctx, jobHistoryKeep)
	return Job{ID: id, Op: op, Status: "running", Started: now}, err
}

// PruneFinishedJobs deletes finished ('done'/'failed') job rows beyond the
// newest `keep`; running jobs are never touched. Returns rows removed.
func (d *DB) PruneFinishedJobs(ctx context.Context, keep int) (int64, error) {
	res, err := d.ExecContext(ctx, `DELETE FROM jobs WHERE status<>'running' AND id <= COALESCE((
SELECT id FROM jobs WHERE status<>'running' ORDER BY id DESC LIMIT 1 OFFSET ?
), -1)`, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RunningJob returns the live running job, if any. Stale runners — a heartbeat
// frozen longer than jobStaleAfter, which means the owning process died — are
// marked failed first so they cannot block every later op forever.
func (d *DB) RunningJob(ctx context.Context) (Job, bool, error) {
	now := time.Now().UTC()
	if _, err := d.ExecContext(ctx, `UPDATE jobs SET status='failed',error='interrupted (stale heartbeat)',finished_at=? WHERE status='running' AND COALESCE(heartbeat_at,'')<?`,
		now.Format(time.RFC3339), now.Add(-jobStaleAfter).Format(time.RFC3339)); err != nil {
		return Job{}, false, err
	}
	var j Job
	err := d.QueryRowContext(ctx, `SELECT id,op,status,detail,error,created_at,COALESCE(finished_at,'') FROM jobs WHERE status='running' ORDER BY id LIMIT 1`).
		Scan(&j.ID, &j.Op, &j.Status, &j.Detail, &j.Error, &j.Started, &j.Finished)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	return j, true, nil
}

// HeartbeatJob refreshes a running job's liveness timestamp.
func (d *DB) HeartbeatJob(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `UPDATE jobs SET heartbeat_at=? WHERE id=? AND status='running'`,
		time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// RunHeartbeat refreshes a running job's heartbeat on a ticker until the
// returned stop function is called or ctx ends. Every runner (CLI command or
// server op) must hold one so peers can distinguish live work from a corpse.
func (d *DB) RunHeartbeat(ctx context.Context, id int64) (stop func()) {
	return d.runHeartbeat(ctx, id, jobHeartbeatInterval)
}

func (d *DB) runHeartbeat(ctx context.Context, id int64, interval time.Duration) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				_ = d.HeartbeatJob(ctx, id)
			}
		}
	}()
	return func() { close(done) }
}

// FinishJob records the outcome of a job.
func (d *DB) FinishJob(ctx context.Context, id int64, status, detail, errText string) error {
	_, err := d.ExecContext(ctx, `UPDATE jobs SET status=?,detail=?,error=?,finished_at=? WHERE id=?`,
		status, detail, errText, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// UpdateJobDetail refreshes the live detail of a running job (e.g. analysis
// progress) and bumps its heartbeat; finished jobs are left untouched.
func (d *DB) UpdateJobDetail(ctx context.Context, id int64, detail string) error {
	_, err := d.ExecContext(ctx, `UPDATE jobs SET detail=?,heartbeat_at=? WHERE id=? AND status='running'`,
		detail, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// SaveRepoReportSnapshot atomically replaces the latest local repository
// report. The original report JSON stays intact for the API and dashboard.
func (d *DB) SaveRepoReportSnapshot(ctx context.Context, generatedAt string, data []byte) error {
	if !json.Valid(data) {
		return fmt.Errorf("invalid repository report JSON")
	}
	_, err := d.ExecContext(ctx, `INSERT INTO repo_report_snapshot(id,generated_at,data_json)
VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET generated_at=excluded.generated_at,data_json=excluded.data_json`,
		generatedAt, string(data))
	return err
}

// RepoReportSnapshot returns the latest report JSON and its generated time.
func (d *DB) RepoReportSnapshot(ctx context.Context) (string, json.RawMessage, error) {
	var generatedAt, data string
	err := d.QueryRowContext(ctx, `SELECT generated_at,data_json FROM repo_report_snapshot WHERE id=1`).
		Scan(&generatedAt, &data)
	return generatedAt, json.RawMessage(data), err
}

// reportHistoryKeep bounds retained per-run report summaries so trend charts
// stay useful without growing the database unboundedly.
const reportHistoryKeep = 365

// ReportHistoryPoint is one run's report summary for the progress chart.
type ReportHistoryPoint struct {
	GeneratedAt string          `json:"generated_at"`
	Summary     json.RawMessage `json:"summary"`
}

// SaveRepoReportHistory appends one run's summary for the progress-over-time
// chart, pruning rows beyond reportHistoryKeep in the same transaction.
func (d *DB) SaveRepoReportHistory(ctx context.Context, generatedAt string, summary []byte) error {
	if !json.Valid(summary) {
		return fmt.Errorf("invalid report history summary JSON")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO repo_report_history(generated_at,summary_json) VALUES(?,?)`,
		generatedAt, string(summary)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM repo_report_history WHERE id NOT IN
		(SELECT id FROM repo_report_history ORDER BY id DESC LIMIT ?)`,
		reportHistoryKeep); err != nil {
		return err
	}
	return tx.Commit()
}

// RepoReportHistory returns stored per-run summaries oldest-first, capped at
// limit rows (reportHistoryKeep when limit is not positive).
func (d *DB) RepoReportHistory(ctx context.Context, limit int) ([]ReportHistoryPoint, error) {
	if limit <= 0 {
		limit = reportHistoryKeep
	}
	rows, err := d.QueryContext(ctx,
		`SELECT generated_at,summary_json FROM repo_report_history ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var points []ReportHistoryPoint
	for rows.Next() {
		var point ReportHistoryPoint
		var summary string
		if err := rows.Scan(&point.GeneratedAt, &summary); err != nil {
			return nil, err
		}
		point.Summary = json.RawMessage(summary)
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(points)-1; i < j; i, j = i+1, j-1 {
		points[i], points[j] = points[j], points[i]
	}
	return points, nil
}

// KnowledgeProgressDay counts pipeline events for one calendar day, derived
// from the immutable timestamps on documents and suggestions.
type KnowledgeProgressDay struct {
	Date        string `json:"date"`
	Notes       int    `json:"notes"`
	Analyzed    int    `json:"analyzed"`
	Suggestions int    `json:"suggestions"`
	Handled     int    `json:"handled"`
	Applied     int    `json:"applied"`
}

// KnowledgeProgress aggregates per-day pipeline events for the knowledge
// progress chart: first-seen notes, analyzed notes, created suggestions, and
// decided (handled)/applied suggestions.
func (d *DB) KnowledgeProgress(ctx context.Context) ([]KnowledgeProgressDay, error) {
	series := []struct {
		query string
		set   func(*KnowledgeProgressDay, int)
	}{
		{`SELECT substr(first_seen_at,1,10) AS day,COUNT(*) FROM documents GROUP BY day`,
			func(p *KnowledgeProgressDay, n int) { p.Notes = n }},
		{`SELECT substr(analyzed_at,1,10) AS day,COUNT(*) FROM documents WHERE analyzed_at IS NOT NULL AND analyzed_at<>'' GROUP BY day`,
			func(p *KnowledgeProgressDay, n int) { p.Analyzed = n }},
		{`SELECT substr(created_at,1,10) AS day,COUNT(*) FROM suggestions GROUP BY day`,
			func(p *KnowledgeProgressDay, n int) { p.Suggestions = n }},
		{`SELECT substr(decided_at,1,10) AS day,COUNT(*) FROM suggestions WHERE decided_at IS NOT NULL AND decided_at<>'' GROUP BY day`,
			func(p *KnowledgeProgressDay, n int) { p.Handled = n }},
		{`SELECT substr(applied_at,1,10) AS day,COUNT(*) FROM suggestions WHERE applied_at IS NOT NULL AND applied_at<>'' GROUP BY day`,
			func(p *KnowledgeProgressDay, n int) { p.Applied = n }},
	}
	days := map[string]*KnowledgeProgressDay{}
	for _, item := range series {
		if err := d.progressDayCounts(ctx, item.query, days, item.set); err != nil {
			return nil, err
		}
	}
	keys := make([]string, 0, len(days))
	for day := range days {
		keys = append(keys, day)
	}
	slices.Sort(keys)
	result := make([]KnowledgeProgressDay, 0, len(keys))
	for _, day := range keys {
		result = append(result, *days[day])
	}
	return result, nil
}

// progressDayCounts folds one day-bucketed query into the shared map via set.
// Isolated so rows.Close stays deferred (gosec flags unhandled closes).
func (d *DB) progressDayCounts(ctx context.Context, query string, days map[string]*KnowledgeProgressDay, set func(*KnowledgeProgressDay, int)) error {
	rows, err := d.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var day string
		var n int
		if err := rows.Scan(&day, &n); err != nil {
			return err
		}
		entry := days[day]
		if entry == nil {
			entry = &KnowledgeProgressDay{Date: day}
			days[day] = entry
		}
		set(entry, n)
	}
	return rows.Err()
}

// Jobs returns the most recent `limit` jobs, newest first.
func (d *DB) Jobs(ctx context.Context, limit int) ([]Job, error) {
	rows, err := d.QueryContext(ctx, `SELECT id,op,status,detail,error,created_at,COALESCE(finished_at,'') FROM jobs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

// JobsForOps returns the most recent jobs whose operation is in ops. An
// empty operation list returns an empty slice.
func (d *DB) JobsForOps(ctx context.Context, limit int, ops ...string) ([]Job, error) {
	if len(ops) == 0 {
		return []Job{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ops)), ",")
	query := `SELECT id,op,status,detail,error,created_at,COALESCE(finished_at,'') FROM jobs WHERE op IN (` +
		placeholders + `) ORDER BY id DESC LIMIT ?`
	args := make([]any, 0, len(ops)+1)
	for _, op := range ops {
		args = append(args, op)
	}
	args = append(args, limit)
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

func scanJobs(rows *sql.Rows) ([]Job, error) {
	out := []Job{} // non-nil so JSON responses are [] rather than null
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.Op, &j.Status, &j.Detail, &j.Error, &j.Started, &j.Finished); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Job fetches a single job by id; returns sql.ErrNoRows if it doesn't exist.
func (d *DB) Job(ctx context.Context, id int64) (Job, error) {
	var j Job
	err := d.QueryRowContext(ctx, `SELECT id,op,status,detail,error,created_at,COALESCE(finished_at,'') FROM jobs WHERE id=?`, id).
		Scan(&j.ID, &j.Op, &j.Status, &j.Detail, &j.Error, &j.Started, &j.Finished)
	return j, err
}

// FailStaleJobs marks any job still 'running' as failed — called at server
// startup, since a running job cannot survive a restart.
func (d *DB) FailStaleJobs(ctx context.Context) error {
	_, err := d.ExecContext(ctx, `UPDATE jobs SET status='failed',error='interrupted by server restart',finished_at=? WHERE status='running'`,
		time.Now().UTC().Format(time.RFC3339))
	return err
}
