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

	"knowledge-ba46-flow/internal/frontmatter"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" database/sql driver
)

// DB embeds *sql.DB so callers get the full database/sql API plus the helpers below.
type DB struct{ *sql.DB }

// Document mirrors a row of the documents table: a markdown file plus its
// analysis metadata. SHA256 is the strict canonical source version used for
// proposal staleness; the task input hashes omit graph-only links and include
// prompt/policy fingerprints. AnalyzedSHA256 and the sibling task markers
// record those evaluation inputs, while model fields preserve lane provenance.
// Signature is a marshaled similarity sketch used to rank merge/relation candidates by text overlap;
// Embedding is a marshaled float32 vector from the optional embed model,
// used to rank candidates by topical relatedness.
type Document struct {
	Path                   string `json:"path"`
	SHA256                 string `json:"sha256"`
	AnalyzedSHA256         string `json:"analyzed_sha256"`
	AnalyzedModel          string `json:"analyzed_model"`
	AnalyzedSemanticSHA256 string `json:"analyzed_semantic_sha256"`
	// ExternalSHA256/ExternalModel form the optional external lane: an
	// enhanced run (ollama_model_external, e.g. a cloud model) stamps these
	// instead of analyzed_*, so the local analysis lineage — and therefore
	// the next normal run's pending/drift view — is left untouched.
	ExternalSHA256         string `json:"external_sha256"`
	ExternalModel          string `json:"external_model"`
	ExternalSemanticSHA256 string `json:"external_semantic_sha256"`
	EnrichedSHA256         string `json:"enriched_sha256"`
	EnrichedModel          string `json:"enriched_model"`
	// FmSHA256/BodySHA256 split the strict canonical hash at the frontmatter
	// boundary for reporting source changes; they are not task invalidation keys.
	FmSHA256   string `json:"fm_sha256"`
	BodySHA256 string `json:"body_sha256"`
	// SemanticSHA256 omits graph-only frontmatter and internal Markdown link
	// destinations. SemanticBodySHA256 is the same projection of the body.
	// These drive task invalidation; SHA256 remains the strict source version
	// used to protect reviewed full-file proposals.
	SemanticSHA256         string `json:"semantic_sha256"`
	SemanticFrontmatterSHA string `json:"semantic_frontmatter_sha256"`
	SemanticBodySHA256     string `json:"semantic_body_sha256"`
	AnalysisInputSHA256    string `json:"analysis_input_sha256"`
	EnrichmentInputSHA256  string `json:"enrichment_input_sha256"`
	ExpandInputSHA256      string `json:"expand_input_sha256"`
	FreshnessInputSHA256   string `json:"freshness_input_sha256"`
	// Legacy*InputSHA256 carry the same fingerprints recomputed under the
	// previous recipe. UpsertScanned uses them to re-stamp lineages proven
	// current under the old recipe instead of requeueing them.
	LegacyAnalysisInputSHA256   string `json:"-"`
	LegacyEnrichmentInputSHA256 string `json:"-"`
	LegacyExpandInputSHA256     string `json:"-"`
	LegacyFreshnessInputSHA256  string `json:"-"`
	FreshnessWebVerify          bool   `json:"freshness_web_verify"`
	FreshnessVerifyModel        string `json:"freshness_verify_model"`
	FreshnessVerifyPromptSHA    string `json:"-"`
	// SeenFmSHA256/SeenBodySHA256 record what the document looked like when
	// local analysis last ran — the reference for the
	// changed-frontmatter-only vs changed-body breakdown.
	SeenFmSHA256   string `json:"seen_fm_sha256"`
	SeenBodySHA256 string `json:"seen_body_sha256"`
	// External lane keeps its own seen pair — measuring frontmatter drift
	// against the wrong lane's last analysis would silently under-fire the
	// external dependents sweep.
	SeenExternalFmSHA256   string `json:"-"`
	SeenExternalBodySHA256 string `json:"-"`
	// Lane-local acknowledgement for evidence invalidations. The backing
	// column retains its historical removal name, but advances for both
	// tombstones and reference-topology changes.
	SeenRemovalGeneration         int64 `json:"-"`
	SeenExternalRemovalGeneration int64 `json:"-"`
	// AnalysisEvidenceJSON records which other documents the local analysis
	// actually consumed: candidate paths/scores, path-only inbound/sibling
	// sets and rendered validation context. Empty for legacy rows.
	AnalysisEvidenceJSON string `json:"-"`
	// ExternalAnalysisEvidenceJSON keeps the independent external lane's
	// prompt evidence separate from local analysis.
	ExternalAnalysisEvidenceJSON string `json:"-"`
	// MissingSince is the scan timestamp at which the file disappeared —
	// retained for diagnostics; MissingGeneration is the monotonic removal
	// event identity used for lane-local acknowledgement.
	MissingSince      string `json:"-"`
	MissingGeneration int64  `json:"-"`
	// Expand/Freshness/Embedded lineage keep optional passes incremental against
	// their current input fingerprints; full source SHA remains available above.
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
	ID               int64   `json:"id"`
	DocumentPath     string  `json:"document_path"`
	SourceSHA256     string  `json:"source_sha256,omitempty"`
	Action           string  `json:"action"`
	TargetPath       string  `json:"target_path"`
	Reason           string  `json:"reason"`
	ProposedContent  string  `json:"proposed_content,omitempty"`
	Status           string  `json:"status"`
	Confidence       float64 `json:"confidence"`
	Origin           string  `json:"origin"`
	CreatedAt        string  `json:"created_at"`
	Error            string  `json:"error,omitempty"`
	AppliedAt        string  `json:"applied_at,omitempty"`
	ApplyBatchID     string  `json:"apply_batch_id,omitempty"`
	ApplyAttemptedAt string  `json:"apply_attempted_at,omitempty"`
	// DecisionNote carries the reviewer's "why" on reject/accept decisions —
	// the ground-truth feedback the analysis prompt shows back to the model.
	DecisionNote string `json:"decision_note,omitempty"`
	// CurrentContent/CurrentContentError are populated only by
	// GET /api/suggestions/{id} for the review diff view — the file on disk
	// now, or why it could not be read. Never persisted.
	CurrentContent      string   `json:"current_content,omitempty"`
	CurrentContentError string   `json:"current_content_error,omitempty"`
	ReviewState         string   `json:"review_state,omitempty"`
	ReviewReason        string   `json:"review_reason,omitempty"`
	ChangeTypes         []string `json:"change_types,omitempty"`
	DocumentID          string   `json:"document_id,omitempty"`
	AcceptedID          int64    `json:"accepted_id,omitempty"`
}

// KnowledgeDashboard summarizes current analysis work and the most recent
// successful full run cycle.
type KnowledgeDashboard struct {
	ChangedFiles         int `json:"changed_files"`
	ExternalChangedFiles int `json:"external_changed_files"`
	// ModelDrift counts documents whose last analysis ran under a model other
	// than the configured one — candidates for an explicit re-analyze run.
	ModelDrift         int    `json:"model_drift"`
	ExternalModelDrift int    `json:"external_model_drift"`
	LastCycleAt        string `json:"last_cycle_at"`
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

// UpsertScanned inserts a freshly scanned file or updates its source and
// semantic fingerprints. A strict source-hash change supersedes pending or
// accepted full-file proposals; only a semantic-input change resets task
// status/embeddings. Graph-only changes do not requeue content evaluations.
// A unique UUID (or exact hash without one) plus observed filesystem absence
// can identify a rename. Without absence evidence no rename is inferred.
// Returns whether the file was new or changed (i.e. needs re-analysis).
func scannedDefaults(doc Document) Document {
	if doc.SemanticSHA256 == "" {
		doc.SemanticSHA256 = doc.SHA256
	}
	if doc.SemanticFrontmatterSHA == "" {
		doc.SemanticFrontmatterSHA = doc.FmSHA256
	}
	if doc.SemanticBodySHA256 == "" {
		doc.SemanticBodySHA256 = doc.BodySHA256
	}
	if doc.AnalysisInputSHA256 == "" {
		doc.AnalysisInputSHA256 = doc.SemanticSHA256
	}
	if doc.EnrichmentInputSHA256 == "" {
		doc.EnrichmentInputSHA256 = doc.SemanticSHA256
	}
	if doc.ExpandInputSHA256 == "" {
		doc.ExpandInputSHA256 = doc.SemanticSHA256
	}
	if doc.FreshnessInputSHA256 == "" {
		doc.FreshnessInputSHA256 = doc.SemanticSHA256
	}
	return doc
}

func (d *DB) UpsertScanned(ctx context.Context, doc Document, seen string, absence ...func(string) bool) (bool, error) {
	doc = scannedDefaults(doc)
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	old, err := loadScannedSnapshot(ctx, tx, doc.Path)
	newPath := errors.Is(err, sql.ErrNoRows)
	if err != nil && !newPath {
		return false, err
	}
	if newPath {
		var absent func(string) bool
		if len(absence) > 0 {
			absent = absence[0]
		}
		renamed, rerr := renameObserved(ctx, tx, doc, seen, absent)
		if rerr != nil {
			return false, rerr
		}
		if renamed {
			old, err = loadScannedSnapshot(ctx, tx, doc.Path)
			if err != nil {
				return false, err
			}
			newPath = false
		}
	}
	changed := newPath || old.sha != doc.SHA256
	legacy, semanticSame := scannedSemanticsUnchanged(old, doc, newPath)
	semanticChanged := newPath || !semanticSame
	lineage := migrateLegacyLineage(old, doc, legacy && semanticSame)
	if err := upsertScannedRow(ctx, tx, doc, seen, old, newPath, semanticChanged, lineage); err != nil {
		return false, err
	}
	if err := retireStaleScannedSuggestions(ctx, tx, doc, changed); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return changed, nil
}

type scanSnapshot struct {
	sha, fmSHA, bodySHA, semanticSHA, semanticFM, semanticBody                  string
	analyzedSHA, externalSHA, enrichedSHA, expandSHA, freshnessSHA, embeddedSHA string
	analyzedSemantic, externalSemantic                                          string
	analysisInput, enrichmentInput, expandInput, freshnessInput                 string
	frontmatterJSON, status                                                     string
}

type scanLineage struct {
	analyzedSHA, externalSHA, enrichedSHA, expandSHA, freshnessSHA, embeddedSHA string
	analyzedSemantic, externalSemantic                                          string
}

func loadScannedSnapshot(ctx context.Context, tx *sql.Tx, path string) (scanSnapshot, error) {
	var old scanSnapshot
	err := tx.QueryRowContext(ctx, `SELECT sha256,fm_sha256,body_sha256,semantic_sha256,
semantic_frontmatter_sha256,semantic_body_sha256,analyzed_sha256,external_sha256,enriched_sha256,
expand_sha256,freshness_sha256,embedded_sha256,analyzed_semantic_sha256,external_semantic_sha256,
	frontmatter_json,status,analysis_input_sha256,enrichment_input_sha256,expand_input_sha256,
freshness_input_sha256
FROM documents WHERE path=?`, path).Scan(
		&old.sha, &old.fmSHA, &old.bodySHA, &old.semanticSHA, &old.semanticFM, &old.semanticBody,
		&old.analyzedSHA, &old.externalSHA, &old.enrichedSHA, &old.expandSHA,
		&old.freshnessSHA, &old.embeddedSHA, &old.analyzedSemantic, &old.externalSemantic,
		&old.frontmatterJSON, &old.status, &old.analysisInput, &old.enrichmentInput, &old.expandInput,
		&old.freshnessInput)
	return old, err
}

func scannedSemanticsUnchanged(old scanSnapshot, doc Document, newPath bool) (legacy, same bool) {
	if newPath {
		return false, false
	}
	legacy = old.semanticSHA == "" || old.semanticSHA == old.sha
	if !legacy {
		return false, old.semanticSHA == doc.SemanticSHA256
	}
	if old.sha == doc.SHA256 {
		return true, true
	}
	oldSemanticFM, ok := frontmatter.SemanticFrontmatterHashJSON(old.frontmatterJSON)
	return true, ok && oldSemanticFM == doc.SemanticFrontmatterSHA && old.bodySHA == doc.BodySHA256
}

func migrateLegacyLineage(old scanSnapshot, doc Document, semanticSame bool) scanLineage {
	lineage := scanLineage{
		analyzedSHA: old.analyzedSHA, externalSHA: old.externalSHA,
		enrichedSHA: old.enrichedSHA, expandSHA: old.expandSHA,
		freshnessSHA: old.freshnessSHA, embeddedSHA: old.embeddedSHA,
		analyzedSemantic: old.analyzedSemantic, externalSemantic: old.externalSemantic,
	}
	lineage = migrateTaskInputRecipe(old, doc, lineage)
	if !semanticSame {
		return lineage
	}
	if old.analyzedSHA == old.sha {
		lineage.analyzedSHA, lineage.analyzedSemantic = doc.AnalysisInputSHA256, doc.SemanticSHA256
	}
	if old.externalSHA == old.sha {
		lineage.externalSHA, lineage.externalSemantic = doc.AnalysisInputSHA256, doc.SemanticSHA256
	}
	if old.enrichedSHA == old.sha {
		lineage.enrichedSHA = doc.EnrichmentInputSHA256
	}
	if old.expandSHA == old.sha {
		lineage.expandSHA = doc.ExpandInputSHA256
	}
	if old.embeddedSHA == old.sha {
		lineage.embeddedSHA = doc.SemanticSHA256
	}
	return lineage
}

func migrateTaskInputRecipe(old scanSnapshot, doc Document, lineage scanLineage) scanLineage {
	// Recipe migration: a stored input equal to both the lane stamp and this
	// scan's old-recipe recomputation proves the run covered the same inputs;
	// restamp lineage under the current recipe instead of requeueing. Rows
	// already on the new recipe never equal the recomputed old value. The
	// recomputation embeds the semantic hash, so a changed document can never
	// satisfy this.
	if doc.LegacyAnalysisInputSHA256 != "" && old.analyzedSHA != "" &&
		old.analyzedSHA == old.analysisInput && old.analysisInput == doc.LegacyAnalysisInputSHA256 {
		lineage.analyzedSHA, lineage.analyzedSemantic = doc.AnalysisInputSHA256, doc.SemanticSHA256
	}
	if doc.LegacyAnalysisInputSHA256 != "" && old.externalSHA != "" &&
		old.externalSHA == old.analysisInput && old.analysisInput == doc.LegacyAnalysisInputSHA256 {
		lineage.externalSHA, lineage.externalSemantic = doc.AnalysisInputSHA256, doc.SemanticSHA256
	}
	if doc.LegacyEnrichmentInputSHA256 != "" && old.enrichedSHA != "" &&
		old.enrichedSHA == old.enrichmentInput && old.enrichmentInput == doc.LegacyEnrichmentInputSHA256 {
		lineage.enrichedSHA = doc.EnrichmentInputSHA256
	}
	if doc.LegacyExpandInputSHA256 != "" && old.expandSHA != "" &&
		old.expandSHA == old.expandInput && old.expandInput == doc.LegacyExpandInputSHA256 {
		lineage.expandSHA = doc.ExpandInputSHA256
	}
	if doc.LegacyFreshnessInputSHA256 != "" && old.freshnessSHA != "" &&
		old.freshnessSHA == old.freshnessInput && old.freshnessInput == doc.LegacyFreshnessInputSHA256 {
		lineage.freshnessSHA = doc.FreshnessInputSHA256
	}
	return lineage
}

func upsertScannedRow(ctx context.Context, tx *sql.Tx, doc Document, seen string, old scanSnapshot, newPath, semanticChanged bool, lineage scanLineage) error {
	analysisCurrent := lineage.analyzedSHA == doc.AnalysisInputSHA256
	newStatus := old.status
	if newPath || semanticChanged {
		newStatus = "pending"
	} else if old.status == "missing" {
		newStatus = "pending"
		if analysisCurrent {
			newStatus = "analyzed"
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO documents(
path,sha256,size,modified_unix,tags_json,frontmatter_json,frontmatter_issues,sig,status,
first_seen_at,last_seen_at,fm_sha256,body_sha256,semantic_sha256,semantic_frontmatter_sha256,
semantic_body_sha256,analysis_input_sha256,enrichment_input_sha256,expand_input_sha256,
freshness_input_sha256,analyzed_sha256,external_sha256,enriched_sha256,expand_sha256,
freshness_sha256,embedded_sha256,analyzed_semantic_sha256,external_semantic_sha256)
VALUES(?,?,?,?,?,?,?,?,'pending',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(path) DO UPDATE SET sha256=excluded.sha256,fm_sha256=excluded.fm_sha256,
body_sha256=excluded.body_sha256,semantic_sha256=excluded.semantic_sha256,
semantic_frontmatter_sha256=excluded.semantic_frontmatter_sha256,
semantic_body_sha256=excluded.semantic_body_sha256,
analysis_input_sha256=excluded.analysis_input_sha256,
enrichment_input_sha256=excluded.enrichment_input_sha256,
expand_input_sha256=excluded.expand_input_sha256,
freshness_input_sha256=excluded.freshness_input_sha256,
analyzed_sha256=?,external_sha256=?,enriched_sha256=?,expand_sha256=?,freshness_sha256=?,
embedded_sha256=?,analyzed_semantic_sha256=?,external_semantic_sha256=?,
size=excluded.size,modified_unix=excluded.modified_unix,
tags_json=CASE WHEN ? THEN excluded.tags_json ELSE documents.tags_json END,
frontmatter_json=excluded.frontmatter_json,frontmatter_issues=excluded.frontmatter_issues,
sig=excluded.sig,embedding=CASE WHEN ? THEN NULL ELSE documents.embedding END,
last_seen_at=excluded.last_seen_at,status=?,missing_since='',last_error=CASE WHEN ? THEN '' ELSE documents.last_error END`,
		doc.Path, doc.SHA256, doc.Size, doc.ModifiedUnix, doc.TagsJSON, doc.FrontmatterJSON,
		doc.FrontmatterIssues, doc.Signature, seen, seen, doc.FmSHA256, doc.BodySHA256,
		doc.SemanticSHA256, doc.SemanticFrontmatterSHA, doc.SemanticBodySHA256,
		doc.AnalysisInputSHA256, doc.EnrichmentInputSHA256, doc.ExpandInputSHA256,
		doc.FreshnessInputSHA256, lineage.analyzedSHA, lineage.externalSHA, lineage.enrichedSHA,
		lineage.expandSHA, lineage.freshnessSHA, lineage.embeddedSHA, lineage.analyzedSemantic,
		lineage.externalSemantic, lineage.analyzedSHA, lineage.externalSHA, lineage.enrichedSHA,
		lineage.expandSHA, lineage.freshnessSHA, lineage.embeddedSHA, lineage.analyzedSemantic,
		lineage.externalSemantic, semanticChanged, semanticChanged, newStatus, semanticChanged)
	return err
}

func retireStaleScannedSuggestions(ctx context.Context, tx *sql.Tx, doc Document, changed bool) error {
	if !changed {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE suggestions SET status='superseded',error='source content changed during scan; review a new proposal',
decided_at=COALESCE(decided_at,?) WHERE document_path=? AND status IN ('pending','accepted') AND source_sha256<>?`,
		time.Now().UTC().Format(time.RFC3339), doc.Path, doc.SHA256)
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
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT path FROM documents WHERE last_seen_at<>? AND status<>'missing'`, seen)
	if err != nil {
		return err
	}
	newlyMissing := 0
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			_ = rows.Close()
			return err
		}
		newlyMissing++
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if newlyMissing > 0 {
		if err := bumpEvidenceGeneration(ctx, tx); err != nil {
			return err
		}
		var generation int64
		if err := tx.QueryRowContext(ctx, `SELECT removal_generation FROM evidence_state WHERE id=1`).Scan(&generation); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE documents SET status='missing', missing_since=?, missing_generation=? WHERE last_seen_at<>? AND status<>'missing'`, seen, generation, seen); err != nil {
			return err
		}
	}
	// A missing document's outbound references are dead weight — its rows are
	// never rescanned, so drop them; resurrection recreates them on the next
	// walk. Read paths already filter missing sources; this is hygiene, not
	// correctness.
	if _, err := tx.ExecContext(ctx, `DELETE FROM document_references WHERE source_path IN (SELECT path FROM documents WHERE status='missing')`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM reference_versions WHERE source_path IN (SELECT path FROM documents WHERE status='missing')`); err != nil {
		return err
	}
	return tx.Commit()
}

// documentColumns is the shared SELECT list for Document rows.
const documentColumns = `path,sha256,analyzed_sha256,analyzed_model,analyzed_semantic_sha256,external_sha256,external_model,external_semantic_sha256,enriched_sha256,enriched_model,fm_sha256,body_sha256,semantic_sha256,semantic_frontmatter_sha256,semantic_body_sha256,analysis_input_sha256,enrichment_input_sha256,expand_input_sha256,freshness_input_sha256,freshness_web_verify,freshness_verify_model,freshness_verify_prompt_sha256,seen_fm_sha256,seen_body_sha256,expand_sha256,expand_model,freshness_sha256,freshness_model,embedded_sha256,embedded_model,size,modified_unix,title,summary,tags_json,status,last_error,frontmatter_json,frontmatter_issues,link_issues,analysis_evidence_json,external_analysis_evidence_json,seen_external_fm_sha256,seen_external_body_sha256,seen_removal_generation,seen_external_removal_generation,missing_since,missing_generation,sig,embedding`

// scanDocument reads one Document row in documentColumns order.
func scanDocument(rows *sql.Rows) (Document, error) {
	var x Document
	err := rows.Scan(&x.Path, &x.SHA256, &x.AnalyzedSHA256, &x.AnalyzedModel, &x.AnalyzedSemanticSHA256, &x.ExternalSHA256, &x.ExternalModel, &x.ExternalSemanticSHA256, &x.EnrichedSHA256, &x.EnrichedModel, &x.FmSHA256, &x.BodySHA256, &x.SemanticSHA256, &x.SemanticFrontmatterSHA, &x.SemanticBodySHA256, &x.AnalysisInputSHA256, &x.EnrichmentInputSHA256, &x.ExpandInputSHA256, &x.FreshnessInputSHA256, &x.FreshnessWebVerify, &x.FreshnessVerifyModel, &x.FreshnessVerifyPromptSHA, &x.SeenFmSHA256, &x.SeenBodySHA256, &x.ExpandSHA256, &x.ExpandModel, &x.FreshnessSHA256, &x.FreshnessModel, &x.EmbeddedSHA256, &x.EmbeddedModel, &x.Size, &x.ModifiedUnix, &x.Title, &x.Summary, &x.TagsJSON, &x.Status, &x.LastError, &x.FrontmatterJSON, &x.FrontmatterIssues, &x.LinkIssues, &x.AnalysisEvidenceJSON, &x.ExternalAnalysisEvidenceJSON, &x.SeenExternalFmSHA256, &x.SeenExternalBodySHA256, &x.SeenRemovalGeneration, &x.SeenExternalRemovalGeneration, &x.MissingSince, &x.MissingGeneration, &x.Signature, &x.Embedding)
	return x, err
}

// Pending returns documents whose semantic/prompt input differs from the
// last analysis or enrichment input in the local lane, and the file is not missing. The
// enrichment condition only applies when fastModel is a genuinely different
// model — a single configured model never schedules the cheaper pass
// separately. With reanalyze, documents last processed under a different
// model are included. forceAll explicitly includes every non-missing
// document, for example after a prompt or taxonomy policy change; both
// options stay opt-in.
func (d *DB) Pending(ctx context.Context, analysisModel, fastModel string, reanalyze bool, forceAll ...bool) ([]Document, error) {
	twoModel := fastModel != "" && fastModel != analysisModel
	return d.pending(ctx, "analysis_input_sha256", "analyzed_sha256", "analyzed_model", analysisModel, "enrichment_input_sha256", "enriched_sha256", fastModel, twoModel, reanalyze, forceAll...)
}

// PendingExternal is Pending for the external lane: an enhanced run
// (ollama_model_external) is incremental against its own external_sha256/
// external_model markers instead of the local analyzed_* ones. The
// enrichment stage is shared between lanes, so enrichOn reports whether the
// local config has a distinct fast pass — not whether fastModel differs from
// the external model.
func (d *DB) PendingExternal(ctx context.Context, analysisModel, fastModel string, enrichOn, reanalyze bool, forceAll ...bool) ([]Document, error) {
	return d.pending(ctx, "analysis_input_sha256", "external_sha256", "external_model", analysisModel, "enrichment_input_sha256", "enriched_sha256", fastModel, enrichOn && fastModel != "", reanalyze, forceAll...)
}

// pending builds the shared pending-work condition: a document queues when
// its current input fingerprint differs from the lane stamp, when its
// enrichment input differs from the enrichment stamp (twoModel only), or
// when reanalyze is requested and the stamping model differs.
func (d *DB) pending(ctx context.Context, inputCol, stampCol, modelCol, analysisModel, enrichInputCol, enrichStampCol, fastModel string, twoModel, reanalyze bool, forceAll ...bool) ([]Document, error) {
	if len(forceAll) > 0 && forceAll[0] {
		return d.documentsWhere(ctx, `1=1`, nil)
	}
	cond := inputCol + `<>` + stampCol
	var args []any
	if twoModel {
		cond += ` OR ` + enrichInputCol + `<>` + enrichStampCol
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

// PendingEnrich returns documents whose semantic/prompt input differs from
// the last enrichment stamp, or (with reanalyze) whose stamp came from another
// model.
func (d *DB) PendingEnrich(ctx context.Context, fastModel string, reanalyze bool, forceAll ...bool) ([]Document, error) {
	if len(forceAll) > 0 && forceAll[0] {
		return d.documentsWhere(ctx, `1=1`, nil)
	}
	cond := `enrichment_input_sha256<>enriched_sha256`
	var args []any
	if reanalyze {
		cond += ` OR enriched_model<>?`
		args = append(args, fastModel)
	}
	return d.documentsWhere(ctx, `(`+cond+`)`, args)
}

// taskColumns maps an optional task to its current input fingerprint, stamp and
// model columns. All tasks use semantic input; their fingerprint also includes
// the relevant static prompt/policy settings.
func taskColumns(task string) (cur, sha, model string, err error) {
	switch task {
	case "expand":
		return "expand_input_sha256", "expand_sha256", "expand_model", nil
	case "freshness":
		return "freshness_input_sha256", "freshness_sha256", "freshness_model", nil
	case "embed":
		return "semantic_sha256", "embedded_sha256", "embedded_model", nil
	}
	return "", "", "", fmt.Errorf("unknown task %q", task)
}

// taskCurrent returns the document field matching taskColumns' current column.
func taskCurrent(doc Document, task string) string {
	switch task {
	case "expand":
		return firstNonempty(doc.ExpandInputSHA256, fallbackSemanticSHA(doc))
	case "freshness":
		return firstNonempty(doc.FreshnessInputSHA256, fallbackSemanticSHA(doc))
	case "embed":
		return firstNonempty(doc.SemanticSHA256, doc.SHA256)
	}
	return ""
}

func fallbackSemanticSHA(doc Document) string {
	if doc.SemanticSHA256 != "" {
		return doc.SemanticSHA256
	}
	return doc.SHA256
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// PendingTask returns documents whose task lineage is behind the current
// content — the task run is incremental, exactly like the analysis lanes.
type TaskRunOptions struct {
	WebVerify       bool
	VerifyModel     string
	VerifyPromptSHA string
}

func (d *DB) PendingTask(ctx context.Context, task, model string, reanalyze bool, forceAll ...bool) ([]Document, error) {
	return d.pendingTask(ctx, task, model, reanalyze, TaskRunOptions{}, forceAll...)
}

func (d *DB) PendingTaskForRun(ctx context.Context, task, model string, reanalyze bool, options TaskRunOptions, forceAll ...bool) ([]Document, error) {
	return d.pendingTask(ctx, task, model, reanalyze, options, forceAll...)
}

func (d *DB) pendingTask(ctx context.Context, task, model string, reanalyze bool, opts TaskRunOptions, forceAll ...bool) ([]Document, error) {
	cur, sha, modelCol, err := taskColumns(task)
	if err != nil {
		return nil, err
	}
	if len(forceAll) > 0 && forceAll[0] {
		return d.documentsWhere(ctx, `1=1`, nil)
	}
	cond := cur + `<>` + sha
	var args []any
	if task == "freshness" && opts.WebVerify {
		cond += ` OR freshness_web_verify=0 OR freshness_verify_model<>? OR freshness_verify_prompt_sha256<>?`
		args = append(args, opts.VerifyModel, opts.VerifyPromptSHA)
	}
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
		err := d.QueryRowContext(ctx, `SELECT `+col+`,count(*) FROM documents WHERE `+col+`<>'' AND status<>'missing' GROUP BY `+col+` ORDER BY 2 DESC LIMIT 1`).Scan(&stamp.Model, &stamp.Docs) // #nosec G202 NOSONAR -- columns are compile-time constants
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
	inputSHA := taskCurrent(doc, task)
	query := `UPDATE documents SET ` + shaCol + `=?,` + modelCol + `=?` // #nosec G202 -- columns are compile-time constants from taskColumns
	args := []any{inputSHA, model}
	if task == "freshness" {
		query += `,freshness_web_verify=?,freshness_verify_model=?,
freshness_verify_prompt_sha256=CASE WHEN ? THEN ? ELSE freshness_verify_prompt_sha256 END`
		args = append(args, doc.FreshnessWebVerify, doc.FreshnessVerifyModel,
			doc.FreshnessWebVerify, doc.FreshnessVerifyPromptSHA)
	}
	query += ` WHERE path=?`
	args = append(args, doc.Path)
	if _, err = tx.ExecContext(ctx, query, args...); err != nil { // #nosec G202 NOSONAR -- columns are compile-time constants from taskColumns, values are bound args
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
	inputSHA := taskCurrent(doc, task)
	query := `UPDATE documents SET ` + shaCol + `=?,` + modelCol + `=?` // #nosec G202 -- columns are compile-time constants from taskColumns
	args := []any{inputSHA, model}
	if task == "freshness" {
		query += `,freshness_web_verify=?,freshness_verify_model=?,
freshness_verify_prompt_sha256=CASE WHEN ? THEN ? ELSE freshness_verify_prompt_sha256 END`
		args = append(args, doc.FreshnessWebVerify, doc.FreshnessVerifyModel,
			doc.FreshnessWebVerify, doc.FreshnessVerifyPromptSHA)
	}
	query += ` WHERE path=?`
	args = append(args, doc.Path)
	_, err = d.ExecContext(ctx, query, args...) // #nosec G202 NOSONAR -- columns are compile-time constants from taskColumns, values are bound args
	return err
}

func (d *DB) documentsWhere(ctx context.Context, condition string, args []any) ([]Document, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+documentColumns+` FROM documents WHERE `+condition+` AND status<>'missing' ORDER BY path`, args...) // #nosec G202 NOSONAR -- column list and condition fragments are compile-time constants, values are bound args
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

// truncateRunes bounds a free-text field at max runes — reviewer notes and
// error strings stay small enough to quote in a prompt or a job detail.
func truncateRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit])
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
// tags, analysis input lineage, status='analyzed' — and inserts any actionable
// suggestions as 'pending'. 'none' suggestions are dropped. The transaction
// makes it all-or-nothing: a crash can't leave half-saved state.
func (d *DB) SaveAnalysis(ctx context.Context, doc Document, model, title, summary string, tags []string, suggestions []Suggestion, results ...StageResult) error {
	return d.saveAnalysis(ctx, doc,
		"analyzed_sha256", "analyzed_model", "analyzed_semantic_sha256",
		"seen_fm_sha256", "seen_body_sha256", "analysis_evidence_json", "seen_removal_generation",
		model, title, summary, tags, suggestions, results...)
}

// SaveExternalAnalysis is SaveAnalysis for the external lane: an enhanced run
// stamps external input/model lineage and leaves the local analyzed_*
// markers untouched, so the next normal run does not see the whole corpus as
// new or model-drifted. Suggestion handling stays identical — pending
// 'analysis' suggestions are superseded and the new ones are inserted.
func (d *DB) SaveExternalAnalysis(ctx context.Context, doc Document, model, title, summary string, tags []string, suggestions []Suggestion, results ...StageResult) error {
	return d.saveAnalysis(ctx, doc,
		"external_sha256", "external_model", "external_semantic_sha256",
		"seen_external_fm_sha256", "seen_external_body_sha256", "external_analysis_evidence_json", "seen_external_removal_generation",
		model, title, summary, tags, suggestions, results...)
}

func (d *DB) saveAnalysis(ctx context.Context, doc Document, shaCol, modelCol, semanticCol, seenFmCol, seenBodyCol, evidenceCol, seenRemovalCol, model, title, summary string, tags []string, suggestions []Suggestion, results ...StageResult) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	tj, _ := json.Marshal(tags)
	inputSHA := doc.AnalysisInputSHA256
	if inputSHA == "" {
		inputSHA = fallbackSemanticSHA(doc)
	}
	seenRemovalGeneration := doc.SeenRemovalGeneration
	if seenRemovalCol == "seen_external_removal_generation" {
		seenRemovalGeneration = doc.SeenExternalRemovalGeneration
	}
	if _, err = tx.ExecContext(ctx, `UPDATE documents SET `+shaCol+`=?,`+modelCol+`=?,`+semanticCol+`=?,title=?,summary=?,tags_json=?,status='analyzed',last_error='',analyzed_at=?,`+seenFmCol+`=?,`+seenBodyCol+`=?,`+evidenceCol+`=?,`+seenRemovalCol+`=? WHERE path=?`, // #nosec G202 NOSONAR -- columns are compile-time constants, never user input
		inputSHA, model, fallbackSemanticSHA(doc), title, summary, string(tj), time.Now().UTC().Format(time.RFC3339), doc.FmSHA256, doc.BodySHA256, doc.AnalysisEvidenceJSON, seenRemovalGeneration, doc.Path); err != nil {
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
	inputSHA := doc.EnrichmentInputSHA256
	if inputSHA == "" {
		inputSHA = fallbackSemanticSHA(doc)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE documents SET enriched_sha256=?,enriched_model=?,
title=CASE WHEN analyzed_sha256=analysis_input_sha256 THEN title ELSE ? END,
summary=CASE WHEN analyzed_sha256=analysis_input_sha256 THEN summary ELSE ? END,
tags_json=CASE WHEN analyzed_sha256=analysis_input_sha256 THEN tags_json ELSE ? END WHERE path=?`,
		inputSHA, model, title, summary, string(tj), doc.Path); err != nil {
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
	err := d.QueryRowContext(ctx, `SELECT id,document_path,source_sha256,action,target_path,reason,confidence,proposed_content,status,origin,created_at,decision_note,error,COALESCE(applied_at,''),apply_batch_id,apply_attempted_at,COALESCE((SELECT lower(json_extract(d.frontmatter_json,'$.document_id')) FROM documents d WHERE d.path=suggestions.document_path AND json_valid(d.frontmatter_json)),'') FROM suggestions WHERE id=?`, id).
		Scan(&s.ID, &s.DocumentPath, &s.SourceSHA256, &s.Action, &s.TargetPath, &s.Reason, &s.Confidence, &s.ProposedContent, &s.Status, &s.Origin, &s.CreatedAt, &s.DecisionNote, &s.Error, &s.AppliedAt, &s.ApplyBatchID, &s.ApplyAttemptedAt, &s.DocumentID)
	return s, err
}

// Documents lists every tracked document, ordered by path — used by the HTTP API.
func (d *DB) Documents(ctx context.Context) ([]Document, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+documentColumns+` FROM documents ORDER BY path`) // #nosec G202 NOSONAR -- column list is a compile-time constant, never user input
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
func (d *DB) KnowledgeDashboard(ctx context.Context, model string, externalModels ...string) (KnowledgeDashboard, error) {
	var overview KnowledgeDashboard
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents
WHERE analysis_input_sha256<>analyzed_sha256 AND status<>'missing'`).Scan(&overview.ChangedFiles); err != nil {
		return KnowledgeDashboard{}, err
	}
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents
WHERE analysis_input_sha256<>external_sha256 AND status<>'missing'`).Scan(&overview.ExternalChangedFiles); err != nil {
		return KnowledgeDashboard{}, err
	}
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents
WHERE analyzed_model<>? AND analysis_input_sha256=analyzed_sha256 AND status<>'missing'`, model).Scan(&overview.ModelDrift); err != nil {
		return KnowledgeDashboard{}, err
	}
	if len(externalModels) > 0 && externalModels[0] != "" {
		if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents
WHERE external_model<>? AND analysis_input_sha256=external_sha256 AND status<>'missing'`,
			externalModels[0]).Scan(&overview.ExternalModelDrift); err != nil {
			return KnowledgeDashboard{}, err
		}
	}
	err := d.QueryRowContext(ctx, `SELECT COALESCE((
SELECT finished_at FROM jobs
WHERE op='run' AND status='done' AND finished_at IS NOT NULL
ORDER BY id DESC LIMIT 1
), '')`).Scan(&overview.LastCycleAt)
	return overview, err
}

// suggestions is the shared query helper behind PendingSuggestions and
// Accepted. Active suggestions whose source document is missing are excluded —
// MarkMissing retires the document row but keeps it (and its suggestions)
// for resurrection, so a vanished file must not keep feeding the review and
// apply queues. If the file reappears its suggestions resurface still valid.
func (d *DB) suggestions(ctx context.Context, status string) ([]Suggestion, error) {
	rows, err := d.QueryContext(ctx, `SELECT id,document_path,source_sha256,action,target_path,reason,confidence,proposed_content,status,origin,created_at,decision_note,error,COALESCE(applied_at,''),apply_batch_id,apply_attempted_at,COALESCE((SELECT lower(json_extract(d.frontmatter_json,'$.document_id')) FROM documents d WHERE d.path=suggestions.document_path AND json_valid(d.frontmatter_json)),'') FROM suggestions WHERE status=? AND (? NOT IN ('pending','accepted') OR EXISTS(SELECT 1 FROM documents d WHERE d.path=suggestions.document_path AND d.status<>'missing')) ORDER BY id`, status, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Suggestion{} // non-nil so JSON responses are [] rather than null
	for rows.Next() {
		var s Suggestion
		if err := rows.Scan(&s.ID, &s.DocumentPath, &s.SourceSHA256, &s.Action, &s.TargetPath, &s.Reason, &s.Confidence, &s.ProposedContent, &s.Status, &s.Origin, &s.CreatedAt, &s.DecisionNote, &s.Error, &s.AppliedAt, &s.ApplyBatchID, &s.ApplyAttemptedAt, &s.DocumentID); err != nil {
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
func (d *DB) MarkSuperseded(ctx context.Context, id int64, reasons ...string) error {
	reason := "source document changed after analysis; review a new proposal"
	if len(reasons) > 0 {
		reason = reasons[0]
	}
	_, err := d.ExecContext(ctx, `UPDATE suggestions SET status='superseded',error=CASE WHEN error='' THEN ? ELSE error END,decided_at=COALESCE(decided_at,?) WHERE id=? AND status IN ('pending','accepted')`, reason, time.Now().UTC().Format(time.RFC3339), id)
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
	days := map[string]*KnowledgeProgressDay{} // nosemgrep: trailofbits.go.iterate-over-empty-map.iterate-over-empty-map — progressDayCounts fills it
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
