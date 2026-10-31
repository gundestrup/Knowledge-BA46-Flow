// Package proposal validates model-authored file changes before review and
// again before apply. A valid JSON reply is not evidence that an edit is safe.
package proposal

import (
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/links"
	"knowledge-ba46-flow/internal/paths"
)

// EditableField is the metadata allowlist for both model stages. Provenance,
// lifecycle, privacy and unknown fields must remain human-owned.
func EditableField(key string) bool {
	switch key {
	case "title", "tags", "related", "contradicted_by":
		return true
	}
	return false
}

// ValidateMove prevents a model from mistaking a note for a script or config
// described inside it. Destination existence is checked by the caller.
func ValidateMove(source, target string) error {
	src, err := paths.CleanRelative(source)
	if err != nil {
		return err
	}
	dst, err := paths.CleanRelative(target)
	if err != nil {
		return err
	}
	if src == dst {
		return fmt.Errorf("source and target paths are the same")
	}
	if !strings.EqualFold(filepath.Ext(src), filepath.Ext(dst)) {
		return fmt.Errorf("move must preserve the document extension")
	}
	return nil
}

// ValidateRewrite permits additive body edits and allowlisted metadata edits.
// Whitespace reflow is allowed outside code; existing words must remain in
// order and code spans/blocks must survive verbatim (apart from CRLF).
func ValidateRewrite(original, proposed string) error {
	return validateRewrite(original, proposed, nil)
}

// ValidateTagsUnchanged protects body-only proposal stages from silently
// changing metadata when they return a complete document.
func ValidateTagsUnchanged(original, proposed string) error {
	before, _, err := frontmatter.Parse(original)
	if err != nil {
		return fmt.Errorf("original frontmatter: %w", err)
	}
	after, _, err := frontmatter.Parse(proposed)
	if err != nil {
		return fmt.Errorf("proposed frontmatter: %w", err)
	}
	if !slices.Equal(frontmatter.StringList(before, "tags"), frontmatter.StringList(after, "tags")) {
		return fmt.Errorf("proposed content must preserve existing tags")
	}
	return nil
}

// ValidateLinkRepair allows only an unchanged body or the exact local repair
// justified by recorded renames. Origin alone never authorizes a body rewrite.
func ValidateLinkRepair(docPath, original, proposed string, retarget func(string) (string, bool), exists func(string) bool) error {
	return validateRewrite(original, proposed, func(oldBody, newBody string) bool {
		return newBody == oldBody || newBody == links.Retarget(docPath, oldBody, retarget, exists)
	})
}

func validateRewrite(original, proposed string, repair func(string, string) bool) error {
	if strings.TrimSpace(proposed) == "" {
		return fmt.Errorf("missing proposed_content")
	}
	before, oldBody, err := frontmatter.Parse(original)
	if err != nil {
		return fmt.Errorf("original frontmatter: %w", err)
	}
	after, newBody, err := frontmatter.Parse(proposed)
	if err != nil {
		return fmt.Errorf("proposed frontmatter: %w", err)
	}
	if issues := frontmatter.Validate(after); len(issues) > 0 {
		return fmt.Errorf("invalid proposed frontmatter: %s", strings.Join(issues, "; "))
	}
	for key, value := range before {
		if !EditableField(key) {
			next, exists := after[key]
			if !exists || !reflect.DeepEqual(value, next) {
				return fmt.Errorf("protected frontmatter field %q must not change", key)
			}
		}
	}
	for key := range after {
		if _, exists := before[key]; !exists && !EditableField(key) {
			return fmt.Errorf("protected frontmatter field %q must not be added", key)
		}
	}
	if repair != nil {
		if !repair(oldBody, newBody) {
			return fmt.Errorf("proposed body is not an exact verified link repair")
		}
	} else if !retainsWords(oldBody, newBody) {
		return fmt.Errorf("proposed content removes or changes existing body text; only additive edits are supported")
	}
	oldBody = strings.ReplaceAll(oldBody, "\r\n", "\n")
	newBody = strings.ReplaceAll(newBody, "\r\n", "\n")
	counts := map[string]int{}
	for _, span := range codeSpans(oldBody) {
		counts[span]++
	}
	for span, count := range counts {
		if strings.Count(newBody, span) < count {
			return fmt.Errorf("proposed content changes an existing code span or block")
		}
	}
	return nil
}

// ValidateLinks resolves frontmatter links using a caller-supplied, confined
// lookup: the scanned document set during analysis and os.Root during apply.
func ValidateLinks(content string, lookup func(string) error) error {
	fields, _, err := frontmatter.Parse(content)
	if err != nil {
		return err
	}
	for _, key := range []string{"related", "contradicted_by"} {
		for _, link := range frontmatter.StringList(fields, key) {
			local, err := paths.CleanRelative(link)
			if err != nil {
				return fmt.Errorf("%s target %q: %w", key, link, err)
			}
			if err := lookup(filepath.ToSlash(local)); err != nil {
				return fmt.Errorf("%s target %q: %w", key, link, err)
			}
		}
	}
	return nil
}

func retainsWords(original, proposed string) bool {
	want := strings.Fields(original)
	next := 0
	for _, word := range strings.Fields(proposed) {
		if next < len(want) && word == want[next] {
			next++
		}
	}
	return next == len(want)
}

// codeSpans recognizes backtick/tilde fences and matching inline backtick
// runs. Keeping their raw text also protects indentation-sensitive examples.
func codeSpans(body string) []string {
	var spans []string
	var outside, block strings.Builder
	var fence byte
	var width int
	for line := range strings.SplitAfterSeq(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if fence != 0 {
			block.WriteString(line)
			if len(trimmed) >= width && strings.Trim(trimmed, string(fence)) == "" {
				spans = append(spans, strings.TrimSuffix(block.String(), "\n"))
				block.Reset()
				fence = 0
			}
			continue
		}
		if len(trimmed) >= 3 && (trimmed[0] == '`' || trimmed[0] == '~') {
			run := len(trimmed) - len(strings.TrimLeft(trimmed, string(trimmed[0])))
			if run >= 3 {
				fence, width = trimmed[0], run
				block.WriteString(line)
				continue
			}
		}
		outside.WriteString(line)
	}
	if block.Len() > 0 {
		spans = append(spans, block.String())
	}
	return append(spans, inlineCodeSpans(outside.String())...)
}

func inlineCodeSpans(body string) []string {
	var spans []string
	for start := 0; start < len(body); {
		offset := strings.IndexByte(body[start:], '`')
		if offset < 0 {
			break
		}
		start += offset
		width := 1
		for start+width < len(body) && body[start+width] == '`' {
			width++
		}
		end := start + width
		matched := false
		for end < len(body) {
			if body[end] != '`' {
				end++
				continue
			}
			run := 1
			for end+run < len(body) && body[end+run] == '`' {
				run++
			}
			if run == width {
				spans = append(spans, body[start:end+run])
				start = end + run
				matched = true
				break
			}
			end += run
		}
		if !matched {
			start += width
		}
	}
	return spans
}
