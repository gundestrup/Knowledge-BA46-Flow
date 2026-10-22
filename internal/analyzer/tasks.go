// Optional per-task LLM passes. Unlike the always-on analyze/run pipeline,
// each task is an explicit op with its own lineage columns, model routing
// and prompt contract:
//
//	expand    — fill a stub document following its category's STUB.md
//	            template, so the note grows without drifting into Wikipedia
//	            prose. Produces a normal reviewable `expand` suggestion.
//	freshness — heuristic currentness review: the model flags dated claims
//	            (version pins, "latest release", ageing dates) it cannot
//	            verify; the verdict lands in the note's kbm.freshness block.
//	embed     — recompute semantic vectors for new/changed documents.
//
// expand and freshness default to the external lane model when one is
// configured — they are the tasks a stronger model is for — and every task
// accepts a per-run model override naming any configured model.
package analyzer

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/paths"
	"knowledge-ba46-flow/internal/prompts"
	"knowledge-ba46-flow/internal/scanner"
	"knowledge-ba46-flow/internal/similarity"
)

// TaskResult summarizes one expand/freshness/embed run for the job detail
// line and the CLI.
type TaskResult struct {
	Processed   int // documents the model answered for
	Skipped     int // policy/scope skips (no_llm_context, non-stub, stub on freshness)
	NoTemplate  int // expand only: stubs refused because no STUB.md applies
	Failed      int
	Rejected    int // unsafe model proposals omitted from review
	Suggestions int
}

// Summary renders the one-line job/CLI detail.
func (r TaskResult) Summary(task, model string) string {
	return fmt.Sprintf("%s: %d processed; %d skipped; %d without template; %d failed; %d suggestion(s); %d rejected unsafe edit(s) (model %s).",
		task, r.Processed, r.Skipped, r.NoTemplate, r.Failed, r.Suggestions, r.Rejected, model)
}

// expandResult mirrors the contract in prompts/expand.md.
type expandResult struct {
	Title           string   `json:"title"`
	Summary         string   `json:"summary"`
	Tags            []string `json:"tags"`
	Reason          string   `json:"reason"`
	Confidence      float64  `json:"confidence"`
	ProposedContent string   `json:"proposed_content"`
}

// freshnessResult mirrors the contract in prompts/freshness.md: a verdict,
// human-readable summary and flags, plus optional reviewable update
// suggestions in the analysis suggestion shape.
type freshnessResult struct {
	Current     bool            `json:"current"`
	Summary     string          `json:"summary"`
	Flags       []freshnessFlag `json:"flags"`
	Suggestions []llmSuggestion `json:"suggestions"`
}

type freshnessFlag struct {
	Claim   string `json:"claim"`
	Concern string `json:"concern"`
}

var expandSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "title":    {"type": "string"},
    "summary":  {"type": "string"},
    "tags":     {"type": "array", "items": {"type": "string"}},
    "reason":   {"type": "string"},
    "confidence": {"type": "number", "minimum": 0, "maximum": 1},
    "proposed_content": {"type": "string"}
  },
  "required": ["title", "summary", "tags", "reason", "confidence", "proposed_content"]
}`)

var freshnessSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "current": {"type": "boolean"},
    "summary": {"type": "string"},
    "flags": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "claim":   {"type": "string"},
          "concern": {"type": "string"}
        },
        "required": ["claim", "concern"]
      }
    },
    "suggestions": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "action":           {"type": "string", "enum": ["none", "expand"]},
          "target_path":      {"type": "string", "enum": [""]},
          "reason":           {"type": "string"},
          "confidence":       {"type": "number", "minimum": 0, "maximum": 1},
          "proposed_content": {"type": "string"}
        },
        "required": ["action", "target_path", "reason", "confidence", "proposed_content"]
      }
    }
  },
  "required": ["current", "summary", "flags", "suggestions"]
}`)

// RunTask runs one optional task over the corpus: pending selection is
// incremental against the task's own lineage columns, and the task prompt is
// prompts/<task>.md composed with the same base/server files as analysis.
func RunTask(ctx context.Context, c config.Config, db *database.DB, gens Generators, task string, opts RunOptions) (TaskResult, error) {
	model, err := c.TaskModel(task, opts.External, opts.Model)
	if err != nil {
		return TaskResult{}, err
	}
	if gens.Analysis == nil {
		return TaskResult{}, fmt.Errorf("%s needs a generation client for model %s", task, model)
	}
	docs, err := db.PendingTask(ctx, task, model, opts.Reanalyze, opts.ReanalyzeAll)
	if err != nil {
		return TaskResult{}, err
	}
	out := TaskResult{}
	if len(docs) == 0 {
		return out, nil
	}
	p := &pipeline{
		ctx: ctx, c: c, db: db, gens: gens, opts: opts,
		emit: func(pr Progress) {
			if opts.Progress != nil {
				opts.Progress(pr)
			}
		},
		fastModel: c.FastModel(),
	}
	if err := p.loadCorpus(); err != nil {
		return out, err
	}
	root, err := os.OpenRoot(c.RepositoryPath)
	if err != nil {
		return out, err
	}
	defer root.Close()

	for i, doc := range docs {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		base := Progress{Index: i + 1, Total: len(docs), Path: doc.Path}
		var step error
		switch task {
		case config.TaskExpand:
			step = p.expandDoc(root, doc, model, &base, &out)
		case config.TaskFreshness:
			step = p.freshnessDoc(root, doc, model, &base, &out)
		default:
			return out, fmt.Errorf("unknown task %q", task)
		}
		if step != nil {
			return out, step
		}
	}
	return out, nil
}

// readTaskDocument loads and parses one document for a task pass, applying
// the same fail-closed policy checks as the analysis pipeline.
func (p *pipeline) readTaskDocument(doc database.Document, base *Progress, out *TaskResult) (map[string]any, string, string, error) {
	content, err := scanner.ReadDocument(p.c, doc.Path)
	if err != nil {
		return nil, "", "", p.taskFail(base, out, err.Error())
	}
	fm, body, err := frontmatter.Parse(content)
	if err != nil {
		return nil, "", "", p.taskFail(base, out, "invalid frontmatter: "+err.Error())
	}
	return fm, body, content, nil
}

func (p *pipeline) taskFail(base *Progress, out *TaskResult, detail string) error {
	if err := p.db.SaveError(p.ctx, base.Path, detail); err != nil {
		return err
	}
	base.Outcome, base.Detail = "error", detail
	p.emit(*base)
	out.Failed++
	return nil
}

// taskSkip stamps the task lineage for a document the task cannot or should
// not process — private content and out-of-scope shapes stay out of the
// pending set on the next run without a stage-results row.
func (p *pipeline) taskSkip(doc database.Document, task, model, detail string, base *Progress, out *TaskResult) error {
	if err := p.db.StampTask(p.ctx, doc, task, model); err != nil {
		return err
	}
	base.Outcome, base.Detail = "skipped", detail
	p.emit(*base)
	out.Skipped++
	return nil
}

// expandDoc turns a stub into a reviewable expansion: the nearest ancestor
// STUB.md supplies the category skeleton, the model fills it in, and the
// complete proposed document goes through the normal suggestion review.
// Stubs keep their linkable role — the human removes the `stub` tag when the
// note is grown, which the proposal may legitimately do via `tags`.
func (p *pipeline) expandDoc(root *os.Root, doc database.Document, model string, base *Progress, out *TaskResult) error {
	fm, body, content, err := p.readTaskDocument(doc, base, out)
	if err != nil {
		return err
	}
	if frontmatter.NoLLMContext(fm) {
		return p.taskSkip(doc, config.TaskExpand, model, "skipped: kb_policy.no_llm_context", base, out)
	}
	if !frontmatter.IsStub(fm, body, p.c.StubLineLimit()) {
		return p.taskSkip(doc, config.TaskExpand, model, "not a stub", base, out)
	}
	template, err := stubTemplate(root, doc.Path)
	if err != nil {
		return p.taskFail(base, out, err.Error())
	}
	if template == "" {
		base.Outcome, base.Detail = "skipped", "no STUB.md template in any ancestor directory"
		p.emit(*base)
		out.NoTemplate++
		return nil
	}
	p.emit(Progress{Index: base.Index, Total: base.Total, Path: base.Path, Outcome: "expanding"})
	contextText := fmt.Sprintf("Current path: %s\nCategory template (follow its section structure exactly):\n%s\nDocument frontmatter: %s\n%s%sRelated documents in this repository (link candidates for related:):\n%s",
		doc.Path, template, cmp.Or(doc.FrontmatterJSON, "none"),
		p.taxonomyPrompt(doc, fm), frontmatterContext(doc, fm, p.knownDocs),
		renderTaskCandidates(doc.Path, fm, p.knownDocs, p.visiblePaths))
	prompt, err := prompts.ComposeTask(p.c.PromptDirectory, p.mcpServer, "expand.md", contextText, content)
	if err != nil {
		return err
	}
	var parsed expandResult
	_, dur, err := p.generateTask(prompt, expandSchema, &parsed)
	if err != nil {
		return p.taskFail(base, out, err.Error())
	}
	sugs, rejected := p.expandSuggestion(doc, content, parsed)
	audit := database.StageResult{
		DurationMS: dur.Milliseconds(),
		Metadata:   stageMetadata(parsed.Title, parsed.Summary, parsed.Tags),
		Rejected:   rejected,
	}
	if err := p.db.SaveTask(p.ctx, doc, config.TaskExpand, model, sugs, audit); err != nil {
		return err
	}
	base.Outcome = "expanded"
	base.Detail = suggestionDetail(len(sugs), rejected)
	p.emit(*base)
	out.Processed++
	out.Suggestions += len(sugs)
	out.Rejected += len(rejected)
	return nil
}

// freshnessDoc asks the model whether a document reads as still-current:
// the verdict and per-claim flags are recorded in the note's kbm.freshness
// block (hash-immune), and concrete corrections arrive as ordinary pending
// suggestions. The model cannot verify live facts — flags say "check this",
// never "this is dead".
func (p *pipeline) freshnessDoc(root *os.Root, doc database.Document, model string, base *Progress, out *TaskResult) error {
	fm, body, content, err := p.readTaskDocument(doc, base, out)
	if err != nil {
		return err
	}
	if frontmatter.NoLLMContext(fm) {
		return p.taskSkip(doc, config.TaskFreshness, model, "skipped: kb_policy.no_llm_context", base, out)
	}
	if frontmatter.IsStub(fm, body, p.c.StubLineLimit()) {
		return p.taskSkip(doc, config.TaskFreshness, model, "skipped: stub placeholder", base, out)
	}
	p.emit(Progress{Index: base.Index, Total: base.Total, Path: base.Path, Outcome: "checking freshness"})
	var kbmNote string
	if kbm, ok := fm[frontmatter.ManagedKey].(map[string]any); ok {
		if lc, ok := kbm["link_check"].(map[string]any); ok {
			b, _ := json.Marshal(lc)
			kbmNote = "Latest external link check result: " + string(b) + "\n"
		}
	}
	contextText := fmt.Sprintf("Current path: %s\nFile modified: %s\nDocument frontmatter: %s\n%s",
		doc.Path, time.Unix(doc.ModifiedUnix, 0).UTC().Format("2006-01-02"),
		cmp.Or(doc.FrontmatterJSON, "none"), kbmNote)
	prompt, err := prompts.ComposeTask(p.c.PromptDirectory, p.mcpServer, "freshness.md", contextText, content)
	if err != nil {
		return err
	}
	var parsed freshnessResult
	raw, dur, err := p.generateTask(prompt, freshnessSchema, &parsed)
	if err != nil {
		return p.taskFail(base, out, err.Error()+"; response: "+truncate(raw, 400))
	}
	verdict := "current"
	if len(parsed.Flags) > 0 || !parsed.Current {
		verdict = "review"
	}
	flags := make([]map[string]any, 0, len(parsed.Flags))
	for _, f := range parsed.Flags {
		flags = append(flags, map[string]any{"claim": f.Claim, "concern": f.Concern})
	}
	// The kbm.freshness block is tool-owned operational metadata — the one
	// documented file write outside apply. The canonical hash ignores it, so
	// recording the check never requeues analysis.
	if err := frontmatter.WriteManaged(root, doc.Path, "freshness", map[string]any{
		"at":      time.Now().UTC().Format(time.RFC3339),
		"model":   model,
		"verdict": verdict,
		"summary": parsed.Summary,
		"flags":   flags,
	}); err != nil {
		return p.taskFail(base, out, "record kbm.freshness: "+err.Error())
	}
	if refreshed, rerr := scanner.ReadDocument(p.c, doc.Path); rerr == nil {
		if fields, _, perr := frontmatter.Parse(refreshed); perr == nil {
			fmJSON, _ := json.Marshal(fields)
			issues, _ := json.Marshal(frontmatter.Validate(fields))
			_ = p.db.RefreshFrontmatter(p.ctx, doc.Path, string(fmJSON), string(issues))
		}
	}
	validated := p.validateModelSuggestions(doc, content, nil, parsed.Suggestions, metadataPatch{}, false)
	var sugs []database.Suggestion
	for i := range validated.items {
		validated.items[i].Origin = "freshness"
		sugs = append(sugs, validated.items[i])
	}
	audit := database.StageResult{
		DurationMS: dur.Milliseconds(),
		Rejected:   validated.rejected,
	}
	if err := p.db.SaveTask(p.ctx, doc, config.TaskFreshness, model, sugs, audit); err != nil {
		return err
	}
	base.Outcome = "checked"
	base.Detail = fmt.Sprintf("verdict %s; %d flag(s)", verdict, len(parsed.Flags))
	if len(validated.rejected) > 0 {
		base.Detail += "; " + suggestionDetail(len(sugs), validated.rejected)
	}
	p.emit(*base)
	out.Processed++
	out.Suggestions += len(sugs)
	out.Rejected += len(validated.rejected)
	return nil
}

// generateTask issues one task generation with the shared annotated retry on
// malformed JSON — the same recovery the analysis pass uses.
func (p *pipeline) generateTask(prompt string, schema json.RawMessage, out any) (string, time.Duration, error) {
	started := time.Now()
	raw, err := p.gens.Analysis.Generate(p.ctx, prompt, schema)
	if err != nil {
		return raw, time.Since(started), err
	}
	if uerr := json.Unmarshal([]byte(stripFence(raw)), out); uerr != nil {
		raw, err = p.gens.Analysis.Generate(p.ctx, prompt+jsonRetryHint(uerr), schema)
		if err != nil {
			return raw, time.Since(started), err
		}
		if uerr := json.Unmarshal([]byte(stripFence(raw)), out); uerr != nil {
			return raw, time.Since(started), fmt.Errorf("invalid JSON: %w", uerr)
		}
	}
	return raw, time.Since(started), nil
}

// expandSuggestion validates the model's expanded document through the same
// proposal guards as analysis: only additive body edits, allowlisted
// frontmatter fields, resolvable links.
func (p *pipeline) expandSuggestion(doc database.Document, content string, parsed expandResult) ([]database.Suggestion, []string) {
	sug := database.Suggestion{
		DocumentPath:    doc.Path,
		Action:          "expand",
		Reason:          strings.TrimSpace(parsed.Reason),
		Confidence:      clamp(parsed.Confidence),
		ProposedContent: parsed.ProposedContent,
		Origin:          "expand",
	}
	if err := p.validateSuggestion(sug, content, nil, false); err != nil {
		return nil, []string{"expand: " + err.Error()}
	}
	return []database.Suggestion{sug}, nil
}

// stubTemplate finds the expansion template for a stub: the nearest STUB.md
// walking up from the stub's directory to the repository root. Empty string
// means no template applies — expansion is refused rather than improvised.
func stubTemplate(root *os.Root, docPath string) (string, error) {
	dir := filepath.ToSlash(filepath.Dir(docPath))
	for {
		candidate := scanner.StubTemplateName
		if dir != "." {
			candidate = dir + "/" + scanner.StubTemplateName
		}
		// Guard case-insensitive filesystems (default macOS): a stub named
		// stub.md must not resolve as its own template.
		if strings.EqualFold(candidate, docPath) {
			dir = filepath.ToSlash(filepath.Dir(dir))
			continue
		}
		local, err := paths.CleanRelative(candidate)
		if err != nil {
			return "", err
		}
		if b, err := root.ReadFile(local); err == nil {
			return string(b), nil
		}
		if dir == "." {
			return "", nil
		}
		dir = filepath.ToSlash(filepath.Dir(dir))
	}
}

// renderTaskCandidates lists the visible corpus for a task prompt — paths
// plus title/tags where known, already linked entries first. Cheaper than
// the analysis ranker: expand only needs link targets, not similarity.
func renderTaskCandidates(docPath string, fm map[string]any, known []database.Document, visible map[string]bool) string {
	linked := map[string]bool{}
	for _, key := range []string{"related", "contradicted_by"} {
		for _, l := range frontmatter.StringList(fm, key) {
			linked[filepath.ToSlash(strings.TrimSpace(l))] = true
		}
	}
	var lines []string
	for _, d := range known {
		if d.Path == docPath || !visible[d.Path] {
			continue
		}
		line := "- " + d.Path
		if t := strings.TrimSpace(d.Title); t != "" {
			line += ` — "` + t + `"`
		}
		var tags []string
		if err := json.Unmarshal([]byte(d.TagsJSON), &tags); err == nil && len(tags) > 0 {
			line += " [" + strings.Join(tags, ", ") + "]"
		}
		if linked[d.Path] {
			line += " (already linked)"
		}
		lines = append(lines, line)
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

// RunEmbed recomputes semantic vectors for documents whose canonical hash
// moved past the embedded stamp — the standalone form of the embedding
// backfill the analysis pipeline performs. Unlike the in-pipeline backfill a
// transport failure aborts the run: an explicit embed op with a dead embed
// model should fail loudly, not silently degrade.
func RunEmbed(ctx context.Context, c config.Config, db *database.DB, gens Generators, opts RunOptions) (TaskResult, error) {
	override := opts.EmbedModel
	if override == "" {
		override = opts.Model // backwards-compatible standalone embed --model
	}
	model, err := c.TaskModel(config.TaskEmbed, false, override)
	if err != nil || model == "" {
		return TaskResult{}, errors.New("embed requires ollama_embed_model (or an ollama_task_models.embed default) in the config")
	}
	if gens.Embed == nil {
		return TaskResult{}, fmt.Errorf("embed needs an embedding client for model %s", model)
	}
	docs, err := db.PendingTask(ctx, config.TaskEmbed, model, opts.Reanalyze, opts.ReanalyzeAll)
	if err != nil {
		return TaskResult{}, err
	}
	out := TaskResult{}
	for i, doc := range docs {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		base := Progress{Index: i + 1, Total: len(docs), Path: doc.Path}
		if opts.Progress != nil {
			opts.Progress(Progress{Index: base.Index, Total: base.Total, Path: doc.Path, Outcome: "embedding"})
		}
		content, err := scanner.ReadDocument(c, doc.Path)
		if err != nil {
			out.Failed++
			continue
		}
		fm, _, err := frontmatter.Parse(content)
		if err != nil || frontmatter.NoLLMContext(fm) {
			// Private or unparseable content is never embedded; stamp the
			// lineage so it stays out of the pending set.
			if err := db.StampTask(ctx, doc, config.TaskEmbed, model); err != nil {
				return out, err
			}
			out.Skipped++
			continue
		}
		content, err = embeddingText(doc, content)
		if err != nil {
			return out, err
		}
		vec, err := gens.Embed.Embed(ctx, content)
		if err != nil {
			return out, fmt.Errorf("embed %s: %w", doc.Path, err)
		}
		if err := db.SaveEmbedding(ctx, doc, model, similarity.MarshalEmbedding(vec)); err != nil {
			return out, err
		}
		out.Processed++
	}
	return out, nil
}
