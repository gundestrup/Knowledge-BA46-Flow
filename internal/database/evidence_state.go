// The evidence-state table carries the monotonic removal/reference-topology
// generation that the dependents sweep acknowledges per lane. Deletions and
// link-graph changes bump it; analysis lanes stamp the generation they saw.
package database

import (
	"context"
	"database/sql"
)

// bumpEvidenceGeneration advances the shared evidence generation inside an
// existing transaction — one event per batch of missing docs, reference
// topology changes, or rename retargets.
func bumpEvidenceGeneration(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE evidence_state SET removal_generation=removal_generation+1 WHERE id=1`)
	return err
}

// MissingDocument is a tombstone with the monotonic generation at which it
// disappeared. The generation survives repeated scans and clock adjustments.
type MissingDocument struct {
	Path       string
	Generation int64
}

// MissingDocuments returns all currently missing paths with a removal event.
// Consumers compare each event's generation with their own lane's last
// completed sweep, making deletion propagation idempotent and retryable.
func (d *DB) MissingDocuments(ctx context.Context) ([]MissingDocument, error) {
	rows, err := d.QueryContext(ctx, `SELECT path,missing_generation FROM documents WHERE status='missing' AND missing_generation>0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MissingDocument
	for rows.Next() {
		var doc MissingDocument
		if err := rows.Scan(&doc.Path, &doc.Generation); err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
	return out, rows.Err()
}

// RemovalGeneration returns the latest monotonic evidence-event generation.
// The SQLite column name is historical; it advances for deletions and
// reference-topology changes.
func (d *DB) RemovalGeneration(ctx context.Context) (int64, error) {
	var generation int64
	err := d.QueryRowContext(ctx, `SELECT removal_generation FROM evidence_state WHERE id=1`).Scan(&generation)
	return generation, err
}

// NeedsRemovalSweep reports whether a visible document in the selected
// analysis lane has not yet checked evidence events through the current
// generation.
func (d *DB) NeedsRemovalSweep(ctx context.Context, external bool) (bool, error) {
	seenCol := "seen_removal_generation"
	if external {
		seenCol = "seen_external_removal_generation"
	}
	var needed bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM documents d CROSS JOIN evidence_state s
WHERE d.status<>'missing' AND d.`+seenCol+`<s.removal_generation
)`).Scan(&needed) // #nosec G202 -- seenCol is selected from fixed internal column names only.
	return needed, err
}

// MarkRemovalChecked advances one document's lane-local evidence sweep without
// touching its analysis stamp. It is only used after proving the document did
// not consume evidence affected by the new generation.
func (d *DB) MarkRemovalChecked(ctx context.Context, path string, external bool, generation int64) error {
	seenCol := "seen_removal_generation"
	if external {
		seenCol = "seen_external_removal_generation"
	}
	_, err := d.ExecContext(ctx, `UPDATE documents SET `+seenCol+`=MAX(`+seenCol+`,?) WHERE path=?`, generation, path) // #nosec G202 -- seenCol is selected from fixed internal column names only.
	return err
}

// RestampEvidence writes a reconstructed v2 evidence record and acknowledges
// the current removal generation in one update — the no-LLM migration for
// rows analyzed before evidence recording existed.
func (d *DB) RestampEvidence(ctx context.Context, path string, external bool, evidence string, generation int64) error {
	col, genCol := "analysis_evidence_json", "seen_removal_generation"
	if external {
		col, genCol = "external_analysis_evidence_json", "seen_external_removal_generation"
	}
	_, err := d.ExecContext(ctx, `UPDATE documents SET `+col+`=?,`+genCol+`=? WHERE path=?`, evidence, generation, path) // #nosec G202 NOSONAR -- lane columns are fixed strings, values bound
	return err
}
