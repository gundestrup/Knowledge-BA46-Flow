package analyzer

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/prompts"
	"knowledge-ba46-flow/internal/scanner"
	"knowledge-ba46-flow/internal/taxonomy"
)

// enrich keeps optional generation failures separate from source/read/policy
// failures. Only the former may fall through to the full-analysis batch.
func (p *pipeline) enrich(doc database.Document, fm map[string]any, content string, carryDet bool, detSug database.Suggestion, base *Progress, out *Result) error {
	p.emit(Progress{Index: base.Index, Total: base.Total, Path: base.Path, Outcome: "enriching"})
	started := time.Now()
	if p.c.EnrichmentMode != config.EnrichmentModeAlways && !carryDet {
		done, err := p.saveLocalEnrichment(doc, fm, content, started, base, out)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	contextText := fmt.Sprintf("Current path: %s\nDocument frontmatter: %s\n%s%s",
		doc.Path, cmp.Or(doc.FrontmatterJSON, "none"), p.taxonomyPrompt(doc, fm), frontmatterContext(doc, fm, p.knownDocs))
	prompt, err := prompts.ComposeTask(p.c.PromptDirectory, p.mcpServer, "enrich.md", contextText, content)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(p.ctx, p.c.EnrichmentTimeout())
	defer cancel()
	p.enrichmentUsed = true
	enr, err := runEnrich(ctx, p.gens.Enrich, doc, fm, p.knownDocs, content, prompt, p.taxonomy)
	if parentErr := p.ctx.Err(); parentErr != nil {
		return parentErr
	}
	if err != nil {
		if err := p.db.SaveStageError(p.ctx, doc, "enrich", p.fastModel, err.Error(), time.Since(started)); err != nil {
			return err
		}
		base.Outcome = "enrichment_error"
		base.Detail = "retryable; full analysis remains eligible: " + err.Error()
		p.emit(*base)
		out.EnrichmentFailed++
		return nil
	}
	var sugs []database.Suggestion
	if enr.suggestion != nil {
		sugs = append(sugs, *enr.suggestion)
	}
	if carryDet {
		sugs = append(sugs, detSug)
	}
	tags := normalizeTags(enr.tags, doc.TagsJSON, frontmatter.StringList(fm, "tags"))
	enr.audit.DurationMS = time.Since(started).Milliseconds()
	if err := p.db.SaveEnrichment(p.ctx, doc, p.fastModel, enr.title, enr.summary, tags, sugs, enr.audit); err != nil {
		return err
	}
	base.Outcome = "enriched"
	base.Detail = suggestionDetail(len(sugs), enr.audit.Rejected) + "; patch " + enr.audit.PatchOutcome
	if len(enr.audit.DerivedFields) > 0 {
		base.Detail += "; filled missing " + strings.Join(enr.audit.DerivedFields, ", ")
	}
	p.emit(*base)
	out.Enriched++
	out.Rejected += len(enr.audit.Rejected)
	return nil
}

func (p *pipeline) saveLocalEnrichment(doc database.Document, fm map[string]any, content string, started time.Time, base *Progress, out *Result) (bool, error) {
	local, ok := deterministicEnrichment(doc, fm, content, p.knownDocs, p.taxonomy)
	if !ok {
		return false, nil
	}
	local.audit.DurationMS = time.Since(started).Milliseconds()
	tags := normalizeTags(local.tags, doc.TagsJSON, frontmatter.StringList(fm, "tags"))
	var suggestions []database.Suggestion
	if local.suggestion != nil {
		suggestions = append(suggestions, *local.suggestion)
		local.audit.PatchOutcome = "suggested"
	} else {
		local.audit.PatchOutcome = "unchanged"
	}
	if err := p.db.SaveEnrichment(p.ctx, doc, "deterministic", local.title, "", tags,
		suggestions, local.audit); err != nil {
		return false, err
	}
	base.Outcome = "locally_enriched"
	detail := "local title/tags already valid"
	if len(local.audit.DerivedFields) > 0 {
		detail = "locally derived " + strings.Join(local.audit.DerivedFields, ", ")
	}
	base.Detail = detail + "; " + suggestionDetail(len(suggestions), nil)
	p.emit(*base)
	out.LocalEnriched++
	return true, nil
}

func (p *pipeline) localEnrichOnlyBatch(docs []database.Document, out *Result) error {
	for i, doc := range docs {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		if doc.EnrichedSHA256 == doc.SHA256 && !p.opts.ReanalyzeAll {
			continue
		}
		base := Progress{Index: i + 1, Total: len(docs), Path: doc.Path}
		p.emit(Progress{Index: base.Index, Total: base.Total, Path: base.Path, Outcome: "enriching"})
		content, err := scanner.ReadDocument(p.c, doc.Path)
		if err != nil {
			continue
		}
		fm, body, err := frontmatter.Parse(content)
		if err != nil || frontmatter.NoLLMContext(fm) ||
			frontmatter.IsStub(fm, body, p.c.StubLineLimit()) {
			continue
		}
		if _, err := p.saveLocalEnrichment(doc, fm, content, time.Now(), &base, out); err != nil {
			return err
		}
	}
	return nil
}

type enrichResult struct {
	title      string
	summary    string
	tags       []string
	suggestion *database.Suggestion
	audit      database.StageResult
}

func runEnrich(ctx context.Context, gen Generator, doc database.Document, fm map[string]any, knownDocs []database.Document, content, prompt string, vocabularies ...*taxonomy.Vocabulary) (enrichResult, error) {
	raw, err := gen.Generate(ctx, prompt, enrichSchema)
	if ctx.Err() != nil {
		return enrichResult{}, ctx.Err()
	}
	if err != nil {
		return enrichResult{}, err
	}
	var parsed llmEnrich
	if err := json.Unmarshal([]byte(stripFence(raw)), &parsed); err != nil {
		return enrichResult{}, fmt.Errorf("invalid enrich JSON: %w", err)
	}
	if parsed.Title == nil || parsed.Summary == nil || parsed.Tags == nil {
		return enrichResult{}, fmt.Errorf("enrichment response requires non-null title, summary and tags")
	}
	if strings.TrimSpace(*parsed.Title) == "" || strings.TrimSpace(*parsed.Summary) == "" {
		return enrichResult{}, fmt.Errorf("enrichment title and summary must not be empty")
	}
	out := enrichResult{
		title: strings.TrimSpace(*parsed.Title), summary: strings.TrimSpace(*parsed.Summary),
		tags: normalizeTags(*parsed.Tags, "[]", nil),
	}
	effective, derived := missingMetadataPatch(fm, parsed.Patch, out.title, out.tags)
	var rejected []string
	var vocabulary *taxonomy.Vocabulary
	if len(vocabularies) > 0 {
		vocabulary = vocabularies[0]
	}
	out.suggestion, rejected = patchSuggestion(doc, fm, effective, content, knownDocs, "enrich", vocabulary)
	out.audit = stageResult(parsed.Patch, out.suggestion, rejected)
	out.audit.Metadata = stageMetadata(*parsed.Title, *parsed.Summary, *parsed.Tags)
	out.audit.DerivedFields = derived
	return out, nil
}

// Extracted metadata is already model output. Build missing-field proposals
// locally instead of asking the model to duplicate it inside a patch.
// Explicit patch decisions win, and existing human metadata is never defaulted.
func missingMetadataPatch(fm map[string]any, patch *metadataPatch, title string, tags []string) (metadataPatch, []string) {
	effective := patchValue(patch)
	effective.Set = maps.Clone(effective.Set)
	if effective.Set == nil {
		effective.Set = map[string]any{}
	}
	var derived []string
	for _, field := range []struct {
		key    string
		value  any
		usable bool
	}{
		{"title", title, title != ""},
		{"tags", tags, len(tags) > 0},
	} {
		_, exists := fm[field.key]
		_, explicit := effective.Set[field.key]
		if !exists && !explicit && field.usable && !slices.Contains(effective.Remove, field.key) {
			effective.Set[field.key] = field.value
			derived = append(derived, field.key)
		}
	}
	return effective, derived
}
