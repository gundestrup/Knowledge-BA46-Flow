package analyzer

import (
	"context"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
)

// Estimate counts what an analysis run would queue: the lane-pending
// documents plus the unchanged notes the evidence sweep would pull in.
// The HTTP dry-run uses it to show the consequence of a selection before
// committing to a run.
type Estimate struct {
	Pending          int            `json:"pending"`
	Dependents       int            `json:"dependents"`
	DependentReasons map[string]int `json:"dependent_reasons,omitempty"`
	Total            int            `json:"total"`
	SemanticChanges  int            `json:"semantic_changes"`
	PromptChanges    int            `json:"prompt_changes"`
	ModelDrift       int            `json:"model_drift"`
	Unstamped        int            `json:"unstamped"`
}

// EstimateAnalysis computes the RunProgress queue — pending resolution plus
// the evidence-dependents sweep — and returns counts only. It performs the
// same corpus reads (and signature backfill) a real run performs but never
// calls a model; enrichment and analysis stop before their first stage.
func EstimateAnalysis(ctx context.Context, c config.Config, db *database.DB, opts RunOptions) (Estimate, error) {
	models, err := resolveRunModels(c, opts)
	if err != nil {
		return Estimate{}, err
	}
	// Mirror RunProgress: the server always builds an enrichment client when a
	// fast model resolves, so the pending predicate must see the same enrichOn.
	p := newPipeline(ctx, c, db, Generators{}, opts, models, models.fast != "")
	allDocs, err := db.Documents(ctx)
	if err != nil {
		return Estimate{}, err
	}
	direct, est := pendingForEstimate(allDocs, p, opts)
	est.Total = len(direct)
	needsRemovalSweep, err := db.NeedsRemovalSweep(ctx, opts.External)
	if err != nil {
		return Estimate{}, err
	}
	if opts.EnrichOnly || (len(direct) == 0 && !needsRemovalSweep && !hasV2Evidence(allDocs, p)) {
		return est, nil
	}
	p.removalGeneration, err = db.RemovalGeneration(ctx)
	if err != nil {
		return Estimate{}, err
	}
	if err := p.loadCorpus(); err != nil {
		return Estimate{}, err
	}
	p.forceAnalysis = make(map[string]bool)
	dependents, err := p.evidenceDependents(direct, false)
	if err != nil {
		return Estimate{}, err
	}
	est.Dependents = len(dependents)
	est.DependentReasons = p.depReasons
	est.Total = len(mergeDocumentLists(direct, dependents))
	return est, nil
}

func hasV2Evidence(docs []database.Document, p *pipeline) bool {
	for _, doc := range docs {
		if p.laneSHA(doc) == "" {
			continue
		}
		ev, ok := parseAnalysisEvidenceRecord(p.laneEvidenceJSON(doc))
		if ok && ev.Version == 2 {
			return true
		}
	}
	return false
}

func pendingForEstimate(allDocs []database.Document, p *pipeline, opts RunOptions) ([]database.Document, Estimate) {
	direct := make([]database.Document, 0, len(allDocs))
	est := Estimate{}
	for _, doc := range allDocs {
		if doc.Status == "missing" {
			continue
		}
		laneInput, laneSemantic, laneModel := p.laneSHA(doc), p.laneSemanticSHA(doc), p.laneModel(doc)
		semanticChanged := doc.SemanticSHA256 != laneSemantic
		promptChanged := !semanticChanged && doc.AnalysisInputSHA256 != laneInput
		modelDrift := opts.Reanalyze && laneModel != p.analysisModel
		if p.isLanePending(doc) {
			direct = append(direct, doc)
			est.Pending++
			if opts.ReanalyzeAll {
				continue
			}
			if laneInput == "" {
				est.Unstamped++
			}
			if semanticChanged {
				est.SemanticChanges++
			}
			if promptChanged {
				est.PromptChanges++
			}
			if modelDrift {
				est.ModelDrift++
			}
		}
	}
	return direct, est
}
