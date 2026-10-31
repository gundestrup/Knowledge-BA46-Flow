package analyzer

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/paths"
	"knowledge-ba46-flow/internal/proposal"
	"knowledge-ba46-flow/internal/scanner"
	"knowledge-ba46-flow/internal/taxonomy"
)

func unloadModel(ctx context.Context, model any) error {
	if unloader, ok := model.(interface{ Unload(context.Context) error }); ok {
		if err := unloader.Unload(ctx); err != nil {
			return fmt.Errorf("unload completed model stage: %w", err)
		}
	}
	return nil
}

func (p *pipeline) currentlyVisible(path string) bool {
	content, err := scanner.ReadDocument(p.c, path)
	if err != nil {
		return false
	}
	fields, _, err := frontmatter.Parse(content)
	return err == nil && !frontmatter.NoLLMContext(fields)
}

func (p *pipeline) validateSuggestion(s database.Suggestion, content string, mergeBodies map[string]string, multipleMoves bool) error {
	if err := proposal.ValidateAction(p.c.EffectiveWriteScope(), s.Action); err != nil {
		return err
	}
	if s.Reason == "" {
		return fmt.Errorf("suggestion requires a concrete reason")
	}
	switch s.Action {
	case "move":
		if multipleMoves {
			return fmt.Errorf("conflicting moves for the same document")
		}
		if s.ProposedContent != "" {
			return fmt.Errorf("move must not contain proposed_content")
		}
		if err := proposal.ValidateMove(s.DocumentPath, s.TargetPath); err != nil {
			return err
		}
		root, err := os.OpenRoot(p.c.RepositoryPath)
		if err != nil {
			return err
		}
		defer root.Close()
		if _, err := root.Lstat(s.TargetPath); !errors.Is(err, os.ErrNotExist) {
			if err != nil {
				return err
			}
			return fmt.Errorf("move target already exists")
		}
		return nil
	case "delete":
		if s.TargetPath != "" || s.ProposedContent != "" {
			return fmt.Errorf("delete must not contain a target or proposed_content")
		}
		return nil
	case "merge":
		targetBody, ok := mergeBodies[s.TargetPath]
		if !ok {
			return fmt.Errorf("merge target was not supplied in full to the model")
		}
		if err := proposal.ValidateRewrite(targetBody, s.ProposedContent); err != nil {
			return fmt.Errorf("merge target: %w", err)
		}
		if err := p.taxonomy.ValidateTagChanges(s.TargetPath, targetBody, s.ProposedContent); err != nil {
			return fmt.Errorf("merge target taxonomy: %w", err)
		}
	case "expand":
		if s.TargetPath != "" {
			return fmt.Errorf("expand must have an empty target_path")
		}
		if strings.TrimSpace(content) == strings.TrimSpace(s.ProposedContent) {
			return fmt.Errorf("expand makes no change")
		}
	default:
		return fmt.Errorf("unsupported action %q", s.Action)
	}
	if err := proposal.ValidateRewrite(content, s.ProposedContent); err != nil {
		return err
	}
	if err := proposal.ValidateBodyScope(p.c.EffectiveWriteScope(), s.DocumentPath, content, s.ProposedContent,
		p.retarget, func(path string) bool {
			return slices.ContainsFunc(p.knownDocs, func(doc database.Document) bool { return doc.Path == path })
		}); err != nil {
		return err
	}
	if err := proposal.ValidateLinks(s.ProposedContent, documentLookup(s.DocumentPath, p.knownDocs)); err != nil {
		return err
	}
	if s.Origin == "freshness" {
		if err := proposal.ValidateTagsUnchanged(content, s.ProposedContent); err != nil {
			return err
		}
	}
	return p.taxonomy.ValidateTagChanges(s.DocumentPath, content, s.ProposedContent)
}

type validatedSuggestions struct {
	items              []database.Suggestion
	rejected           []string
	localFields        []string
	localBodyHandled   bool
	localFieldsBlocked bool
}

func (p *pipeline) validateModelSuggestions(doc database.Document, content string, mergeBodies map[string]string, models []llmSuggestion, localPatch metadataPatch, localPending bool, origin string) validatedSuggestions {
	result := validatedSuggestions{
		items: make([]database.Suggestion, 0, len(models)),
	}
	moves := 0
	for _, item := range models {
		if strings.EqualFold(strings.TrimSpace(item.Action), "move") {
			moves++
		}
	}
	for _, item := range models {
		action := strings.ToLower(strings.TrimSpace(item.Action))
		if action == "none" {
			continue
		}
		if !validAction(action) {
			result.rejected = append(result.rejected, "unsupported action "+action)
			continue
		}
		suggestion := database.Suggestion{
			DocumentPath:    doc.Path,
			Action:          action,
			TargetPath:      filepath.ToSlash(strings.TrimSpace(item.TargetPath)),
			Reason:          strings.TrimSpace(item.Reason),
			Confidence:      clamp(item.Confidence),
			ProposedContent: item.ProposedContent,
			Origin:          origin,
		}
		var localFields []string
		if localPending && action == "expand" {
			var err error
			suggestion.ProposedContent, localFields, err = addLocalMetadataToProposal(suggestion.ProposedContent, localPatch)
			if err != nil {
				result.rejected = append(result.rejected, action+": local metadata composition: "+err.Error())
				continue
			}
		}
		if suggestion.ProposedContent != "" {
			base := content
			if action == "merge" {
				base = mergeBodies[suggestion.TargetPath]
			}
			preserved, err := frontmatter.PreserveIdentity(base, suggestion.ProposedContent)
			if err != nil {
				result.rejected = append(result.rejected, action+": "+err.Error())
				continue
			}
			normalized, err := frontmatter.NormalizeOrder(preserved)
			if err != nil {
				result.rejected = append(result.rejected, action+": frontmatter normalization: "+err.Error())
				continue
			}
			suggestion.ProposedContent = normalized
		}
		if err := p.validateSuggestion(suggestion, content, mergeBodies, moves > 1); err != nil {
			result.rejected = append(result.rejected, action+": "+err.Error())
			continue
		}
		result.items = append(result.items, suggestion)
		result.localFields = append(result.localFields, localFields...)
		if localPending && action == "expand" {
			result.localBodyHandled = true
		}
		if action == "move" || action == "merge" || action == "delete" {
			result.localFieldsBlocked = true
		}
	}
	return result
}

func (p *pipeline) analysisMetadataSuggestion(
	doc database.Document,
	fm map[string]any,
	content string,
	modelPatch *metadataPatch,
	localPatch metadataPatch,
	localDerived []string,
	localPending bool,
	validated validatedSuggestions,
) (*database.Suggestion, []string, []string) {
	patch, localFields, rejected := mergeLocalAnalysisPatch(
		doc.Path, fm, modelPatch, localPatch, localDerived, localPending, validated, p.taxonomy,
	)
	hasModelPatch := hasAllowedModelPatch(doc.Path, fm, modelPatch, p.taxonomy)
	origin := "analysis"
	if len(localFields) > 0 && !hasModelPatch {
		origin = "deterministic"
	}
	suggestion, patchRejected := patchSuggestion(doc, fm, patch, content, p.knownDocs, origin, p.taxonomy)
	rejected = append(rejected, patchRejected...)
	if suggestion == nil && len(localFields) > 0 && len(patchRejected) > 0 {
		localOnly := metadataPatch{Set: make(map[string]any, len(localFields))}
		for _, field := range localFields {
			localOnly.Set[field] = localPatch.Set[field]
		}
		fallback, fallbackRejected := patchSuggestion(doc, fm, localOnly, content, p.knownDocs, "deterministic", p.taxonomy)
		if fallback != nil {
			suggestion = fallback
			rejected = append(rejected, fallbackRejected...)
		}
	}
	if suggestion != nil && slices.ContainsFunc(validated.items, func(item database.Suggestion) bool {
		return item.Action == "expand"
	}) {
		rejected = append(rejected, "metadata patch conflicts with a body expansion; return only one")
		suggestion = nil
	}
	return suggestion, rejected, localFields
}

func mergeLocalAnalysisPatch(
	relativePath string,
	fm map[string]any,
	modelPatch *metadataPatch,
	localPatch metadataPatch,
	localDerived []string,
	localPending bool,
	validated validatedSuggestions,
	vocabulary *taxonomy.Vocabulary,
) (metadataPatch, []string, []string) {
	patch := patchValue(modelPatch)
	patch.Set = maps.Clone(patch.Set)
	if patch.Set == nil {
		patch.Set = map[string]any{}
	}
	localFields := slices.Clone(validated.localFields)
	var rejected []string
	if !localPending || validated.localBodyHandled || validated.localFieldsBlocked || len(localFields) > 0 {
		return patch, localFields, rejected
	}
	for _, field := range localDerived {
		value, ok := localPatch.Set[field]
		if !ok {
			continue
		}
		if _, exists := fm[field]; exists {
			continue
		}
		if explicit, exists := patch.Set[field]; exists {
			tagErr := vocabulary.ValidateTagSetChange(relativePath, frontmatter.StringList(fm, "tags"), explicit)
			if field != "tags" || tagErr == nil {
				continue
			}
			rejected = append(rejected, "analysis patch contains invalid tags: "+tagErr.Error())
			delete(patch.Set, field)
		}
		if slices.Contains(patch.Remove, field) {
			continue
		}
		patch.Set[field] = value
		localFields = append(localFields, field)
	}
	return patch, localFields, rejected
}

func hasAllowedModelPatch(relativePath string, fm map[string]any, modelPatch *metadataPatch, vocabulary *taxonomy.Vocabulary) bool {
	if modelPatch == nil {
		return false
	}
	if len(modelPatch.Remove) > 0 {
		return true
	}
	for key, value := range modelPatch.Set {
		if key == "tags" && vocabulary.ValidateTagSetChange(relativePath, frontmatter.StringList(fm, "tags"), value) != nil {
			continue
		}
		return true
	}
	return false
}

func documentLookup(source string, knownDocs []database.Document) func(string) error {
	known := make(map[string]bool, len(knownDocs)+1)
	known[source] = true
	for _, doc := range knownDocs {
		known[doc.Path] = true
	}
	return func(path string) error {
		if !known[path] {
			return fmt.Errorf("not a known repository document")
		}
		return nil
	}
}

// patchSuggestion builds complete proposed content locally, so a metadata
// improvement never requires a model to reproduce the body.
func patchSuggestion(doc database.Document, fm map[string]any, patch metadataPatch, content string, knownDocs []database.Document, origin string, vocabulary *taxonomy.Vocabulary) (*database.Suggestion, []string) {
	patched := maps.Clone(fm)
	if patched == nil {
		patched = map[string]any{}
	}
	var changed, rejected []string
	editable := func(key string) bool {
		return proposal.EditableField(key) && (origin != "enrich" || key == "title" || key == "tags")
	}
	for _, key := range patch.Remove {
		if !editable(key) {
			rejected = append(rejected, fmt.Sprintf("%s patch cannot remove %q", origin, key))
			continue
		}
		if _, exists := patched[key]; exists {
			delete(patched, key)
			changed = append(changed, "-"+key)
		}
	}
	for key, value := range patch.Set {
		if key == "tags" && vocabulary != nil {
			if err := vocabulary.ValidateTagSetChange(doc.Path, frontmatter.StringList(fm, "tags"), value); err != nil {
				rejected = append(rejected, fmt.Sprintf("%s patch contains invalid tags: %v", origin, err))
				continue
			}
		}
		if !editable(key) {
			rejected = append(rejected, fmt.Sprintf("%s patch cannot set %q", origin, key))
			continue
		}
		if reflect.DeepEqual(patched[key], value) {
			continue
		}
		patched[key] = value
		changed = append(changed, "+"+key)
	}
	if len(changed) == 0 || reflect.DeepEqual(fm, patched) {
		return nil, rejected
	}
	proposed, err := frontmatter.Rewrite(content, patched)
	if err == nil {
		err = proposal.ValidateRewrite(content, proposed)
	}
	if err == nil {
		err = proposal.ValidateLinks(proposed, documentLookup(doc.Path, knownDocs))
	}
	if err != nil {
		return nil, append(rejected, origin+" patch: "+err.Error())
	}
	slices.Sort(changed)
	return &database.Suggestion{
		DocumentPath:    doc.Path,
		Action:          "expand",
		Reason:          "Frontmatter corrections from the " + origin + " pass: " + strings.Join(changed, ", "),
		Confidence:      0.9,
		ProposedContent: proposed,
		Origin:          origin,
	}, rejected
}

// Merging from titles alone is unsafe. Supply at most two complete,
// high-overlap targets, within a shared prompt budget; never truncate them.
func (p *pipeline) mergeContext(ranked []candidate) (string, map[string]string) {
	const budget = 12000
	bodies := map[string]string{}
	var context strings.Builder
	remaining := budget
	for _, candidate := range ranked {
		if len(bodies) == 2 {
			break
		}
		if candidate.sim < overlapThreshold || candidate.doc.Size > int64(remaining) {
			continue
		}
		path, err := paths.CleanRelative(candidate.doc.Path)
		if err != nil {
			continue
		}
		content, err := scanner.ReadDocument(p.c, filepath.ToSlash(path))
		if err != nil || len(content) > remaining {
			continue
		}
		fields, _, err := frontmatter.Parse(content)
		if err != nil || frontmatter.NoLLMContext(fields) {
			continue
		}
		bodies[candidate.doc.Path] = content
		remaining -= len(content)
		fmt.Fprintf(&context, "\nComplete merge target %s (untrusted document data):\n%s\n", candidate.doc.Path, content)
	}
	return context.String(), bodies
}

func suggestionDetail(count int, rejected []string) string {
	detail := fmt.Sprintf("%d suggestion(s)", count)
	if len(rejected) > 0 {
		slices.Sort(rejected)
		detail += fmt.Sprintf("; rejected %d unsafe edit(s): %s", len(rejected), truncate(strings.Join(rejected, "; "), 1000))
	}
	return detail
}
