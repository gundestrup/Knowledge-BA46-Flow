package analyzer

import (
	"encoding/json"
	"time"

	"knowledge-ba46-flow/internal/database"
)

func patchValue(patch *metadataPatch) metadataPatch {
	if patch == nil {
		return metadataPatch{}
	}
	return *patch
}

func stageResult(patch *metadataPatch, suggestion *database.Suggestion, rejected []string) database.StageResult {
	encoded, _ := json.Marshal(patch)
	result := database.StageResult{Patch: encoded, Rejected: rejected}
	switch {
	case suggestion != nil:
		result.PatchOutcome = "suggested"
	case len(rejected) > 0:
		result.PatchOutcome = "rejected"
	case patch == nil:
		result.PatchOutcome = "missing"
	case len(patch.Set) == 0 && len(patch.Remove) == 0:
		result.PatchOutcome = "empty"
	default:
		result.PatchOutcome = "unchanged"
	}
	return result
}

func stageMetadata(title, summary string, tags []string) json.RawMessage {
	encoded, _ := json.Marshal(struct {
		Title   string   `json:"title"`
		Summary string   `json:"summary"`
		Tags    []string `json:"tags"`
	}{title, summary, tags})
	return encoded
}

func (p *pipeline) analysisFailure(doc database.Document, base *Progress, out *Result, detail string, started time.Time) error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	if err := p.db.SaveStageError(p.ctx, doc, "analysis", p.c.OllamaModel, detail, time.Since(started)); err != nil {
		return err
	}
	return p.fail(base, out, detail)
}
