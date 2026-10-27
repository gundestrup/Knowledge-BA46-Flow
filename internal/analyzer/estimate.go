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
	Pending         int `json:"pending"`
	Dependents      int `json:"dependents"`
	Total           int `json:"total"`
	SemanticChanges int `json:"semantic_changes"`
	PromptChanges   int `json:"prompt_changes"`
	ModelDrift      int `json:"model_drift"`
	Unstamped       int `json:"unstamped"`
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
	p := &pipeline{
		ctx: ctx, c: c, db: db, opts: opts,
		emit:          func(Progress) {},
		fastModel:     models.fast,
		external:      opts.External,
		analysisModel: models.analysis,
		embedModel:    models.embed,
	}
	// Mirror RunProgress: the server always builds an enrichment client when a
	// fast model resolves, so the pending predicate must see the same enrichOn.
	p.enrichOn = models.fast != "" && (models.fast != c.OllamaModel || opts.EnrichOnly)
	allDocs, err := db.Documents(ctx)
	if err != nil {
		return Estimate{}, err
	}
	direct := make([]database.Document, 0, len(allDocs))
	est := Estimate{}
	for _, doc := range allDocs {
		if doc.Status == "missing" {
			continue
		}
		laneInput, laneSemantic, laneModel := doc.AnalyzedSHA256, doc.AnalyzedSemanticSHA256, doc.AnalyzedModel
		if p.external {
			laneInput, laneSemantic, laneModel = doc.ExternalSHA256, doc.ExternalSemanticSHA256, doc.ExternalModel
		}
		semanticChanged := doc.SemanticSHA256 != laneSemantic
		promptChanged := !semanticChanged && doc.AnalysisInputSHA256 != laneInput
		modelDrift := opts.Reanalyze && laneModel != p.analysisModel
		if opts.ReanalyzeAll || doc.AnalysisInputSHA256 != laneInput || modelDrift {
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
	est.Total = len(direct)
	if len(direct) == 0 || opts.EnrichOnly {
		return est, nil
	}
	if err := p.loadCorpus(); err != nil {
		return Estimate{}, err
	}
	changedEvidence, analysisPending := p.pendingEvidencePaths(direct)
	p.forceAnalysis = make(map[string]bool)
	dependents, err := p.evidenceDependents(changedEvidence, analysisPending)
	if err != nil {
		return Estimate{}, err
	}
	est.Dependents = len(dependents)
	est.Total = len(mergeDocumentLists(direct, dependents))
	return est, nil
}
