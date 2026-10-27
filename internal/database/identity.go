package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

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

type Reference struct {
	WrittenTarget string `json:"written_target"`
	TargetPath    string `json:"target_path"`
	TargetID      string `json:"target_document_id"`
	Kind          string `json:"kind"`
	SourceSHA256  string `json:"source_sha256"`
}

func (d *DB) SourceReferences(ctx context.Context, source string) ([]Reference, error) {
	rows, err := d.QueryContext(ctx, `SELECT written_target,target_path,target_document_id,kind,source_sha256 FROM document_references WHERE source_path=?`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Reference{}
	for rows.Next() {
		var ref Reference
		if err := rows.Scan(&ref.WrittenTarget, &ref.TargetPath, &ref.TargetID, &ref.Kind, &ref.SourceSHA256); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

func (d *DB) ReplaceReferences(ctx context.Context, source, hash string, refs []Reference) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM document_references WHERE source_path=?`, source); err != nil {
		return err
	}
	for _, ref := range refs {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO document_references
(source_path,written_target,target_path,target_document_id,kind,source_sha256) VALUES(?,?,?,?,?,?)`,
			source, ref.WrittenTarget, ref.TargetPath, ref.TargetID, ref.Kind, hash); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO reference_versions(source_path,source_sha256) VALUES(?,?)
ON CONFLICT(source_path) DO UPDATE SET source_sha256=excluded.source_sha256`, source, hash); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) IndexedInboundLinks(ctx context.Context, path string) ([]string, bool, error) {
	var incomplete int
	err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM documents d WHERE d.status<>'missing'
AND NOT EXISTS(SELECT 1 FROM reference_versions v WHERE v.source_path=d.path AND v.source_sha256=d.sha256))`).Scan(&incomplete)
	if err != nil || incomplete != 0 {
		return nil, false, err
	}
	var raw string
	if err := d.QueryRowContext(ctx, `SELECT frontmatter_json FROM documents WHERE path=? AND status<>'missing'`, path).Scan(&raw); err != nil && err != sql.ErrNoRows {
		return nil, false, err
	}
	id := DocumentIdentity(raw)
	if id != "" && !d.uniqueIdentityAt(ctx, path, id) {
		return nil, true, fmt.Errorf("ambiguous document_id for %s", path)
	}
	rows, err := d.QueryContext(ctx, `SELECT DISTINCT r.source_path FROM document_references r
JOIN documents d ON d.path=r.source_path AND d.sha256=r.source_sha256 AND d.status<>'missing'
WHERE r.source_path<>? AND (r.target_path=? OR (?<>'' AND r.target_document_id=?)) ORDER BY r.source_path`, path, path, id, id)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var source string
		if err := rows.Scan(&source); err != nil {
			return nil, false, err
		}
		out = append(out, source)
	}
	return out, true, rows.Err()
}
