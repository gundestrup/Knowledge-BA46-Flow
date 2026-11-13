// The document_references / reference_versions tables: recorded link edges
// per source document, their current-version marker rows, and the inbound
// ("who links here") queries the analyzer's evidence sweep consumes. Link
// maintenance writes (deterministic fixes, link-issue findings) live here too.
package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"knowledge-ba46-flow/internal/frontmatter"
)

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

// InboundLinks returns the paths of documents whose frontmatter `related` or
// `contradicted_by` lists path — the "who links here" view used to detect
// missing backlinks. The LIKE prefilter may over-match; the Go verification
// on the parsed frontmatter keeps only exact list entries.
func (d *DB) InboundLinks(ctx context.Context, path string) ([]string, error) {
	if indexed, ready, err := d.IndexedInboundLinks(ctx, path); err != nil || ready {
		return indexed, err
	}
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

// fmLinksTo reports whether the document's declared graph targets contain
// target as an exact string entry (scalar or list).
func fmLinksTo(fields map[string]any, target string) bool {
	return slices.Contains(frontmatter.GraphLinkTargets(fields), target)
}

type Reference struct {
	WrittenTarget string `json:"written_target"`
	TargetPath    string `json:"target_path"`
	TargetID      string `json:"target_document_id"`
	Kind          string `json:"kind"`
	SourceSHA256  string `json:"source_sha256"`
}

type referenceKey struct {
	writtenTarget string
	targetPath    string
	targetID      string
	kind          string
}

type referenceIdentity struct {
	writtenTarget string
	kind          string
}

func normalizeReferences(refs []Reference) []Reference {
	positions := make(map[referenceIdentity]int, len(refs))
	normalized := make([]Reference, 0, len(refs))
	for _, ref := range refs {
		identity := referenceIdentity{ref.WrittenTarget, ref.Kind}
		if position, ok := positions[identity]; ok {
			normalized[position] = ref
			continue
		}
		positions[identity] = len(normalized)
		normalized = append(normalized, ref)
	}
	return normalized
}

func sameReferenceTopology(a, b []Reference) bool {
	if len(a) != len(b) {
		return false
	}
	keys := make(map[referenceKey]int, len(a))
	for _, ref := range a {
		keys[referenceKey{ref.WrittenTarget, ref.TargetPath, ref.TargetID, ref.Kind}]++
	}
	for _, ref := range b {
		key := referenceKey{ref.WrittenTarget, ref.TargetPath, ref.TargetID, ref.Kind}
		if keys[key] == 0 {
			return false
		}
		keys[key]--
	}
	return true
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
	refs = normalizeReferences(refs)
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT written_target,target_path,target_document_id,kind,source_sha256 FROM document_references WHERE source_path=?`, source)
	if err != nil {
		return err
	}
	var previous []Reference
	for rows.Next() {
		var ref Reference
		if err := rows.Scan(&ref.WrittenTarget, &ref.TargetPath, &ref.TargetID, &ref.Kind, &ref.SourceSHA256); err != nil {
			_ = rows.Close()
			return err
		}
		previous = append(previous, ref)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !sameReferenceTopology(previous, refs) {
		// Inbound-link lists are part of analysis context. A graph-only edit
		// does not change task hashes, so advance the evidence generation for
		// the dependents sweep to compare the recorded inbound sets.
		if err := bumpEvidenceGeneration(ctx, tx); err != nil {
			return err
		}
	}
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

// indexedInboundSources returns documents with indexed edges of the given
// kinds resolving to path. The readiness check is corpus-wide: one unindexed
// source makes the answer incomplete, so callers get ready=false rather than
// a partial result.
func (d *DB) indexedInboundSources(ctx context.Context, path string, kinds []string) ([]string, bool, error) {
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
	kindArgs := make([]any, 0, len(kinds))
	placeholders := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		kindArgs = append(kindArgs, kind)
		placeholders = append(placeholders, "?")
	}
	args := append([]any{path, path, id, id}, kindArgs...)
	rows, err := d.QueryContext(ctx, `SELECT DISTINCT r.source_path FROM document_references r
JOIN documents d ON d.path=r.source_path AND d.sha256=r.source_sha256 AND d.status<>'missing'
WHERE r.source_path<>? AND (r.target_path=? OR (?<>'' AND r.target_document_id=?))
AND r.kind IN (`+strings.Join(placeholders, ",")+`) ORDER BY r.source_path`, args...)
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

// OutboundReferenceCounts returns each source's indexed edge count — the
// hub/catalog signal: a page linking to dozens of documents is navigational,
// not a merge candidate.
func (d *DB) OutboundReferenceCounts(ctx context.Context) (map[string]int, error) {
	rows, err := d.QueryContext(ctx, `SELECT r.source_path, COUNT(*) FROM document_references r
JOIN documents d ON d.path=r.source_path AND d.sha256=r.source_sha256 AND d.status<>'missing'
GROUP BY r.source_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var source string
		var count int
		if err := rows.Scan(&source, &count); err != nil {
			return nil, err
		}
		out[source] = count
	}
	return out, rows.Err()
}

// IndexedInboundLinks returns the frontmatter graph linkers to path — the
// inbound evidence contract. Only frontmatter edges (GraphLinkKeys) count:
// body links are indexed for link repair, but a catalog/README table row is
// navigational, and letting it join the inbound set would make every index
// edit requeue its link targets for full analysis. This also keeps the
// indexed path identical in kind to the frontmatter fallback in InboundLinks.
func (d *DB) IndexedInboundLinks(ctx context.Context, path string) ([]string, bool, error) {
	return d.indexedInboundSources(ctx, path, frontmatter.GraphLinkKeys)
}

// AppliedGraphEdgeExists reports whether a graph edge from source to target
// was recorded by an accepted suggestion — i.e. the link is a reviewed human
// decision, not unexplained drift.
func (d *DB) AppliedGraphEdgeExists(ctx context.Context, source, target string) (bool, error) {
	kindArgs := make([]any, 0, len(frontmatter.GraphLinkKeys)+2)
	kindArgs = append(kindArgs, source, target)
	placeholders := make([]string, 0, len(frontmatter.GraphLinkKeys))
	for _, kind := range frontmatter.GraphLinkKeys {
		kindArgs = append(kindArgs, kind)
		placeholders = append(placeholders, "?")
	}
	var found int
	err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM applied_edges
WHERE source_path=? AND target_path=? AND kind IN (`+strings.Join(placeholders, ",")+`))`, kindArgs...).Scan(&found)
	return found != 0, err
}

// BodyInboundSources returns documents whose indexed body links resolve to
// path. Body edges are navigational — kept for repair but excluded from
// inbound evidence — so this exists to prove that a recorded inbound entry
// was bookkeeping, never to widen the evidence contract.
func (d *DB) BodyInboundSources(ctx context.Context, path string) ([]string, bool, error) {
	return d.indexedInboundSources(ctx, path, []string{"body"})
}
