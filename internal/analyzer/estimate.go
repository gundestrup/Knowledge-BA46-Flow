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
	Pending    int `json:"pending"`
	Dependents int `json:"dependents"`
	Total      int `json:"total"`
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
		return Estimate{}, err
	}
	est := Estimate{Pending: len(docs), Total: len(docs)}
	if len(docs) == 0 || opts.EnrichOnly {
		return est, nil
	}
	if err := p.loadCorpus(); err != nil {
		return Estimate{}, err
	}
	changedEvidence, analysisPending := p.pendingEvidencePaths(docs)
	p.forceAnalysis = make(map[string]bool)
	dependents, err := p.evidenceDependents(changedEvidence, analysisPending)
	if err != nil {
		return Estimate{}, err
	}
	est.Dependents = len(dependents)
	est.Total = len(mergeDocumentLists(docs, dependents))
	return est, nil
}
