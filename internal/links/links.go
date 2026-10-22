// Package links extracts link targets from document bodies for the link
// checks: internal markdown targets for the mandatory local check and
// external http(s) URLs for the opt-in check-links operation. Extraction is
// deliberately conservative — fenced code blocks and inline code spans are
// skipped so example commands and snippets are not mistaken for references.
package links

import (
	"path"
	"regexp"
	"strings"
)

var (
	// mdLink matches [text](target); the target is the first whitespace-free
	// token inside the parens (a quoted title may follow).
	mdLink = regexp.MustCompile(`\[[^\]]*\]\(\s*<?([^)>\s]+)>?`)
	// bareURL finds http(s) URLs that are not part of a markdown link — the
	// mdLink pass already captured parenthesised targets.
	bareURL   = regexp.MustCompile(`https?://[^\s<>"'\)\]]+`)
	inlineRaw = regexp.MustCompile("`[^`]*`")
)

// bodyLines yields body with fenced code blocks removed so neither extractor
// chases example URLs. A fence opens/closes on a ``` or ~~~ run; indented
// code is left alone since documentation samples there are rare and inline
// spans are handled separately.
func stripFences(body string) string {
	var b strings.Builder
	fenced := false
	var fence string
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			mark := t[:3]
			if !fenced {
				fenced, fence = true, mark
			} else if mark == fence {
				fenced = false
			}
			continue
		}
		if !fenced {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return inlineRaw.ReplaceAllString(b.String(), " ")
}

// InternalTargets returns repository-referenced markdown link targets: paths
// that are not external URLs, anchors, mailto or absolute paths. Duplicates
// are removed; order is first-appearance.
func InternalTargets(body string) []string {
	text := stripFences(body)
	var out []string
	seen := map[string]bool{}
	for _, m := range mdLink.FindAllStringSubmatch(text, -1) {
		target := strings.TrimSpace(m[1])
		if target == "" || strings.HasPrefix(target, "#") ||
			strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") ||
			strings.HasPrefix(target, "/") {
			continue
		}
		if !seen[target] {
			seen[target] = true
			out = append(out, target)
		}
	}
	return out
}

// ExternalURLs returns deduplicated http(s) URLs from link targets and bare
// text. Trailing punctuation that is almost never part of a URL is trimmed.
func ExternalURLs(body string) []string {
	text := stripFences(body)
	var out []string
	seen := map[string]bool{}
	add := func(raw string) {
		u := strings.TrimRight(raw, ".,;:!?'\"")
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	for _, m := range mdLink.FindAllStringSubmatch(text, -1) {
		if strings.HasPrefix(m[1], "http://") || strings.HasPrefix(m[1], "https://") {
			add(m[1])
		}
	}
	for _, u := range bareURL.FindAllString(text, -1) {
		add(u)
	}
	return out
}

// InternalCandidates returns the repository-relative paths a link target
// could mean, in preference order: relative to the linking document's
// directory first, then repository-root relative. Fragile/empty inputs
// return nil. Fragments and query strings are stripped — link targets name
// files, not sections.
func InternalCandidates(docPath, target string) []string {
	t := strings.SplitN(strings.SplitN(target, "#", 2)[0], "?", 2)[0]
	t = strings.TrimSpace(t)
	if t == "" {
		return nil
	}
	out := []string{}
	if dir := path.Dir(docPath); dir != "." {
		out = append(out, path.Join(dir, t))
	}
	out = append(out, path.Clean(t))
	return out
}

// IsDocumentTarget reports whether a link target names a markdown document —
// those resolve against the scanned document set; other relative targets are
// checked for plain file existence instead.
func IsDocumentTarget(target string) bool {
	ext := strings.ToLower(path.Ext(strings.SplitN(target, "#", 2)[0]))
	return ext == ".md" || ext == ".markdown"
}
