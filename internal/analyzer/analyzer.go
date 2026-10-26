// Package analyzer runs the two-stage LLM pipeline over changed documents:
// an optional cheap enrichment pass (ollama_model_fast) that writes title,
// summary, tags and a frontmatter patch, followed by the full analysis pass
// that produces reviewed move/merge/expand/delete suggestions. Mechanical
// frontmatter repairs and merge-candidate ranking are computed deterministi-
// cally in Go — the models never have to guess at corpus structure.
package analyzer

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/links"
	"knowledge-ba46-flow/internal/prompts"
	"knowledge-ba46-flow/internal/scanner"
	"knowledge-ba46-flow/internal/similarity"
	"knowledge-ba46-flow/internal/taxonomy"
	"knowledge-ba46-flow/internal/websearch"
)

// Generator produces a completion for a prompt; *ollama.Client implements it.
// Defined as an interface so tests (and future LLM backends) can substitute a
// fake — the analyzer never depends on HTTP.
type Generator interface {
	Generate(ctx context.Context, prompt string, format json.RawMessage) (string, error)
}

// Searcher answers one bounded web search for evidence about a claim —
// implemented by *websearch.Client. It is a deliberate egress boundary (the
// hosted API, not the local model server), so a client is only attached when
// a run explicitly opts into verification.
type Searcher interface {
	Search(ctx context.Context, query string, maxResults int) ([]websearch.Result, error)
}

// Embedder produces a semantic embedding vector for one text — implemented
// by *ollama.Client (ollama_embed_model). May be nil; the analyzer then falls
// back to shingle-similarity ranking only.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Generators carries the model clients for the two passes. Enrich may be nil
// (or the same client as Analysis) — the enrichment pass only runs when a
// distinct ollama_model_fast is configured anyway. Embed may be nil. Search
// and Verify may be nil; the freshness verify stage requires both when
// RunOptions.WebVerify is set — Verify is the model that re-judges flags
// against the search evidence, independently routed from Analysis via
// ollama_task_models.verify.
type Generators struct {
	Analysis Generator
	Enrich   Generator
	Embed    Embedder
	Search   Searcher
	Verify   Generator
}

// llmSuggestion and llmResult mirror the JSON contract defined in prompts/analyze.md.
type llmSuggestion struct {
	Action          string  `json:"action"`
	TargetPath      string  `json:"target_path"`
	Reason          string  `json:"reason"`
	Confidence      float64 `json:"confidence"`
	ProposedContent string  `json:"proposed_content"`
}

type llmResult struct {
	Title       string          `json:"title"`
	Summary     string          `json:"summary"`
	Tags        []string        `json:"tags"`
	Obsolete    bool            `json:"obsolete"`
	Patch       *metadataPatch  `json:"frontmatter_patch"`
	Suggestions []llmSuggestion `json:"suggestions"`
}

type metadataPatch struct {
	Set    map[string]any `json:"set"`
	Remove []string       `json:"remove"`
}

// llmEnrich mirrors the contract in prompts/enrich.md: metadata plus an optional
// frontmatter patch the analyzer applies, validates and offers as a normal
// suggestion for human review.
type llmEnrich struct {
	Title   *string        `json:"title"`
	Summary *string        `json:"summary"`
	Tags    *[]string      `json:"tags"`
	Patch   *metadataPatch `json:"frontmatter_patch"`
}

// Result reports analyzed documents, local-policy and stub skips, and
// failures in one run.
type Result struct {
	Processed        int
	Enriched         int // documents enriched by the fast model
	LocalEnriched    int // documents handled by the deterministic metadata stage
	Skipped          int
	Stubs            int // stub-tagged placeholders; skipped, still valid link targets
	Relinked         int // deterministic retarget fixes queued after renames
	Failed           int
	Rejected         int // unsafe model proposals omitted from review
	EnrichmentFailed int // optional generation failures; full analysis can still succeed
}

// Summary keeps CLI and HTTP job reporting consistent about optional failures.
func (r Result) Summary(model, fastModel string) string {
	return fmt.Sprintf("Analyzed %d; fast-enriched %d; locally enriched %d; skipped %d by MCP policy; failed %d (model %s, fast %s); enrichment failures %d (retryable); rejected %d unsafe edit(s); skipped %d stub placeholder(s).",
		r.Processed, r.Enriched, r.LocalEnriched, r.Skipped, r.Failed, model, fastModel, r.EnrichmentFailed, r.Rejected, r.Stubs)
}

// Progress reports one step of document analysis. Outcome is "enriching" or
// "analyzing" when a pass starts, then a local/model completion, "analyzed",
// "skipped" or "error". "enrichment_error" is a retryable warning, not a
// failed full analysis. Detail carries a short note (suggestion count,
// skip reason or error message). Long runs use it to show forward motion
// instead of silence.
type Progress struct {
	Index   int // 1-based position in the pending queue
	Total   int
	Path    string
	Outcome string
	Detail  string
}

// String formats a one-line progress summary for job rows and logs.
func (p Progress) String() string {
	if p.Outcome == "analyzing" || p.Outcome == "enriching" {
		return fmt.Sprintf("%s %d/%d: %s", p.Outcome, p.Index, p.Total, p.Path)
	}
	if p.Detail != "" {
		return fmt.Sprintf("%s %d/%d: %s (%s)", p.Outcome, p.Index, p.Total, p.Path, p.Detail)
	}
	return fmt.Sprintf("%s %d/%d: %s", p.Outcome, p.Index, p.Total, p.Path)
}

// analysisSchema is passed as Ollama's `format` parameter, activating
// structured-outputs mode so the model's reply is constrained to the llmResult
// shape — far more reliable than relying on prompt instructions alone.
var analysisSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "title":    {"type": "string"},
    "summary":  {"type": "string"},
    "tags":     {"type": "array", "items": {"type": "string"}},
    "obsolete": {"type": "boolean"},
    "frontmatter_patch": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "set": {
          "type": "object",
          "additionalProperties": false,
          "properties": {
            "title": {"type": "string"},
            "tags": {"type": "array", "items": {"type": "string"}},
            "related": {"type": "array", "items": {"type": "string"}},
            "contradicted_by": {"type": "array", "items": {"type": "string"}}
          }
        },
        "remove": {"type": "array", "items": {"type": "string", "enum": ["title", "tags", "related", "contradicted_by"]}}
      },
      "required": ["set", "remove"]
    },
    "suggestions": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "action":           {"type": "string", "enum": ["none", "move", "merge", "expand", "delete"]},
          "target_path":      {"type": "string"},
          "reason":           {"type": "string"},
          "confidence":       {"type": "number", "minimum": 0, "maximum": 1},
          "proposed_content": {"type": "string"}
        },
        "required": ["action", "target_path", "reason", "confidence", "proposed_content"]
      }
    }
  },
  "required": ["title", "summary", "tags", "obsolete", "frontmatter_patch", "suggestions"]
}`)

// enrichSchema constrains the fast pass to metadata plus a frontmatter patch.
var enrichSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "title":   {"type": "string"},
    "summary": {"type": "string"},
    "tags":    {"type": "array", "items": {"type": "string"}},
    "frontmatter_patch": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "set": {
          "type": "object",
          "additionalProperties": false,
          "properties": {
            "title": {"type": "string"},
            "tags": {"type": "array", "items": {"type": "string"}}
          }
        },
        "remove": {"type": "array", "items": {"type": "string", "enum": ["title", "tags"]}}
      },
      "required": ["set", "remove"]
    }
  },
  "required": ["title", "summary", "tags"]
}`)

// candidateLimit caps how many corpus entries the prompt lists. With
// similarity ranking the top of the list is genuinely relevant, so far fewer
// than the old 200 alphabetical paths are needed.
const candidateLimit = 16

// overlapThreshold is the estimated Jaccard above which a pair is flagged as
// a probable duplicate in the prompt context.
const overlapThreshold = 0.5

// evidenceNeighborLimit caps how many dependent notes one changed document
// can pull into the evidence sweep — candidate-list sized, so a single
// changed note can never requeue more than its own closest neighborhood.
const evidenceNeighborLimit = 16

// evidenceFloorQuantile selects the corpus-wide cosine percentile a pair must
// clear before an embedding counts as evidence of dependency. The floor is
// calibrated per run against the stored vectors, because cosine distributions
// are model-specific: an absolute threshold tuned on one model is a
// pass-through on a denser one and a wall on a sparser one.
const evidenceFloorQuantile = 0.95

// evidenceFloorSamplePairs caps the sampled pair count used for the floor
// calibration so large repositories stay cheap — O(sample), not O(n²) cosines.
const evidenceFloorSamplePairs = 60000

// Run analyses every pending document with a single generator — equivalent to
// RunProgress with only the analysis pass configured.
func Run(ctx context.Context, c config.Config, db *database.DB, client Generator) (Result, error) {
	return RunProgress(ctx, c, db, Generators{Analysis: client}, RunOptions{})
}

// RunOptions tunes a Run: Progress receives per-document events (may be nil);
// Reanalyze handles model drift, while ReanalyzeAll explicitly forces every
// non-missing document through both stages after a prompt/taxonomy change.
// External runs the analysis pass with ollama_model_external (the "enhanced"
// lane): results stamp external_sha256/external_model instead of analyzed_*,
// so the local lineage — and the next normal run's pending/drift view — is
// left untouched. Model is a per-run override naming any configured model —
// when set it drives every generation stage of that run ("one model for all
// tasks"). EnrichOnly restricts the run to the fast enrichment pass.
// WebVerify is an explicit opt-in for the freshness task: flagged claims are
// sent as queries to the hosted web search API (claim text leaves the
// machine) and a second model pass judges each flag against the evidence.
type RunOptions struct {
	Reanalyze    bool
	ReanalyzeAll bool
	External     bool
	EnrichOnly   bool
	Model        string
	EmbedModel   string
	AllowCloud   bool
	AllowRemote  bool
	WebVerify    bool
	Progress     func(Progress)
}

// RunProgress processes pending documents in model-sized batches:
//  1. apply local frontmatter policy (no_llm_context means no LLM call)
//  2. compute deterministic frontmatter fixes in Go — they become ordinary
//     pending suggestions the reviewer accepts or rejects like any other
//  3. if a distinct fast model is configured and the enrichment pass is
//     behind, derive missing title/tags from clear H1/path evidence locally,
//     then use the fast model only when local evidence is insufficient
//  4. if the analysis pass is behind, send the document to the analysis
//     model with similarity-ranked, annotated candidates as context
//  5. validate replies and persist them via SaveEnrichment/SaveAnalysis
//
// Optional enrichment model failures are warnings and do not block analysis.
// Source/read and full-analysis failures are recorded as status='error';
// neither kind of per-document failure aborts the run.
func RunProgress(ctx context.Context, c config.Config, db *database.DB, gens Generators, opts RunOptions) (Result, error) {
	models, err := resolveRunModels(c, opts)
	if err != nil {
		return Result{}, err
	}
	p := &pipeline{
		ctx: ctx, c: c, db: db, gens: gens, opts: opts,
		emit: func(pr Progress) {
			if opts.Progress != nil {
				opts.Progress(pr)
			}
		},
		fastModel:     models.fast,
		external:      opts.External,
		analysisModel: models.analysis,
		embedModel:    models.embed,
	}
	// Enrichment is a shared local stage: it runs (and is pending) whenever a
	// distinct fast model is configured — comparing against the resolved
	// analysis model would wrongly enable it on every external run.
	p.enrichOn = gens.Enrich != nil && (p.fastModel != c.OllamaModel || opts.EnrichOnly)
	var docs []database.Document
	switch {
	case opts.EnrichOnly:
		docs, err = db.PendingEnrich(ctx, p.fastModel, opts.Reanalyze, opts.ReanalyzeAll)
	case p.external:
		docs, err = db.PendingExternal(ctx, p.analysisModel, p.fastModel, p.enrichOn, opts.Reanalyze, opts.ReanalyzeAll)
	default:
		docs, err = db.Pending(ctx, p.analysisModel, p.fastModel, opts.Reanalyze, opts.ReanalyzeAll)
	}
	if err != nil {
		return Result{}, err
	}
	out := Result{}
	// Recorded renames mean link maintenance may be due even when nothing is
	// pending — a moved file does not mark its referrers changed.
	p.renames, err = db.Renames(ctx)
	if err != nil {
		return Result{}, err
	}
	if len(docs) == 0 && len(p.renames) == 0 && (gens.Embed == nil || models.embed == "" || opts.EnrichOnly) {
		return out, nil
	}
	if err := p.loadCorpus(); err != nil {
		return out, err
	}
	if err := p.relinkMoved(&out); err != nil {
		return out, err
	}
	if len(docs) == 0 {
		if p.embeddingUsed {
			return out, unloadModel(ctx, p.gens.Embed)
		}
		return out, nil
	}
	changedEvidence, analysisPending := p.pendingEvidencePaths(docs)
	p.forceAnalysis = make(map[string]bool)
	docs, err = p.prepareModelStages(docs, &out)
	if err != nil {
		return out, err
	}
	if !opts.EnrichOnly {
		dependents, err := p.evidenceDependents(changedEvidence, analysisPending)
		if err != nil {
			return out, err
		}
		docs = mergeDocumentLists(docs, dependents)
	}
	for i, doc := range docs {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		base := Progress{Index: i + 1, Total: len(docs), Path: doc.Path}
		if err := p.process(doc, &base, &out, opts.EnrichOnly); err != nil {
			return out, err
		}
	}
	return out, nil
}

type runModels struct {
	analysis string
	fast     string
	embed    string
}

func resolveRunModels(c config.Config, opts RunOptions) (runModels, error) {
	analysis, err := c.TaskModel(config.TaskAnalyze, opts.External, opts.Model)
	if err != nil {
		return runModels{}, err
	}
	if opts.External && analysis == "" {
		return runModels{}, errors.New("external run requires ollama_model_external in the config")
	}
	fast, err := c.TaskModel(config.TaskEnrich, false, opts.Model)
	if err != nil {
		return runModels{}, err
	}
	embed, err := c.TaskModel(config.TaskEmbed, false, opts.EmbedModel)
	if err != nil && !errors.Is(err, config.ErrNoTaskModel) {
		return runModels{}, err
	}
	return runModels{analysis: analysis, fast: fast, embed: embed}, nil
}

// laneSHA and laneModel read the active analysis lane's markers: the local
// analyzed_* pair normally, the external_* pair on an enhanced run.
func (p *pipeline) laneSHA(doc database.Document) string {
	if p.external {
		return doc.ExternalSHA256
	}
	return doc.AnalyzedSHA256
}

func (p *pipeline) laneModel(doc database.Document) string {
	if p.external {
		return doc.ExternalModel
	}
	return doc.AnalyzedModel
}

// saveAnalysis stamps the active lane: the local analyzed_* markers on a
// normal run, external_* on an enhanced run. The recorded model is always
// the resolved analysis model for this run.
func (p *pipeline) saveAnalysis(doc database.Document, title, summary string, tags []string, sugs []database.Suggestion, results ...database.StageResult) error {
	if p.external {
		return p.db.SaveExternalAnalysis(p.ctx, doc, p.analysisModel, title, summary, tags, sugs, results...)
	}
	return p.db.SaveAnalysis(p.ctx, doc, p.analysisModel, title, summary, tags, sugs, results...)
}

func (p *pipeline) pendingEvidencePaths(docs []database.Document) (map[string]bool, map[string]bool) {
	changed := make(map[string]bool)
	analysisPending := make(map[string]bool)
	for _, doc := range docs {
		if doc.SHA256 != p.laneSHA(doc) {
			changed[doc.Path] = true
		}
		if p.opts.ReanalyzeAll || doc.SHA256 != p.laneSHA(doc) ||
			(p.opts.Reanalyze && p.laneModel(doc) != p.analysisModel) {
			analysisPending[doc.Path] = true
		}
	}
	return changed, analysisPending
}

func (p *pipeline) prepareModelStages(docs []database.Document, out *Result) ([]database.Document, error) {
	if p.embeddingUsed {
		if err := unloadModel(p.ctx, p.gens.Embed); err != nil {
			return nil, err
		}
	}
	if p.enrichOn {
		var err error
		docs, err = p.enrichBatch(docs, out)
		if err != nil {
			return nil, err
		}
		if p.enrichmentUsed {
			if err := unloadModel(p.ctx, p.gens.Enrich); err != nil {
				return nil, err
			}
		}
	} else if p.c.EnrichmentMode != config.EnrichmentModeAlways {
		if err := p.localEnrichOnlyBatch(docs, out); err != nil {
			return nil, err
		}
	}
	if p.enrichOn || p.c.EnrichmentMode != config.EnrichmentModeAlways {
		if err := p.refreshDocuments(docs); err != nil {
			return nil, err
		}
	}
	return docs, nil
}

func (p *pipeline) taxonomyPrompt(doc database.Document, fm map[string]any) string {
	tags := p.taxonomy.PromptTags(doc.Path, frontmatter.StringList(fm, "tags"))
	if len(tags) == 0 {
		return ""
	}
	return "Approved frontmatter tag values from taxonomy.yaml (use only these for tags): " +
		strings.Join(tags, ", ") + "\n"
}

func (p *pipeline) refreshDocuments(docs []database.Document) error {
	allDocs, err := p.db.Documents(p.ctx)
	if err != nil {
		return err
	}
	p.allDocs = allDocs
	refreshed := make(map[string]database.Document, len(allDocs))
	for _, doc := range allDocs {
		refreshed[doc.Path] = doc
	}
	for i, doc := range docs {
		docs[i] = refreshed[doc.Path]
	}
	return nil
}

func mergeDocumentLists(primary, additional []database.Document) []database.Document {
	seen := make(map[string]bool, len(primary)+len(additional))
	out := append([]database.Document(nil), primary...)
	for _, doc := range primary {
		seen[doc.Path] = true
	}
	for _, doc := range additional {
		if !seen[doc.Path] {
			out = append(out, doc)
			seen[doc.Path] = true
		}
	}
	slices.SortFunc(out, func(a, b database.Document) int { return strings.Compare(a.Path, b.Path) })
	return out
}

func (p *pipeline) enrichBatch(docs []database.Document, out *Result) ([]database.Document, error) {
	ready := make([]database.Document, 0, len(docs))
	for i, doc := range docs {
		if err := p.ctx.Err(); err != nil {
			return nil, err
		}
		base := Progress{Index: i + 1, Total: len(docs), Path: doc.Path}
		if err := p.process(doc, &base, out, true); err != nil {
			return nil, err
		}
		// Only optional model failures fall through. Read/parse/privacy
		// failures retain their normal fail-closed handling.
		if base.Outcome != "error" && base.Outcome != "skipped" {
			ready = append(ready, doc)
		}
	}
	return ready, nil
}

// pipeline carries the per-run state shared by all documents: the corpus
// snapshot for context building, decoded similarity sketches, the visibility
// map and the resolved model names.
type pipeline struct {
	ctx               context.Context
	c                 config.Config
	db                *database.DB
	gens              Generators
	opts              RunOptions
	emit              func(Progress)
	fastModel         string
	external          bool   // true on an enhanced run: stamps the external lane
	analysisModel     string // resolved model for this run (local or external)
	embedModel        string
	enrichOn          bool
	mcpServer         string
	allDocs           []database.Document
	knownDocs         []database.Document
	visiblePaths      map[string]bool
	sigs              map[string][]uint64
	embeds            map[string][]float32
	entities          *entityIndex
	taxonomy          *taxonomy.Vocabulary
	renames           map[string]string
	forceAnalysis     map[string]bool
	embeddingUsed     bool
	embeddingDisabled bool
	enrichmentUsed    bool
}

// loadCorpus snapshots the corpus once per run: computes the LLM-visibility
// map, backfills missing similarity sketches (a local read, no LLM), decodes
// the sketches, and builds the link-resolution document set.
func (p *pipeline) loadCorpus() error {
	vocabulary, err := taxonomy.Load(p.c.RepositoryPath)
	if err != nil {
		return fmt.Errorf("load local tag taxonomy: %w", err)
	}
	p.taxonomy = vocabulary
	allDocs, err := p.db.Documents(p.ctx)
	if err != nil {
		return err
	}
	p.visiblePaths = make(map[string]bool, len(allDocs))
	p.entities = newEntityIndex()
	frameworkCatalog, err := p.indexVisibleCorpus(allDocs)
	if err != nil {
		return err
	}
	p.entities.resolveExplicitMarkdownLinks(p.visiblePaths)
	p.entities.addFrameworkCatalog(frameworkCatalog, p.visiblePaths)
	p.allDocs = allDocs
	p.buildCorpusIndexes(allDocs)
	p.mcpServer = p.c.MCPServer
	if p.mcpServer == "" {
		p.mcpServer = config.MCPServerKnowledgeBase
	}
	return nil
}

func (p *pipeline) indexVisibleCorpus(allDocs []database.Document) (string, error) {
	var frameworkCatalog string
	for i := range allDocs {
		if err := p.ctx.Err(); err != nil {
			return "", err
		}
		text := p.addVisibleDocument(&allDocs[i])
		if allDocs[i].Path == "frameworks/README.md" {
			frameworkCatalog = text
		}
	}
	return frameworkCatalog, nil
}

func (p *pipeline) addVisibleDocument(candidate *database.Document) string {
	if candidate.Status == "missing" {
		return ""
	}
	var fields map[string]any
	if candidate.FrontmatterJSON != "" &&
		json.Unmarshal([]byte(candidate.FrontmatterJSON), &fields) != nil {
		return ""
	}
	if frontmatter.NoLLMContext(fields) {
		return ""
	}
	text, err := scanner.ReadDocument(p.c, candidate.Path)
	if err != nil {
		return ""
	}
	current, _, err := frontmatter.Parse(text)
	if err != nil || frontmatter.NoLLMContext(current) {
		return ""
	}
	p.visiblePaths[candidate.Path] = true
	p.entities.addDocument(candidate.Path, current, text)
	p.entities.addExplicitMarkdownLinks(candidate.Path, text)
	if p.embedModel != "" && p.gens.Embed != nil && !p.embeddingDisabled &&
		(len(candidate.Embedding) == 0 || candidate.EmbeddedModel != p.embedModel || candidate.EmbeddedSHA256 != candidate.SHA256) {
		p.backfillEmbedding(candidate, text)
	}
	if len(candidate.Signature) == 0 {
		sig := similarity.Marshal(similarity.Signature(text))
		if err := p.db.SetSignature(p.ctx, candidate.Path, sig); err == nil {
			candidate.Signature = sig
		}
	}
	return text
}

func (p *pipeline) buildCorpusIndexes(allDocs []database.Document) {
	p.sigs = make(map[string][]uint64, len(allDocs))
	// knownDocs is the link-resolution universe for deterministic fixes and
	// the "unknown target" prompt lines: every non-missing document, including
	// no_llm_context ones — a link to a private document is still a valid link,
	// and flagging it would leak the private path into the prompt as "unknown".
	p.embeds = make(map[string][]float32, len(allDocs))
	for _, candidate := range allDocs {
		p.sigs[candidate.Path] = similarity.Unmarshal(candidate.Signature)
		if p.embedModel != "" && p.visiblePaths[candidate.Path] &&
			candidate.EmbeddedModel == p.embedModel && candidate.EmbeddedSHA256 == candidate.SHA256 {
			p.embeds[candidate.Path] = similarity.UnmarshalEmbedding(candidate.Embedding)
		}
		if candidate.Status != "missing" {
			p.knownDocs = append(p.knownDocs, candidate)
		}
	}
}

// backfillEmbedding computes and stores the semantic vector for one document.
// Only called for LLM-visible documents — private content never reaches the
// embed model. Failure disables embedding for the rest of the run (the
// embedder may be down or the model name wrong); ranking degrades to shingles.
func (p *pipeline) backfillEmbedding(doc *database.Document, text string) {
	text, err := embeddingText(*doc, text)
	if err != nil {
		p.emit(Progress{Path: doc.Path, Outcome: "embedding warning", Detail: err.Error()})
		return
	}
	vec, err := p.gens.Embed.Embed(p.ctx, text)
	if err != nil {
		p.emit(Progress{Path: doc.Path, Outcome: "embedding warning", Detail: err.Error() + "; falling back to compatible vectors/shingles"})
		p.embeddingDisabled = true
		return
	}
	p.embeddingUsed = true
	blob := similarity.MarshalEmbedding(vec)
	if err := p.db.SaveEmbedding(p.ctx, *doc, p.embedModel, blob); err == nil {
		doc.Embedding = blob
		doc.EmbeddedModel = p.embedModel
		doc.EmbeddedSHA256 = doc.SHA256
	} else {
		p.emit(Progress{Path: doc.Path, Outcome: "embedding warning", Detail: err.Error()})
	}
}

// evidenceDependents adds unchanged notes whose explicit or high-confidence
// local evidence changed with another document. It runs only when scan already
// found new/changed documents, so the unchanged path stays cheap.
func (p *pipeline) evidenceDependents(changedPaths, analysisPending map[string]bool) ([]database.Document, error) {
	changed := make(map[string]bool)
	for path := range changedPaths {
		if p.visiblePaths[path] {
			changed[path] = true
		}
	}
	if len(changed) == 0 {
		return nil, nil
	}
	inbound := make(map[string]bool)
	for target := range changed {
		sources, err := p.db.InboundLinks(p.ctx, target)
		if err != nil {
			return nil, err
		}
		for _, source := range sources {
			if p.visiblePaths[source] {
				inbound[source] = true
			}
		}
	}

	related := p.embeddingDependents(changed)
	var dependents []database.Document
	for _, doc := range p.allDocs {
		if p.ctx.Err() != nil {
			return nil, p.ctx.Err()
		}
		if analysisPending[doc.Path] || doc.Status == "missing" ||
			doc.SHA256 != p.laneSHA(doc) || !p.visiblePaths[doc.Path] {
			continue
		}
		content, err := scanner.ReadDocument(p.c, doc.Path)
		if err != nil {
			continue
		}
		fm, _, err := frontmatter.Parse(content)
		if err != nil || frontmatter.NoLLMContext(fm) {
			continue
		}
		if inbound[doc.Path] || p.dependsOnChangedEvidence(doc, fm, content, changed, related) {
			if p.currentlyVisible(doc.Path) {
				p.forceAnalysis[doc.Path] = true
				dependents = append(dependents, doc)
			}
		}
	}
	return dependents, nil
}

// embeddingEvidenceFloor returns the evidenceFloorQuantile cosine of the
// sampled document-pair distribution under the live embedding model — the
// per-run calibration of "unusually close." A zero floor (sparse or tiny
// corpus) degrades gracefully to a pure top-k neighborhood.
func (p *pipeline) embeddingEvidenceFloor() float64 {
	paths := slices.Sorted(maps.Keys(p.embeds))
	n := len(paths)
	if n < 4 {
		return 0
	}
	total := n * (n - 1) / 2
	stride := max(1, total/evidenceFloorSamplePairs)
	scores := make([]float64, 0, min(evidenceFloorSamplePairs, total))
	idx := 0
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if idx%stride == 0 {
				scores = append(scores, similarity.Cosine(p.embeds[paths[i]], p.embeds[paths[j]]))
			}
			idx++
		}
	}
	if len(scores) == 0 {
		return 0
	}
	slices.Sort(scores)
	return scores[int(evidenceFloorQuantile*float64(len(scores)-1))]
}

// embeddingDependents returns the documents unusually close to a changed
// document under the live embedding model: each changed document contributes
// at most its evidenceNeighborLimit nearest neighbors, and only those that
// also clear the corpus-calibrated floor. Rank plus calibration keeps the
// sweep selective across embedding models with very different cosine
// distributions — under the old absolute threshold a dense model flagged a
// quarter of all pairs and re-analyzed nearly the whole repository.
func (p *pipeline) embeddingDependents(changed map[string]bool) map[string]bool {
	related := map[string]bool{}
	floor := p.embeddingEvidenceFloor()
	type neighbor struct {
		path  string
		score float64
	}
	for path := range changed {
		emb := p.embeds[path]
		if len(emb) == 0 {
			continue
		}
		var hits []neighbor
		for other, otherEmb := range p.embeds {
			if other == path {
				continue
			}
			if c := similarity.Cosine(emb, otherEmb); c >= floor {
				hits = append(hits, neighbor{other, c})
			}
		}
		slices.SortFunc(hits, func(a, b neighbor) int {
			if c := cmp.Compare(b.score, a.score); c != 0 {
				return c
			}
			return cmp.Compare(a.path, b.path)
		})
		for i := 0; i < len(hits) && i < evidenceNeighborLimit; i++ {
			related[hits[i].path] = true
		}
	}
	return related
}

func (p *pipeline) dependsOnChangedEvidence(doc database.Document, fm map[string]any, content string, changed, related map[string]bool) bool {
	for _, key := range []string{"related", "contradicted_by"} {
		for _, target := range frontmatter.StringList(fm, key) {
			if changed[filepath.ToSlash(strings.TrimSpace(target))] {
				return true
			}
		}
	}
	for _, mention := range p.entities.matches(doc.Path, content) {
		if changed[mention.path] {
			return true
		}
		for _, source := range mention.sourcePaths {
			if changed[source] {
				return true
			}
		}
	}
	if related[doc.Path] {
		return true
	}
	docSignature := p.sigs[doc.Path]
	if len(docSignature) == 0 {
		docSignature = similarity.Signature(content)
	}
	for path := range changed {
		if similarity.Estimate(docSignature, p.sigs[path]) >= overlapThreshold {
			return true
		}
	}
	return false
}

// fail records a per-document error: persisted to the documents table,
// emitted as an "error" event and counted — never aborts the run.
func (p *pipeline) fail(base *Progress, out *Result, detail string) error {
	if saveErr := p.db.SaveError(p.ctx, base.Path, detail); saveErr != nil {
		return fmt.Errorf("record failure for %s: %w", base.Path, saveErr)
	}
	base.Outcome, base.Detail = "error", detail
	p.emit(*base)
	out.Failed++
	return nil
}

// process runs one document through the selected model stage: policy check,
// deterministic fixes, then enrichment or analysis as its marker requires. Errors
// are systemic (abort the run); per-document problems go through fail.
func (p *pipeline) process(doc database.Document, base *Progress, out *Result, enrichOnly bool) error {
	var fm map[string]any
	if doc.FrontmatterJSON != "" {
		if err := json.Unmarshal([]byte(doc.FrontmatterJSON), &fm); err != nil {
			return p.fail(base, out, "invalid stored frontmatter JSON: "+err.Error())
		}
	}
	candidates := visibleCandidates(doc.Path, p.allDocs, p.visiblePaths)
	content, readErr := scanner.ReadDocument(p.c, doc.Path)

	if frontmatter.NoLLMContext(fm) {
		return p.skipPrivate(doc, fm, content, readErr, base, out)
	}
	if readErr != nil {
		return p.fail(base, out, readErr.Error())
	}
	// Re-check the live file before constructing any prompt; the scanner's
	// stored policy may predate a human edit made during this long run.
	current, body, err := frontmatter.Parse(content)
	if err != nil {
		return p.fail(base, out, err.Error())
	}
	fm = current
	if frontmatter.NoLLMContext(fm) {
		return p.skipPrivate(doc, fm, content, nil, base, out)
	}
	if frontmatter.IsStub(fm, body, p.c.StubLineLimit()) {
		return p.skipStub(doc, fm, content, base, out)
	}
	detSug, hasDet := deterministicFix(doc.Path, content, fm, p.knownDocs, p.retarget)
	needEnrich := p.enrichOn && (p.opts.ReanalyzeAll || doc.SHA256 != doc.EnrichedSHA256 ||
		(p.opts.Reanalyze && doc.EnrichedModel != p.fastModel))
	needAnalyze := p.opts.ReanalyzeAll || p.forceAnalysis[doc.Path] || doc.SHA256 != p.laneSHA(doc) ||
		(p.opts.Reanalyze && p.laneModel(doc) != p.analysisModel)
	if enrichOnly {
		if needEnrich {
			return p.enrich(doc, fm, content, hasDet, detSug, base, out)
		}
		return nil
	}
	if needAnalyze {
		return p.analyzeDoc(doc, fm, candidates, content, detSug, hasDet, base, out)
	}
	return nil
}

// skipPrivate handles a kb_policy.no_llm_context document: no LLM call, but
// deterministic frontmatter fixes still apply (computed locally, so nothing
// private reaches the model).
func (p *pipeline) skipPrivate(doc database.Document, fm map[string]any, content string, readErr error, base *Progress, out *Result) error {
	if err := p.skipLLM(doc, fm, content, readErr,
		"LLM analysis skipped by kb_policy.no_llm_context or an invalid kb_policy.",
		"kb_policy.no_llm_context", base); err != nil {
		return err
	}
	out.Skipped++
	return nil
}

// skipStub handles a placeholder document (stub tag or minimal body): it
// stays scanned, embedded and visible as a link target and candidate, but no
// generation stage runs — a stub's only job is to exist for other notes to
// link to.
func (p *pipeline) skipStub(doc database.Document, fm map[string]any, content string, base *Progress, out *Result) error {
	if err := p.skipLLM(doc, fm, content, nil,
		"Generation skipped: this note is a link-only placeholder (stub tag or minimal body).",
		"stub placeholder", base); err != nil {
		return err
	}
	out.Stubs++
	return nil
}

// skipLLM records a generation-free outcome for a document that must not be
// analyzed: deterministic frontmatter fixes still apply (computed locally),
// and stage markers are stamped so the document does not stay pending forever.
func (p *pipeline) skipLLM(doc database.Document, fm map[string]any, content string, readErr error, summary, detail string, base *Progress) error {
	var sugs []database.Suggestion
	if readErr == nil {
		if s, ok := deterministicFix(doc.Path, content, fm, p.knownDocs, p.retarget); ok {
			sugs = []database.Suggestion{s}
		}
	}
	title, _ := fm["title"].(string)
	tags := normalizeTags(nil, doc.TagsJSON, frontmatter.StringList(fm, "tags"))
	// Enrichment save runs first: each save supersedes the suggestion origins
	// it regenerates, and SaveAnalysis must run last so the deterministic fix
	// it carries is not superseded back out.
	if p.enrichOn {
		if err := p.db.SaveEnrichment(p.ctx, doc, p.fastModel, strings.TrimSpace(title), summary, tags, nil, database.StageResult{Outcome: "skipped"}); err != nil {
			return fmt.Errorf("record %s enrichment skip for %s: %w", detail, doc.Path, err)
		}
	}
	// An enrich-only run never stamps the analysis lane — deterministic fixes
	// stay pending so the next full run still reviews them.
	if !p.opts.EnrichOnly {
		if err := p.saveAnalysis(doc, strings.TrimSpace(title), summary, tags, sugs, database.StageResult{Outcome: "skipped"}); err != nil {
			return fmt.Errorf("record %s skip for %s: %w", detail, doc.Path, err)
		}
	}
	base.Outcome, base.Detail = "skipped", detail
	p.emit(*base)
	return nil
}

// analyzeDoc runs the full analysis pass: similarity-ranked candidates,
// deterministic context, the analysis model, then local validation of the
// model's suggestions before SaveAnalysis persists them.
func (p *pipeline) analyzeDoc(doc database.Document, fm map[string]any, candidates []database.Document, content string, detSug database.Suggestion, hasDet bool, base *Progress, out *Result) error {
	p.emit(Progress{Index: base.Index, Total: base.Total, Path: base.Path, Outcome: "analyzing"})
	var localPatch metadataPatch
	var localDerived []string
	localMetadataPending := doc.EnrichedModel == "deterministic" && doc.EnrichedSHA256 == doc.SHA256
	if localMetadataPending {
		localPatch, localDerived, localMetadataPending = deterministicMetadataPatch(doc, fm, content, p.knownDocs, p.taxonomy)
	}
	docSig := p.sigs[doc.Path]
	if len(docSig) == 0 {
		docSig = similarity.Signature(content)
	}
	docEmb := p.embeds[doc.Path]
	mentions := p.entities.matches(doc.Path, content)
	mentionsByPath := make(map[string][]entityMention)
	for _, mention := range mentions {
		mentionsByPath[mention.path] = append(mentionsByPath[mention.path], mention)
	}
	ranked := rankCandidates(doc.Path, fm, candidates, docSig, p.sigs, docEmb, p.embeds, mentionsByPath, candidateLimit)
	ranked = slices.DeleteFunc(ranked, func(c candidate) bool { return !p.currentlyVisible(c.doc.Path) })
	mergeText, mergeBodies := p.mergeContext(ranked)
	inbound, _ := p.db.InboundLinks(p.ctx, doc.Path)
	inbound = filterLLMVisiblePaths(inbound, p.visiblePaths)
	inbound = slices.DeleteFunc(inbound, func(path string) bool { return !p.currentlyVisible(path) })
	siblings, _ := scanner.SiblingFiles(p.c, doc.Path)
	contextText := fmt.Sprintf("Current path: %s\nFile modified: %s\nPath tags: %s\nDocument frontmatter: %s\n%s%s%s%s%s%s%s%sExisting documents (possible relations/merge candidates; annotated with title, tags and content similarity):\n%s",
		doc.Path,
		time.Unix(doc.ModifiedUnix, 0).UTC().Format("2006-01-02"),
		doc.TagsJSON, cmp.Or(doc.FrontmatterJSON, "none"),
		p.taxonomyPrompt(doc, fm),
		frontmatterContext(doc, fm, p.knownDocs),
		localMetadataContext(localPatch),
		entityEvidenceContext(p.c, ranked),
		p.rejectionContext(doc.Path),
		inboundContext(inbound),
		siblingContext(siblings, content),
		overlapContext(ranked),
		renderCandidates(ranked))
	contextText += mergeText
	prompt, err := prompts.Compose(p.c.PromptDirectory, p.mcpServer, contextText, content)
	if err != nil {
		// Missing prompt files are a systemic problem — abort the run.
		return err
	}
	started := time.Now()
	raw, err := p.gens.Analysis.Generate(p.ctx, prompt, analysisSchema)
	if err != nil {
		return p.analysisFailure(doc, base, out, err.Error(), started)
	}
	var parsed llmResult
	if uerr := json.Unmarshal([]byte(stripFence(raw)), &parsed); uerr != nil {
		// Malformed JSON is usually a systematic slip — an unescaped
		// backslash or a stray brace — that an identical temperature-0
		// re-prompt would repeat. One annotated retry names the parser
		// error so the model can avoid the exact mistake.
		raw, err = p.gens.Analysis.Generate(p.ctx, prompt+jsonRetryHint(uerr), analysisSchema)
		if err != nil {
			return p.analysisFailure(doc, base, out, err.Error(), started)
		}
		if uerr = json.Unmarshal([]byte(stripFence(raw)), &parsed); uerr != nil {
			return p.analysisFailure(doc, base, out, "invalid JSON: "+uerr.Error()+"; response: "+truncate(raw, 1000), started)
		}
	}
	tags := normalizeTags(parsed.Tags, doc.TagsJSON, frontmatter.StringList(fm, "tags"))
	validated := p.validateModelSuggestions(doc, content, mergeBodies, parsed.Suggestions, localPatch, localMetadataPending)
	sugs := validated.items
	patch, patchRejected, localFieldsApplied := p.analysisMetadataSuggestion(
		doc, fm, content, parsed.Patch, localPatch, localDerived, localMetadataPending, validated,
	)
	if patch != nil {
		sugs = append(sugs, *patch)
	}
	rejected := slices.Clone(validated.rejected)
	rejected = append(rejected, patchRejected...)
	if hasDet {
		sugs = append(sugs, detSug)
	}
	audit := stageResult(parsed.Patch, patch, patchRejected)
	audit.Metadata = stageMetadata(parsed.Title, parsed.Summary, parsed.Tags)
	audit.Rejected = rejected
	audit.DerivedFields = slices.Compact(localFieldsApplied)
	if patch == nil && len(localFieldsApplied) > 0 && len(sugs) > 0 {
		audit.PatchOutcome = "suggested"
	}
	audit.DurationMS = time.Since(started).Milliseconds()
	if err = p.saveAnalysis(doc, strings.TrimSpace(parsed.Title), strings.TrimSpace(parsed.Summary), tags, sugs, audit); err != nil {
		return err
	}
	base.Outcome = "analyzed"
	base.Detail = suggestionDetail(len(sugs), rejected)
	p.emit(*base)
	out.Processed++
	out.Rejected += len(rejected)
	return nil
}

// retarget resolves a moved document path through the renames map — the
// deterministic link repair's lookup into RetargetPath.
func (p *pipeline) retarget(path string) (string, bool) {
	return p.db.RetargetPath(p.ctx, path)
}

// relinkMoved is deterministic maintenance that runs before the generation
// passes: documents whose frontmatter or body links resolve to a path the
// scanner saw renamed away get the retarget repair suggestion — no model, no
// lineage stamps (SaveLinkFix leaves *_sha256 untouched), just the fix. A
// renamed file does not mark its referrers changed, so this pass finds them
// itself; nothing here runs without recorded renames.
func (p *pipeline) relinkMoved(out *Result) error {
	if len(p.renames) == 0 {
		return nil
	}
	for _, doc := range p.knownDocs {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		if !referencesMoved(doc, p.renames) {
			continue
		}
		content, err := scanner.ReadDocument(p.c, doc.Path)
		if err != nil {
			continue
		}
		fm, _, err := frontmatter.Parse(content)
		if err != nil {
			continue
		}
		sug, ok := deterministicFix(doc.Path, content, fm, p.knownDocs, p.retarget)
		if !ok {
			continue
		}
		if err := p.db.SaveLinkFix(p.ctx, doc, []database.Suggestion{sug}); err != nil {
			return err
		}
		p.emit(Progress{Path: doc.Path, Outcome: "link fix", Detail: sug.Reason})
		out.Relinked++
	}
	return nil
}

// referencesMoved is the cheap prefilter for relinkMoved: does this document
// mention any recorded old path — in frontmatter related/contradicted_by or
// in a stored link_issues entry? Resolution itself goes through RetargetPath
// (chain-following, live-endpoint check) inside deterministicFix.
func referencesMoved(doc database.Document, renames map[string]string) bool {
	var fm map[string]any
	if doc.FrontmatterJSON != "" {
		_ = json.Unmarshal([]byte(doc.FrontmatterJSON), &fm)
	}
	for _, key := range []string{"related", "contradicted_by"} {
		for _, item := range frontmatter.StringList(fm, key) {
			if _, ok := renames[filepath.ToSlash(strings.TrimSpace(item))]; ok {
				return true
			}
		}
	}
	var issues []string
	if doc.LinkIssues != "" {
		_ = json.Unmarshal([]byte(doc.LinkIssues), &issues)
	}
	for _, i := range issues {
		target := strings.TrimPrefix(i, "unresolved link target: ")
		for _, candidate := range links.InternalCandidates(doc.Path, target) {
			if _, ok := renames[candidate]; ok {
				return true
			}
		}
	}
	return false
}

// deterministicFix produces a frontmatter/body-repair suggestion without any
// LLM: `related`/`contradicted_by` entries and markdown body links that point
// at a document the scanner saw move are retargeted to its new path; entries
// that are not valid repository-relative paths or resolve to nothing are
// dropped. The fix is an ordinary pending suggestion — only a human accept
// turns it into a write. knownDocs lists every non-missing document
// (including policy-excluded ones — linking to a private document is
// legitimate). retarget resolves a moved path to its new live location.
func deterministicFix(docPath, content string, fm map[string]any, knownDocs []database.Document, retarget func(string) (string, bool)) (database.Suggestion, bool) {
	if len(fm) == 0 {
		return database.Suggestion{}, false
	}
	known := map[string]bool{docPath: true}
	for _, c := range knownDocs {
		known[c.Path] = true
	}
	fixed := maps.Clone(fm)
	var removed, retargeted []string
	for _, key := range []string{"related", "contradicted_by"} {
		items := frontmatter.StringList(fm, key)
		if len(items) == 0 {
			continue
		}
		var keep []string
		for _, item := range items {
			p := filepath.ToSlash(strings.TrimSpace(item))
			if p != "" && !filepath.IsAbs(p) && !strings.HasPrefix(p, "..") && !known[p] {
				if next, ok := retarget(p); ok {
					retargeted = append(retargeted, key+" -> "+item+" → "+next)
					keep = append(keep, next)
					continue
				}
			}
			if p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "..") || !known[p] {
				removed = append(removed, key+" -> "+item)
				continue
			}
			keep = append(keep, p)
		}
		if len(keep) == 0 {
			delete(fixed, key)
		} else {
			fixed[key] = keep
		}
	}
	// Body links to a moved file are retargeted the same way — before the
	// frontmatter rewrite, so one suggestion carries both repairs.
	relinked := links.Retarget(docPath, content, retarget)
	if relinked != content {
		retargeted = append(retargeted, "body link(s)")
	}
	if len(removed) == 0 && len(retargeted) == 0 {
		return database.Suggestion{}, false
	}
	proposed, err := frontmatter.Rewrite(relinked, fixed)
	if err != nil {
		return database.Suggestion{}, false
	}
	var parts []string
	if len(retargeted) > 0 {
		parts = append(parts, "Retargeted moved link target(s): "+strings.Join(retargeted, ", "))
	}
	if len(removed) > 0 {
		parts = append(parts, "Removed unresolved link target(s): "+strings.Join(removed, ", "))
	}
	return database.Suggestion{
		DocumentPath:    docPath,
		Action:          "expand",
		Reason:          strings.Join(parts, "; "),
		Confidence:      1.0,
		ProposedContent: proposed,
		Origin:          "deterministic",
	}, true
}

// candidate pairs a corpus document with its computed ranking signals:
// sim is the shingle Jaccard estimate (literal text overlap → merge signal)
// and rel is the embedding cosine (topical relatedness → link signal).
type candidate struct {
	doc      database.Document
	sim      float64
	rel      float64
	mentions []entityMention
	sameDir  bool
	linked   bool
}

// visibleCandidates returns the non-missing, LLM-visible corpus documents
// other than the one under analysis.
func visibleCandidates(docPath string, all []database.Document, visible map[string]bool) []database.Document {
	out := make([]database.Document, 0, len(all))
	for _, d := range all {
		if d.Path != docPath && d.Status != "missing" && visible[d.Path] {
			out = append(out, d)
		}
	}
	return out
}

// rankCandidates orders merge/relation candidates for the prompt: estimated
// content similarity first, then same-directory, then documents this doc's
// frontmatter already links to, then path order. Returns at most limit
// entries.
func rankCandidates(docPath string, fm map[string]any, candidates []database.Document, docSig []uint64, sigs map[string][]uint64, docEmb []float32, embeds map[string][]float32, mentions map[string][]entityMention, limit int) []candidate {
	docDir := filepath.ToSlash(filepath.Dir(docPath))
	linked := map[string]bool{}
	for _, key := range []string{"related", "contradicted_by"} {
		for _, l := range frontmatter.StringList(fm, key) {
			linked[filepath.ToSlash(strings.TrimSpace(l))] = true
		}
	}
	scored := make([]candidate, 0, len(candidates))
	for _, d := range candidates {
		scored = append(scored, candidate{
			doc:      d,
			sim:      similarity.Estimate(docSig, sigs[d.Path]),
			rel:      similarity.Cosine(docEmb, embeds[d.Path]),
			mentions: mentions[d.Path],
			sameDir:  filepath.ToSlash(filepath.Dir(d.Path)) == docDir,
			linked:   linked[d.Path],
		})
	}
	slices.SortStableFunc(scored, func(a, b candidate) int {
		if c := cmp.Compare(len(b.mentions), len(a.mentions)); c != 0 {
			return c
		}
		// Semantic relatedness is the better link signal when embeddings
		// exist; shingle overlap remains the sort key when they don't.
		if c := cmp.Compare(b.rel, a.rel); c != 0 {
			return c
		}
		if c := cmp.Compare(b.sim, a.sim); c != 0 {
			return c
		}
		if a.sameDir != b.sameDir {
			if a.sameDir {
				return -1
			}
			return 1
		}
		if a.linked != b.linked {
			if a.linked {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.doc.Path, b.doc.Path)
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored
}

// renderCandidates formats the ranked candidate list for the prompt: path
// plus title, tags and similarity percentage when known.
func renderCandidates(ranked []candidate) string {
	var lines []string
	for _, c := range ranked {
		line := "- " + c.doc.Path
		var ann []string
		if t := strings.TrimSpace(c.doc.Title); t != "" {
			ann = append(ann, `"`+t+`"`)
		}
		var tags []string
		if err := json.Unmarshal([]byte(c.doc.TagsJSON), &tags); err == nil && len(tags) > 0 {
			ann = append(ann, "["+strings.Join(tags, ", ")+"]")
		}
		if c.rel > 0 {
			ann = append(ann, fmt.Sprintf("%.0f%% related", c.rel*100))
		} else if c.sim > 0 {
			ann = append(ann, fmt.Sprintf("%.0f%% similar", c.sim*100))
		}
		if len(c.mentions) > 0 {
			terms := make([]string, 0, min(len(c.mentions), 3))
			for _, mention := range c.mentions {
				if len(terms) == 3 {
					break
				}
				terms = append(terms, fmt.Sprintf("%q at body line %d", mention.term, mention.line))
			}
			ann = append(ann, "exact mention: "+strings.Join(terms, ", "))
		}
		if len(ann) > 0 {
			line += " — " + strings.Join(ann, " ")
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// overlapContext lists probable duplicates above the similarity threshold —
// merge candidates the model can verify instead of guessing from filenames.
func overlapContext(ranked []candidate) string {
	var lines []string
	for _, c := range ranked {
		if c.sim < overlapThreshold {
			continue // rel may sort a low-overlap doc above a high-sim one
		}
		lines = append(lines, fmt.Sprintf("- %s (~%.0f%% similar)", c.doc.Path, c.sim*100))
	}
	if len(lines) == 0 {
		return ""
	}
	return "High content overlap (possible duplicates):\n" + strings.Join(lines, "\n") + "\n"
}

func filterLLMVisiblePaths(paths []string, visible map[string]bool) []string {
	filtered := make([]string, 0, len(paths))
	for _, path := range paths {
		if visible[path] {
			filtered = append(filtered, path)
		}
	}
	return filtered
}

// validAction whitelists the actions apply knows how to execute.
func validAction(a string) bool {
	switch a {
	case "none", "move", "merge", "expand", "delete":
		return true
	}
	return false
}

// clamp forces confidence into [0,1] — models occasionally emit out-of-range values.
func clamp(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

// jsonRetryHint annotates the single analysis re-prompt with the JSON parser
// error — the model sees why its previous reply failed instead of blindly
// regenerating the same malformed output.
func jsonRetryHint(err error) string {
	return "\n\nYour previous reply failed JSON parsing (" + err.Error() +
		"). Return the complete analysis again as one valid JSON object only; escape backslashes and quotes inside string values."
}

// stripFence removes ``` / ```json code fences a model may wrap around its JSON
// output despite being told to answer with raw JSON only.
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// truncate caps strings (e.g. raw LLM output) before storing them in SQLite error fields.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// frontmatterContext renders the deterministic frontmatter findings for the
// prompt: local validation issues plus 'related'/'contradicted_by' entries
// that do not resolve to a known document path. knownDocs covers every
// non-missing document so links to policy-excluded files are not misreported.
func frontmatterContext(doc database.Document, fm map[string]any, knownDocs []database.Document) string {
	var lines []string
	var issues []string
	if doc.FrontmatterIssues != "" {
		_ = json.Unmarshal([]byte(doc.FrontmatterIssues), &issues)
	}
	for _, i := range issues {
		lines = append(lines, "Frontmatter issue: "+i)
	}
	var linkIssues []string
	if doc.LinkIssues != "" {
		_ = json.Unmarshal([]byte(doc.LinkIssues), &linkIssues)
	}
	for _, i := range linkIssues {
		lines = append(lines, "Link check: "+i)
	}
	if len(fm) > 0 {
		known := map[string]bool{doc.Path: true}
		for _, c := range knownDocs {
			known[c.Path] = true
		}
		for _, key := range []string{"related", "contradicted_by"} {
			for _, link := range frontmatter.StringList(fm, key) {
				if !known[filepath.ToSlash(strings.TrimSpace(link))] {
					lines = append(lines, fmt.Sprintf("Frontmatter %s target %q does not match any known document", key, link))
				}
			}
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// rejectionContext summarizes the suggestions a human reviewer refused for
// this note — the strongest feedback signal the pipeline has. The reviewer
// note carries ground truth the model may lack (post-cutoff facts, house
// rules), so it is quoted verbatim, bounded to the most recent entries.
func (p *pipeline) rejectionContext(path string) string {
	rejected, err := p.db.RejectedSuggestions(p.ctx, path, 5)
	if err != nil || len(rejected) == 0 {
		return ""
	}
	lines := make([]string, 0, len(rejected))
	for _, s := range rejected {
		line := "- " + s.Action
		if s.TargetPath != "" {
			line += " → " + s.TargetPath
		}
		if s.Reason != "" {
			line += ": " + truncate(s.Reason, 160)
		}
		if s.DecisionNote != "" {
			line += " (reviewer: " + truncate(s.DecisionNote, 200) + ")"
		}
		lines = append(lines, line)
	}
	return "Human reviewers previously REJECTED these suggestions for this note — do not re-propose them; a reviewer note is ground truth you may lack:\n" + strings.Join(lines, "\n") + "\n"
}

// inboundContext lists documents whose frontmatter already links to this one —
// input for deciding whether a backlink belongs in `related`.
func inboundContext(inbound []string) string {
	if len(inbound) == 0 {
		return ""
	}
	return "Inbound links (documents whose frontmatter links here):\n- " + strings.Join(inbound, "\n- ") + "\n"
}

// siblingContext lists same-directory files the document never mentions —
// candidates for a missing reference (helper scripts, configs, examples).
func siblingContext(siblings []string, content string) string {
	var missing []string
	for _, name := range siblings {
		if !strings.Contains(content, name) {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return "Sibling files the document does not mention:\n- " + strings.Join(missing, "\n- ") + "\n"
}

// normalizeTags merges path-derived, frontmatter-declared and LLM-suggested
// tags: lowercased, spaces -> '-', deduplicated, sorted.
func normalizeTags(llm []string, pathJSON string, fmTags []string) []string {
	var pathTags []string
	_ = json.Unmarshal([]byte(pathJSON), &pathTags)
	m := map[string]bool{}
	var out []string
	for _, list := range [][]string{pathTags, fmTags, llm} {
		for _, t := range list {
			t = strings.ToLower(strings.TrimSpace(t))
			t = strings.ReplaceAll(t, " ", "-")
			if t != "" && !m[t] {
				m[t] = true
				out = append(out, t)
			}
		}
	}
	slices.Sort(out)
	return out
}
