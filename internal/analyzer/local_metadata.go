package analyzer

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/taxonomy"
)

// deterministicEnrichment fills only missing title/tags from a clear H1 and
// taxonomy path. Existing valid frontmatter is human-owned and is never replaced.
func deterministicEnrichment(doc database.Document, fm map[string]any, content string, knownDocs []database.Document, vocabulary *taxonomy.Vocabulary) (enrichResult, bool) {
	fields := maps.Clone(fm)
	if fields == nil {
		fields = map[string]any{}
	}
	patch := &metadataPatch{Set: map[string]any{}, Remove: []string{}}
	var title string
	var tags []string
	var derived []string

	if raw, exists := fields["title"]; exists {
		value, ok := raw.(string)
		if !ok || !usableLocalTitle(value) {
			return enrichResult{}, false
		}
		title = strings.TrimSpace(value)
	} else {
		title = markdownH1(content)
		if !usableLocalTitle(title) {
			return enrichResult{}, false
		}
		patch.Set["title"] = title
		derived = append(derived, "title")
	}
	if raw, exists := fields["tags"]; exists {
		var ok bool
		tags, ok = localFrontmatterTags(raw, vocabulary)
		if !ok {
			return enrichResult{}, false
		}
	} else {
		tags = localTaxonomyTags(doc, vocabulary)
		if len(tags) == 0 {
			return enrichResult{}, false
		}
		patch.Set["tags"] = tags
		derived = append(derived, "tags")
	}

	var suggestion *database.Suggestion
	audit := database.StageResult{Outcome: "success", PatchOutcome: "unchanged"}
	if len(patch.Set) > 0 {
		var rejected []string
		suggestion, rejected = patchSuggestion(doc, fm, *patch, content, knownDocs, "deterministic", vocabulary)
		if suggestion == nil || len(rejected) != 0 {
			return enrichResult{}, false
		}
		audit = stageResult(patch, suggestion, nil)
	}
	audit.Metadata = stageMetadata(title, "", tags)
	audit.DerivedFields = derived
	return enrichResult{
		title:      title,
		tags:       tags,
		suggestion: suggestion,
		audit:      audit,
	}, true
}

func localFrontmatterTags(value any, vocabulary *taxonomy.Vocabulary) ([]string, bool) {
	var tags []string
	switch typed := value.(type) {
	case string:
		tags = []string{typed}
	case []string:
		tags = slices.Clone(typed)
	case []any:
		for _, item := range typed {
			tag, ok := item.(string)
			if !ok {
				return nil, false
			}
			tags = append(tags, tag)
		}
	default:
		return nil, false
	}
	for _, tag := range tags {
		if strings.TrimSpace(tag) == "" || tag != strings.ToLower(tag) {
			return nil, false
		}
	}
	if len(vocabulary.InvalidTagSet(tags)) > 0 {
		return nil, false
	}
	return normalizeTags(tags, "[]", nil), true
}

func deterministicMetadataPatch(doc database.Document, fm map[string]any, content string, knownDocs []database.Document, vocabulary *taxonomy.Vocabulary) (metadataPatch, []string, bool) {
	local, ok := deterministicEnrichment(doc, fm, content, knownDocs, vocabulary)
	if !ok {
		return metadataPatch{}, nil, false
	}
	var patch metadataPatch
	if err := json.Unmarshal(local.audit.Patch, &patch); err != nil {
		return metadataPatch{}, nil, false
	}
	if len(patch.Set) == 0 {
		return metadataPatch{}, nil, false
	}
	return patch, local.audit.DerivedFields, true
}

func localMetadataContext(patch metadataPatch) string {
	if len(patch.Set) == 0 {
		return ""
	}
	encoded, err := json.Marshal(patch.Set)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("Local H1/path metadata candidates (not frontmatter yet; verify before proposing): %s\n", encoded)
}

func addLocalMetadataToProposal(content string, patch metadataPatch) (string, []string, error) {
	fields, _, err := frontmatter.Parse(content)
	if err != nil {
		return "", nil, err
	}
	if fields == nil {
		fields = map[string]any{}
	}
	var added []string
	for key, value := range patch.Set {
		if _, exists := fields[key]; !exists {
			fields[key] = value
			added = append(added, key)
		}
	}
	slices.Sort(added)
	rewritten, err := frontmatter.Rewrite(content, fields)
	return rewritten, added, err
}

func markdownH1(content string) string {
	_, body, err := frontmatter.Parse(content)
	if err != nil {
		return ""
	}
	var fence rune
	fenceWidth := 0
	for _, line := range strings.Split(body, "\n") {
		if isIndentedCode(line) {
			continue
		}
		trimmed := strings.TrimSpace(strings.TrimLeft(line, " "))
		if fence != 0 {
			if closingFence(trimmed, fence, fenceWidth) {
				fence = 0
			}
			continue
		}
		if marker, width := openingFence(trimmed); width > 0 {
			fence, fenceWidth = marker, width
			continue
		}
		if !strings.HasPrefix(trimmed, "# ") {
			continue
		}
		title := strings.TrimSpace(strings.TrimPrefix(trimmed, "# "))
		title = trimClosingATXHashes(title)
		return strings.TrimSpace(unescapeEntityPunctuation(title))
	}
	return ""
}

func trimClosingATXHashes(title string) string {
	trimmed := strings.TrimRight(title, " ")
	start := len(trimmed)
	for start > 0 && trimmed[start-1] == '#' {
		start--
	}
	if start < len(trimmed) && start > 0 && trimmed[start-1] == ' ' {
		return strings.TrimSpace(trimmed[:start])
	}
	return strings.TrimSpace(title)
}

func openingFence(line string) (rune, int) {
	if len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return 0, 0
	}
	marker := rune(line[0])
	width := 0
	for _, r := range line {
		if r != marker {
			break
		}
		width++
	}
	if width < 3 {
		return 0, 0
	}
	return marker, width
}

func closingFence(line string, marker rune, width int) bool {
	count := 0
	for _, r := range line {
		if r != marker {
			return false
		}
		count++
	}
	return count >= width
}

func usableLocalTitle(title string) bool {
	title = strings.TrimSpace(title)
	if title == "" || strings.Contains(title, "/") {
		return false
	}
	switch strings.ToLower(strings.Trim(title, "# ")) {
	case "readme", "overview", "index", "untitled", "todo", "documentation":
		return false
	default:
		return true
	}
}

func localTaxonomyTags(doc database.Document, vocabulary *taxonomy.Vocabulary) []string {
	return vocabulary.TagsForPath(doc.Path)
}
