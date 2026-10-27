package database

import (
	"context"
	"fmt"
)

// SaveEmbedding commits the vector, semantic input hash, model and audit row
// together while validating the strict source SHA to reject a concurrent edit.
// A failed/interrupted model switch can never stamp old vectors as the new model.
func (d *DB) SaveEmbedding(ctx context.Context, doc Document, model string, vector []byte) error {
	if model == "" || len(vector) == 0 || len(vector)%4 != 0 {
		return fmt.Errorf("invalid embedding for %s", doc.Path)
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	semanticSHA := doc.SemanticSHA256
	if semanticSHA == "" {
		semanticSHA = doc.SHA256
	}
	result, err := tx.ExecContext(ctx, `UPDATE documents
SET embedding=?,embedded_sha256=?,embedded_model=? WHERE path=? AND sha256=? AND status<>'missing'`,
		vector, semanticSHA, model, doc.Path, doc.SHA256)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("document %s changed during embedding; scan and retry", doc.Path)
	}
	if err := insertStageResult(ctx, tx, doc, "embed", model, "", "", nil, 0, StageResult{Outcome: "success"}); err != nil {
		return err
	}
	return tx.Commit()
}
