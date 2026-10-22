package analyzer

import (
	"fmt"
	"net/url"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/scanner"
)

type entityAlias struct {
	path          string
	sourcePaths   []string
	label         string
	rawPhrase     string
	caseSensitive bool
}

type entityMention struct {
	path          string
	sourcePaths   []string
	term          string
	line          int
	caseSensitive bool
}

type pendingEntityAlias struct {
	sourcePath string
	targetPath string
	label      string
}

type entityIndex struct {
	byPhrase map[string][]entityAlias
	pending  []pendingEntityAlias
}

var genericEntityTerms = map[string]bool{
	"database": true, "documentation": true, "framework": true,
	"frameworks": true, "guide": true, "index": true, "introduction": true,
	"language": true, "languages": true, "landscape": true, "library": true,
	"overview": true, "pattern": true, "patterns": true, "readme": true,
	"setup": true, "system": true, "test": true, "testing": true,
	"tool": true, "tools": true,
}

func newEntityIndex() *entityIndex {
	return &entityIndex{byPhrase: make(map[string][]entityAlias)}
}

func (i *entityIndex) addDocument(path string, fields map[string]any, content string) {
	parts := strings.Split(filepath.ToSlash(path), "/")
	if len(parts) > 1 && strings.EqualFold(strings.TrimSuffix(parts[len(parts)-1], filepath.Ext(parts[len(parts)-1])), "readme") {
		i.addAlias(parts[len(parts)-2], path)
	} else if len(parts) > 0 {
		stem := strings.TrimSuffix(parts[len(parts)-1], filepath.Ext(parts[len(parts)-1]))
		i.addAlias(stem, path)
		i.addDistinctWords(stem, path)
	}
	if title, ok := fields["title"].(string); ok {
		i.addAlias(title, path)
	}
	if heading := markdownH1(content); heading != "" {
		i.addAlias(heading, path)
	}
}

func (i *entityIndex) addExplicitMarkdownLinks(sourcePath, content string) {
	_, body, err := frontmatter.Parse(content)
	if err != nil {
		return
	}
	for _, line := range proseLines(body) {
		for _, link := range markdownLinks(line.text) {
			target, ok := resolveLocalMarkdownTarget(sourcePath, link.target)
			if !ok {
				continue
			}
			i.pending = append(i.pending, pendingEntityAlias{
				sourcePath: sourcePath, targetPath: target, label: link.label,
			})
		}
	}
}

func (i *entityIndex) resolveExplicitMarkdownLinks(visible map[string]bool) {
	for _, pending := range i.pending {
		if visible[pending.targetPath] {
			i.addAliasFrom(pending.label, pending.targetPath, pending.sourcePath)
		}
	}
	i.pending = nil
}

type markdownLine struct {
	text   string
	number int
}

type markdownLink struct {
	label  string
	target string
}

func proseLines(body string) []markdownLine {
	var lines []markdownLine
	var fence rune
	fenceWidth := 0
	for lineNumber, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
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
		if isIndentedCode(line) {
			continue
		}
		lines = append(lines, markdownLine{text: line, number: lineNumber + 1})
	}
	return lines
}

func isIndentedCode(line string) bool {
	spaces := 0
	for _, r := range line {
		switch r {
		case ' ':
			spaces++
		case '\t':
			return true
		default:
			return spaces >= 4
		}
	}
	return false
}

func markdownLinks(line string) []markdownLink {
	line = maskInlineCode(line)
	var links []markdownLink
	for start := 0; start < len(line); {
		open := strings.IndexByte(line[start:], '[')
		if open < 0 {
			break
		}
		open += start
		if open > 0 && line[open-1] == '!' {
			start = open + 1
			continue
		}
		closeLabel := strings.Index(line[open+1:], "](")
		if closeLabel < 0 {
			break
		}
		closeLabel += open + 1
		targetStart := closeLabel + 2
		closeTarget := strings.IndexByte(line[targetStart:], ')')
		if closeTarget < 0 {
			break
		}
		closeTarget += targetStart
		label := strings.TrimSpace(line[open+1 : closeLabel])
		rawTarget := strings.TrimSpace(line[targetStart:closeTarget])
		if first, _, ok := strings.Cut(rawTarget, " "); ok {
			rawTarget = first
		}
		rawTarget = strings.Trim(rawTarget, "<>")
		if label != "" && rawTarget != "" {
			links = append(links, markdownLink{label: label, target: rawTarget})
		}
		start = closeTarget + 1
	}
	return links
}

func maskInlineCode(line string) string {
	masked := []byte(line)
	for i := 0; i < len(line); {
		if line[i] != '`' {
			i++
			continue
		}
		runEnd := i
		for runEnd < len(line) && line[runEnd] == '`' {
			runEnd++
		}
		marker := line[i:runEnd]
		search := runEnd
		closingStart := -1
		for search < len(line) {
			next := strings.IndexByte(line[search:], '`')
			if next < 0 {
				break
			}
			next += search
			end := next
			for end < len(line) && line[end] == '`' {
				end++
			}
			if end-next == len(marker) {
				closingStart = next
				break
			}
			search = end
		}
		if closingStart < 0 {
			i = runEnd
			continue
		}
		for n := i; n < closingStart+len(marker); n++ {
			if masked[n] != '\n' && masked[n] != '\r' {
				masked[n] = ' '
			}
		}
		i = closingStart + len(marker)
	}
	return string(masked)
}

func resolveLocalMarkdownTarget(sourcePath, rawTarget string) (string, bool) {
	targetURL, err := url.Parse(rawTarget)
	if err != nil || targetURL.IsAbs() || targetURL.Host != "" || targetURL.RawQuery != "" {
		return "", false
	}
	target := targetURL.Path
	ext := strings.ToLower(pathpkg.Ext(target))
	if target == "" || (ext != ".md" && ext != ".markdown") || pathpkg.IsAbs(target) {
		return "", false
	}
	resolved := pathpkg.Clean(pathpkg.Join(pathpkg.Dir(sourcePath), target))
	if resolved == "." || resolved == ".." || strings.HasPrefix(resolved, "../") {
		return "", false
	}
	return resolved, true
}

func entitySlug(text string) string {
	words := entityWords(text)
	out := make([]string, len(words))
	for n, word := range words {
		out[n] = word.normalized
	}
	return strings.Join(out, "-")
}

func (i *entityIndex) addDistinctWords(text, path string) {
	i.addDistinctWordsFrom(text, path, path)
}

func (i *entityIndex) addDistinctWordsFrom(text, path, sourcePath string) {
	for _, word := range entityWords(text) {
		if len([]rune(word.normalized)) >= 5 && !genericEntityTerms[word.normalized] {
			i.addAliasFrom(word.raw, path, sourcePath)
		}
	}
}

// addFrameworkCatalog consumes only the curated language/framework inventory,
// not arbitrary prose from the overview document.
func (i *entityIndex) addFrameworkCatalog(content string, visible map[string]bool) {
	const catalogPath = "frameworks/README.md"
	if !visible[catalogPath] {
		return
	}
	inCatalog := false
	var language string
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "## ") {
			inCatalog = line == "## Languages and their dominant frameworks"
			language = ""
			continue
		}
		if !inCatalog {
			continue
		}
		if strings.HasPrefix(line, "### ") {
			language = catalogName(strings.TrimPrefix(line, "### "))
			target := i.resolveCatalogLanguage(language, visible)
			if target == "" {
				target = catalogPath
			}
			i.addAliasFrom(language, target, catalogPath)
			i.addDistinctWordsFrom(language, target, catalogPath)
			continue
		}
		if language == "" || !strings.HasPrefix(line, "- ") {
			continue
		}
		name := catalogName(strings.TrimPrefix(line, "- "))
		target := i.resolveCatalogFramework(name, language, visible)
		if target == "" {
			target = catalogPath
		}
		i.addAliasFrom(name, target, catalogPath)
		i.addDistinctWordsFrom(name, target, catalogPath)
		if former := parentheticalAlias(name); former != "" {
			i.addAliasFrom(former, target, catalogPath)
		}
	}
}

func (i *entityIndex) resolveCatalogLanguage(name string, visible map[string]bool) string {
	candidate := filepath.ToSlash(filepath.Join("languages", entitySlug(name)+".md"))
	if visible[candidate] {
		return candidate
	}
	return i.resolveExistingAlias(name, "languages/", visible)
}

func (i *entityIndex) resolveCatalogFramework(name, language string, visible map[string]bool) string {
	slug := entitySlug(name)
	for _, candidate := range []string{
		filepath.ToSlash(filepath.Join("frameworks", slug, "README.md")),
		filepath.ToSlash(filepath.Join("frameworks", slug+".md")),
		filepath.ToSlash(filepath.Join("tools", slug+".md")),
		filepath.ToSlash(filepath.Join("libraries", entitySlug(language), slug+".md")),
	} {
		if visible[candidate] {
			return candidate
		}
	}
	for _, root := range []string{"frameworks/", "tools/", "libraries/"} {
		if target := i.resolveExistingAlias(name, root, visible); target != "" {
			return target
		}
	}
	words := entityWords(name)
	for n := len(words) - 1; n >= 0; n-- {
		for _, root := range []string{"frameworks/", "tools/", "libraries/"} {
			if target := i.resolveExistingAlias(words[n].normalized, root, visible); target != "" {
				return target
			}
		}
	}
	return ""
}

func (i *entityIndex) resolveExistingAlias(name, root string, visible map[string]bool) string {
	for _, alias := range i.byPhrase[entityPhrase(name)] {
		if visible[alias.path] && strings.HasPrefix(alias.path, root) {
			return alias.path
		}
	}
	return ""
}

func catalogName(text string) string {
	if before, _, ok := strings.Cut(text, " — "); ok {
		return strings.TrimSpace(before)
	}
	return strings.TrimSpace(text)
}

func parentheticalAlias(text string) string {
	open := strings.Index(text, "(formerly ")
	if open < 0 || !strings.HasSuffix(strings.TrimSpace(text), ")") {
		return ""
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text[open+len("(formerly "):]), ")"))
}

func (i *entityIndex) addAlias(text, path string) {
	i.addAliasFrom(text, path, path)
}

func (i *entityIndex) addAliasFrom(text, path, sourcePath string) {
	words := entityWords(text)
	if len(words) == 0 || len(words) > 5 {
		return
	}
	if len(words) == 1 && strings.HasPrefix(path, "languages/") {
		switch strings.ToLower(words[0].normalized) {
		case "go":
			text = "Go"
			words = entityWords(text)
		case "r":
			text = "R"
			words = entityWords(text)
		}
	}
	normalized := make([]string, len(words))
	raw := make([]string, len(words))
	for n, word := range words {
		normalized[n], raw[n] = word.normalized, word.raw
	}
	if len(words) == 1 {
		if genericEntityTerms[normalized[0]] {
			return
		}
		short := len([]rune(normalized[0])) < 3
		curatedShortLanguage := strings.HasPrefix(path, "languages/") &&
			(strings.EqualFold(text, "go") || strings.EqualFold(text, "r") ||
				strings.EqualFold(text, "c#") || strings.EqualFold(text, "f#"))
		if short && !curatedShortLanguage {
			return
		}
	}
	caseSensitive := len(words) == 1 && len([]rune(normalized[0])) < 3 &&
		!strings.ContainsAny(normalized[0], "+#.")
	label := strings.TrimSpace(text)
	rawPhrase := strings.Join(raw, " ")
	key := strings.Join(normalized, " ")
	for n, existing := range i.byPhrase[key] {
		if existing.path == path {
			if !slices.Contains(existing.sourcePaths, sourcePath) {
				existing.sourcePaths = append(existing.sourcePaths, sourcePath)
				slices.Sort(existing.sourcePaths)
				i.byPhrase[key][n] = existing
			}
			return
		}
	}
	i.byPhrase[key] = append(i.byPhrase[key], entityAlias{
		path: path, sourcePaths: []string{sourcePath}, label: label, rawPhrase: rawPhrase, caseSensitive: caseSensitive,
	})
}

func (i *entityIndex) matches(currentPath, content string) []entityMention {
	_, body, err := frontmatter.Parse(content)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	perTarget := map[string]int{}
	var matches []entityMention
	for _, sourceLine := range proseLines(body) {
		line := sourceLine.text
		words := entityWords(line)
		for start := range words {
			keyParts := make([]string, 0, 5)
			rawParts := make([]string, 0, 5)
			end := min(len(words), start+5)
			for stop := start; stop < end; stop++ {
				keyParts = append(keyParts, words[stop].normalized)
				rawParts = append(rawParts, words[stop].raw)
				key := strings.Join(keyParts, " ")
				rawPhrase := strings.Join(rawParts, " ")
				for _, alias := range i.byPhrase[key] {
					if alias.path == currentPath || (alias.caseSensitive && alias.rawPhrase != rawPhrase) {
						continue
					}
					id := alias.path + "\x00" + alias.label
					if seen[id] || perTarget[alias.path] >= 3 {
						continue
					}
					seen[id] = true
					perTarget[alias.path]++
					matches = append(matches, entityMention{
						path: alias.path, sourcePaths: slices.Clone(alias.sourcePaths),
						term: alias.label, line: sourceLine.number, caseSensitive: alias.caseSensitive,
					})
				}
			}
		}
	}
	slices.SortFunc(matches, func(a, b entityMention) int {
		if c := strings.Compare(a.path, b.path); c != 0 {
			return c
		}
		return strings.Compare(a.term, b.term)
	})
	return matches
}

func entityEvidenceContext(c config.Config, ranked []candidate) string {
	var lines []string
	for _, target := range ranked {
		if len(lines) == 2 {
			break
		}
		if len(target.mentions) == 0 {
			continue
		}
		content, err := scanner.ReadDocument(c, target.doc.Path)
		if err != nil {
			continue
		}
		fields, body, err := frontmatter.Parse(content)
		if err != nil || frontmatter.NoLLMContext(fields) {
			continue
		}
		excerpts := entityExcerpts(body, target.mentions)
		if len(excerpts) == 0 {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", target.doc.Path, strings.Join(excerpts, " | ")))
	}
	if len(lines) == 0 {
		return ""
	}
	return "Exact-name evidence from candidate notes (hints, not proof of a relationship):\n" +
		strings.Join(lines, "\n") + "\n"
}

func entityExcerpts(body string, mentions []entityMention) []string {
	lines := proseLines(body)
	for lineIndex, line := range lines {
		for _, mention := range mentions {
			if !containsEntityPhrase(line.text, mention.term, mention.caseSensitive) {
				continue
			}
			trimmed := strings.TrimSpace(line.text)
			if strings.HasPrefix(trimmed, "#") {
				if excerpt, start, end := proseParagraphAfter(lines, lineIndex+1); excerpt != "" {
					return []string{fmt.Sprintf("body lines %d-%d: %q", start, end, truncateEntityText(excerpt, 260))}
				}
				continue
			}
			return []string{fmt.Sprintf("body line %d: %q", line.number, truncateEntityText(trimmed, 260))}
		}
	}
	return nil
}

func proseParagraphAfter(lines []markdownLine, start int) (string, int, int) {
	for i := start; i < len(lines); i++ {
		text := strings.TrimSpace(lines[i].text)
		if text == "" || strings.HasPrefix(text, "#") || isMarkdownListItem(text) {
			continue
		}
		begin, end := i, i
		var parts []string
		for j := i; j < len(lines); j++ {
			part := strings.TrimSpace(lines[j].text)
			if part == "" {
				break
			}
			if j > i && (strings.HasPrefix(part, "#") || isMarkdownListItem(part)) {
				break
			}
			parts = append(parts, part)
			end = j
		}
		return strings.Join(parts, " "), lines[begin].number, lines[end].number
	}
	return "", 0, 0
}

func isMarkdownListItem(text string) bool {
	return strings.HasPrefix(text, "- ") || strings.HasPrefix(text, "* ") ||
		strings.HasPrefix(text, "+ ") || (len(text) > 2 && text[0] >= '0' && text[0] <= '9' && strings.Contains(text[:3], ". "))
}

func containsEntityPhrase(text, phrase string, caseSensitive bool) bool {
	have, want := entityWords(text), entityWords(phrase)
	if len(want) == 0 || len(want) > len(have) {
		return false
	}
	for start := 0; start+len(want) <= len(have); start++ {
		matched := true
		for offset, word := range want {
			if have[start+offset].normalized != word.normalized {
				matched = false
				break
			}
			if caseSensitive && have[start+offset].raw != word.raw {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func truncateEntityText(text string, maxRunes int) string {
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes]) + "..."
}

type entityWord struct {
	raw        string
	normalized string
}

func entityWords(text string) []entityWord {
	var out []entityWord
	var raw strings.Builder
	text = unescapeEntityPunctuation(text)
	runes := []rune(text)
	flush := func() {
		if raw.Len() == 0 {
			return
		}
		value := raw.String()
		out = append(out, entityWord{raw: value, normalized: strings.ToLower(value)})
		raw.Reset()
	}
	for i, r := range runes {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			raw.WriteRune(r)
		case isEntityPunctuation(r, raw.String(), runes[i+1:]):
			raw.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out
}

func unescapeEntityPunctuation(text string) string {
	return strings.NewReplacer(`\#`, "#", `\+`, "+", `\.`, ".", `\-`, "-").Replace(text)
}

func isEntityPunctuation(r rune, current string, remaining []rune) bool {
	word := strings.ToLower(current)
	switch r {
	case '+':
		return word == "c" || word == "c+"
	case '#':
		return word == "c" || word == "f"
	case '.':
		return len(remaining) > 0 && (unicode.IsLetter(remaining[0]) || unicode.IsDigit(remaining[0]))
	default:
		return false
	}
}

func entityPhrase(text string) string {
	words := entityWords(text)
	parts := make([]string, len(words))
	for n, word := range words {
		parts[n] = word.normalized
	}
	return strings.Join(parts, " ")
}
