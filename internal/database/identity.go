package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"knowledge-ba46-flow/internal/frontmatter"
)

const identityExpression = `CASE WHEN json_valid(frontmatter_json) THEN lower(json_extract(frontmatter_json,'$.document_id')) END`

func DocumentIdentity(raw string) string {
	var fields map[string]any
	if json.Unmarshal([]byte(raw), &fields) != nil {
		return ""
	}
	id, _ := frontmatter.DocumentID(fields)
	return id
}

func (d *DB) upgradeIdentitySchema(ctx context.Context) error {
	var count int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('renames') WHERE name='document_id'`).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		if _, err := d.ExecContext(ctx, `ALTER TABLE renames ADD COLUMN document_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	_, err := d.ExecContext(ctx, `
CREATE INDEX IF NOT EXISTS idx_documents_identity ON documents((`+identityExpression+`));
CREATE TABLE IF NOT EXISTS document_references (
 source_path TEXT NOT NULL,
 written_target TEXT NOT NULL,
 target_path TEXT NOT NULL,
 target_document_id TEXT NOT NULL DEFAULT '',
 kind TEXT NOT NULL,
 source_sha256 TEXT NOT NULL,
 PRIMARY KEY(source_path,written_target,kind),
 FOREIGN KEY(source_path) REFERENCES documents(path) ON UPDATE CASCADE ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_document_references_path ON document_references(target_path,source_path);
CREATE INDEX IF NOT EXISTS idx_document_references_identity ON document_references(target_document_id,source_path)
 WHERE target_document_id<>'';
CREATE TABLE IF NOT EXISTS reference_versions (
 source_path TEXT PRIMARY KEY,
 source_sha256 TEXT NOT NULL,
 FOREIGN KEY(source_path) REFERENCES documents(path) ON UPDATE CASCADE ON DELETE CASCADE
);`)
	return err
}

// A UUID match is identity evidence even after an edit, but only a unique,
// physically vanished predecessor can supply its lineage. Without a UUID the
// fallback still requires one exact canonical-hash match and absence proof.
func renameObserved(ctx context.Context, tx *sql.Tx, doc Document, seen string, absent func(string) bool) (bool, error) {
	if absent == nil {
		return false, nil
	}
	id := DocumentIdentity(doc.FrontmatterJSON)
	query := `SELECT path,last_seen_at FROM documents WHERE path<>? AND sha256=?`
	value := doc.SHA256
	if id != "" {
		query = `SELECT path,last_seen_at FROM documents WHERE path<>? AND (` + identityExpression + `)=?`
		value = id
	}
	rows, err := tx.QueryContext(ctx, query, doc.Path, value)
	if err != nil {
		return false, err
	}
	var previous []string
	ambiguous := false
	for rows.Next() {
		var path, lastSeen string
		if err := rows.Scan(&path, &lastSeen); err != nil {
			_ = rows.Close()
			return false, err
		}
		if lastSeen == seen || !absent(path) {
			ambiguous = true
		}
		previous = append(previous, path)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil || ambiguous || len(previous) != 1 {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE documents SET path=? WHERE path=?`, doc.Path, previous[0]); err != nil {
		return false, err
	}
	// Updating the document path cascades its source-reference rows. The
	// resulting inbound path sets may change for notes it referenced.
	if err := bumpEvidenceGeneration(ctx, tx); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO renames(old_path,new_path,moved_at,document_id) VALUES(?,?,?,?)`,
		previous[0], doc.Path, seen, id); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE suggestions SET target_path=? WHERE target_path=? AND action='merge' AND status IN ('pending','accepted')`,
		doc.Path, previous[0]); err != nil {
		return false, err
	}
	return true, nil
}

func (d *DB) uniqueIdentityAt(ctx context.Context, path, id string) bool {
	var count int
	var actual string
	if err := d.QueryRowContext(ctx, `SELECT COALESCE((`+identityExpression+`),'') FROM documents WHERE path=? AND status<>'missing'`, path).Scan(&actual); err != nil || actual != id {
		return false
	}
	err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents WHERE status<>'missing' AND (`+identityExpression+`)=?`, id).Scan(&count)
	return err == nil && count == 1
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
	target, _, ok := d.RetargetEvidence(ctx, path)
	return target, ok
}

// RetargetEvidence also returns the UUID recorded at the move. Callers must
// check that identity against the live target, not trust a stale scan alone.
func (d *DB) RetargetEvidence(ctx context.Context, path string) (string, string, bool) {
	var originalStatus string
	err := d.QueryRowContext(ctx, `SELECT status FROM documents WHERE path=?`, path).Scan(&originalStatus)
	if err == nil && originalStatus != "missing" {
		return "", "", false
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", "", false
	}
	cur := path
	expectedID := ""
	seen := make(map[string]bool)
	// Follow the renames map to the end of the chain — intermediate hops no
	// longer exist as document rows (the row was re-keyed onward), so only
	// the final endpoint is checked for liveness. The hop bound protects
	// against a corrupted cyclic map.
	for i := 0; i < 8; i++ {
		if seen[cur] {
			return "", "", false
		}
		seen[cur] = true
		var next, id string
		if err := d.QueryRowContext(ctx, `SELECT new_path,document_id FROM renames WHERE old_path=?`, cur).Scan(&next, &id); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return "", "", false
			}
			if cur == path {
				return "", "", false
			}
			var status string
			if err := d.QueryRowContext(ctx, `SELECT status FROM documents WHERE path=?`, cur).Scan(&status); err != nil || status == "missing" {
				return "", "", false
			}
			if expectedID != "" && !d.uniqueIdentityAt(ctx, cur, expectedID) {
				return "", "", false
			}
			return cur, expectedID, true
		}
		if id != "" {
			if expectedID != "" && expectedID != id {
				return "", "", false
			}
			expectedID = id
		}
		cur = next
	}
	return "", "", false
}

// RecordMergeRename records an applied merge as a rename: the absorbed
// path resolves to the merge target, its inbound references retarget to the
// merged document, and the row is tombstoned. Consumers' recorded evidence
// rewrites src→target via RetargetEvidence at the next scan — so links and
// dependents converge on the merged note deterministically, without an LLM
// pass just to redraw edges.
func (d *DB) RecordMergeRename(ctx context.Context, src, dst, movedAt string) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO renames(old_path,new_path,moved_at,document_id) VALUES(?,?,?,'')`,
		src, dst, movedAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE document_references SET target_path=? WHERE target_path=?`, dst, src); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE documents SET status='missing',missing_since=? WHERE path=?`, movedAt, src); err != nil {
		return err
	}
	if err := bumpEvidenceGeneration(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
