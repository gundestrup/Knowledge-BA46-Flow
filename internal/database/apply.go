package database

import (
	"context"
	"fmt"
	"time"
)

func (d *DB) upgradeApplyColumns(ctx context.Context) error {
	rows, err := d.QueryContext(ctx, `PRAGMA table_info(suggestions)`)
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return err
		}
		columns[name] = true
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, name := range []string{"apply_batch_id", "apply_attempted_at"} {
		if !columns[name] {
			// name is an internal constant, never repository/request input.
			if _, err := d.ExecContext(ctx, `ALTER TABLE suggestions ADD COLUMN `+name+` TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
	}
	return nil
}

// RecordApplyAttempt records batch membership before a filesystem operation.
func (d *DB) RecordApplyAttempt(ctx context.Context, id int64, batch, at string) error {
	_, err := d.ExecContext(ctx, `UPDATE suggestions SET apply_batch_id=?,apply_attempted_at=? WHERE id=?`, batch, at, id)
	return err
}

// AcceptExclusive makes the one-snapshot-per-document guard atomic even when
// independent CLI processes accept alternatives at the same time.
func (d *DB) AcceptExclusive(ctx context.Context, id int64) error {
	result, err := d.ExecContext(ctx, `UPDATE suggestions SET status='accepted',decided_at=?,decision_note=''
WHERE id=? AND status='pending' AND NOT EXISTS (
SELECT 1 FROM suggestions AS other WHERE other.document_path=suggestions.document_path AND other.status='accepted')`,
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("suggestion %d is no longer pending or another proposal for its document is already accepted", id)
	}
	return nil
}

// RecordAppliedEdges stores the graph edges a successful apply added to one
// document — provenance the dependents sweep uses to recognise that a newly
// recorded inbound edge came from a human-reviewed link decision.
func (d *DB) RecordAppliedEdges(ctx context.Context, source string, edges map[string][]string) error {
	at := time.Now().UTC().Format(time.RFC3339)
	for kind, targets := range edges {
		for _, target := range targets {
			if _, err := d.ExecContext(ctx, `INSERT OR IGNORE INTO applied_edges(source_path,target_path,kind,applied_at) VALUES(?,?,?,?)`, source, target, kind, at); err != nil {
				return err
			}
		}
	}
	return nil
}

// MarkAppliedAndSupersede retires old snapshots immediately, without stamping
// analysis lineage or claiming that the rewritten document was re-analyzed.
func (d *DB) MarkAppliedAndSupersede(ctx context.Context, s Suggestion, currentHash string) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, `UPDATE suggestions SET status='applied',applied_at=?,error='' WHERE id=?`, at, s.ID); err != nil {
		return err
	}
	reason := fmt.Sprintf("source document changed when suggestion #%d was applied; review a new proposal", s.ID)
	if _, err := tx.ExecContext(ctx, `UPDATE suggestions SET status='superseded',error=?,decided_at=COALESCE(decided_at,?)
WHERE document_path=? AND id<>? AND status IN ('pending','accepted') AND source_sha256<>?`,
		reason, at, s.DocumentPath, s.ID, currentHash); err != nil {
		return err
	}
	return tx.Commit()
}
