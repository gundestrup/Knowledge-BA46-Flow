// Package frontmatter parses and validates the optional YAML frontmatter
// block at the top of knowledge documents. The schema mirrors the whitelist
// used by the knowledge-base-mcp-server project so curated files stay
// compatible with its tag/lifecycle tooling.
package frontmatter

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"knowledge-ba46-flow/internal/paths"
)

// maxBytes caps how far from the top of the file the closing fence may sit —
// the same 8 KiB window the MCP server uses before treating the file as
// frontmatter-free.
const maxBytes = 8192

// ManagedKey is the tool-owned top-level frontmatter key. KBM-flow alone
// writes it (link-check results, check timestamps, similar operational
// metadata); humans and models must not author or edit it. The content hash
// ignores the block so recording a check never requeues analysis.
const ManagedKey = "kbm"

var (
	openFence  = regexp.MustCompile(`^---\r?\n`)
	closeFence = regexp.MustCompile(`(^|\n)---(\r?\n|$)`)
)

// Parse splits the YAML block from the body. A document without a leading
// "---" fence returns nil fields and the full content. A document that opens
// a fence but does not close it within maxBytes, or whose block is not a YAML
// mapping, returns an error so callers can report it as a curation issue.
func Parse(content string) (map[string]any, string, error) {
	content = strings.TrimPrefix(content, "\uFEFF")
	open := openFence.FindStringIndex(content)
	if open == nil {
		return nil, content, nil
	}
	limit := len(content)
	if limit > maxBytes {
		limit = maxBytes
	}
	window := content[open[1]:limit]
	close := closeFence.FindStringIndex(window)
	if close == nil {
		return nil, content, fmt.Errorf("frontmatter opening fence has no closing fence within %d bytes", maxBytes)
	}
	// close[0] is the YAML end in both cases: when the match leads with '\n'
	// that byte belongs to the fence, and an empty-lead match sits at 0 anyway.
	raw := window[:close[0]]
	body := content[open[1]+close[1]:]
	var fields map[string]any
	if err := yaml.Unmarshal([]byte(raw), &fields); err != nil {
		return nil, body, fmt.Errorf("malformed frontmatter YAML: %w", err)
	}
	if fields == nil {
		fields = map[string]any{} // empty block: ---\n---
	}
	return fields, body, nil
}

// Canonical returns the content used for change detection: the input with the
// tool-owned `kbm` top-level frontmatter mapping excised byte-for-byte, and
// the top-level `document_id` line excised. Identity is not content: assigning
// or repairing a UUID never changes the hash, so adding IDs to an existing
// corpus never invalidates a pending proposal and never requeues analysis.
// The stored frontmatter_json carries the real ID for identity checks, so an
// unexpected ID change is still visible — it just is not a content edit.
// Because the rest of the document is preserved verbatim, a document without
// the block hashes exactly as its raw bytes did — adding or updating `kbm`
// never changes the hash, so operational bookkeeping never requeues analysis.
// Malformed or unclosed frontmatter returns the input unchanged.
func Canonical(content string) string {
	fm, body := CanonicalParts(content)
	return fm + body
}

// CanonicalParts is Canonical split at the fence boundary: fm is the
// frontmatter section with the `kbm` block excised (empty when the document
// has no surviving frontmatter), body is the post-frontmatter text. The
// halves always concatenate back to Canonical's output, so sha256(fm),
// sha256(body) and sha256(fm+body) can tell frontmatter edits apart from
// content edits — a frontmatter-only change requeues metadata work without
// pretending the body changed.
func CanonicalParts(content string) (fm, body string) {
	trimmed := strings.TrimPrefix(content, "\uFEFF")
	bom := content[:len(content)-len(trimmed)]
	open := openFence.FindStringIndex(trimmed)
	if open == nil {
		return "", content
	}
	limit := len(trimmed)
	if limit > maxBytes {
		limit = maxBytes
	}
	window := trimmed[open[1]:limit]
	close := closeFence.FindStringIndex(window)
	if close == nil {
		return "", content
	}
	raw := window[:close[0]]
	body = trimmed[open[1]+close[1]:]
	lines := strings.SplitAfter(raw, "\n")
	topIndent := topLevelIndent(lines)
	inKBM := false
	afterKBM := false
	kept := strings.Builder{}
	found := false
	for _, line := range lines {
		trim := strings.TrimRight(line, "\r\n")
		text := strings.TrimSpace(trim)
		structural := isStructuralLine(text, indentOfLine(trim), topIndent)
		switch {
		case structural && strings.HasPrefix(text, ManagedKey+":"):
			// Top-level key: the managed block starts here.
			inKBM, found = true, true
		case structural && strings.HasPrefix(text, "document_id:"):
			// Top-level identity line: excluded from the content hash so
			// assigning a UUID does not look like a document edit. It also
			// ends an open kbm block but deliberately leaves afterKBM alone —
			// the trailing-newline trim must still match a file whose
			// frontmatter ends right after kbm.
			inKBM = false
			found = true
		case inKBM && structural:
			// A top-level, non-comment line ends the block.
			inKBM = false
			afterKBM = true
			kept.WriteString(line)
		case !inKBM:
			kept.WriteString(line)
		}
	}
	if !found {
		// An empty block carries no frontmatter content — Parse reports it
		// as zero fields, so the canonical form treats it as no frontmatter
		// at all (a managed write into it must stay hash-neutral).
		if strings.TrimSpace(raw) == "" {
			return "", body
		}
		return bom + trimmed[:open[1]+close[1]], body
	}
	keptText := kept.String()
	if !afterKBM {
		// The block ran to the end of the frontmatter: drop the line ending
		// left behind so the remainder matches a file that never had it —
		// the raw frontmatter text never carries a trailing newline.
		keptText = strings.TrimSuffix(keptText, "\n")
		keptText = strings.TrimSuffix(keptText, "\r")
	}
	// If kbm was the only frontmatter content the whole block collapses to
	// the body, matching a document that never had a frontmatter fence.
	if strings.TrimSpace(keptText) == "" {
		return "", body
	}
	return trimmed[:open[1]] + keptText + trimmed[open[1]+close[0]:open[1]+close[1]], body
}

// WriteManaged applies SetManaged to a repository file in place: read,
// update kbm.<key>, atomically rewrite — all confined to root. Callers then
// refresh the stored parse (database.RefreshFrontmatter). This is the one
// permitted file write outside apply: the block is operational metadata the
// canonical content hash ignores.
func WriteManaged(root *os.Root, rel, key string, value any) error {
	local, err := paths.CleanRelative(rel)
	if err != nil {
		return err
	}
	raw, err := root.ReadFile(local)
	if err != nil {
		return err
	}
	updated, err := SetManaged(string(raw), key, value)
	if err != nil {
		return err
	}
	if updated == string(raw) {
		return nil
	}
	return paths.AtomicWrite(root, local, []byte(updated))
}

// SetManaged writes one entry inside the tool-owned `kbm` frontmatter block —
// kbm.<key> = value — preserving sibling entries. A nil value removes the
// key. The update is surgical: only the `kbm` mapping is re-emitted (in
// normalized YAML); every other frontmatter byte and the body pass through
// verbatim. That keeps the canonical content hash byte-stable across managed
// writes even when a human's frontmatter formatting differs from Render's —
// a managed write must never look like a content edit.
func SetManaged(content, key string, value any) (string, error) {
	fields, _, err := Parse(content)
	if err != nil {
		return "", err // malformed YAML fails closed — no half-written block
	}
	kbm, _ := fields[ManagedKey].(map[string]any)
	if kbm == nil {
		kbm = map[string]any{}
	}
	if value == nil {
		delete(kbm, key)
	} else {
		kbm[key] = value
	}
	if len(kbm) == 0 {
		kbm = nil
	}
	out, err := setManagedBlock(content, kbm)
	if err != nil {
		return "", err
	}
	return out, verifyManagedWrite(content, key, value, out)
}

// PreserveManaged replaces a proposal's operational metadata with the live
// file's block. A proposal can neither restore old verdicts nor author kbm.
func PreserveManaged(original, proposed string) (string, error) {
	before, _, err := Parse(original)
	if err != nil {
		return "", err
	}
	after, _, err := Parse(proposed)
	if err != nil {
		return "", err
	}
	value, present := before[ManagedKey]
	if reflect.DeepEqual(value, after[ManagedKey]) && present == hasKey(after, ManagedKey) {
		return proposed, nil
	}
	var managed map[string]any
	if present {
		var ok bool
		managed, ok = value.(map[string]any)
		if !ok {
			return "", fmt.Errorf("live kbm must be a mapping")
		}
	}
	out, err := setManagedBlock(proposed, managed)
	if err != nil {
		return "", err
	}
	fields, _, err := Parse(out)
	if err != nil || !reflect.DeepEqual(fields[ManagedKey], value) ||
		hasKey(fields, ManagedKey) != present || Canonical(out) != Canonical(proposed) {
		return "", fmt.Errorf("cannot preserve live kbm without changing proposed content")
	}
	return out, nil
}

func hasKey(fields map[string]any, key string) bool {
	_, ok := fields[key]
	return ok
}

func setManagedBlock(content string, kbm map[string]any) (string, error) {
	if kbm == nil {
		return Canonical(content), nil
	}
	block, err := yaml.Marshal(map[string]any{ManagedKey: kbm})
	if err != nil {
		return "", fmt.Errorf("marshal kbm block: %w", err)
	}

	trimmed := strings.TrimPrefix(content, "\uFEFF")
	bom := content[:len(content)-len(trimmed)]
	open := openFence.FindStringIndex(trimmed)
	if open == nil {
		if len(block) == 0 {
			return content, nil
		}
		return bom + "---\n" + string(block) + "---\n" + trimmed, nil
	}
	limit := len(trimmed)
	if limit > maxBytes {
		limit = maxBytes
	}
	window := trimmed[open[1]:limit]
	close := closeFence.FindStringIndex(window)
	if close == nil {
		return "", fmt.Errorf("unclosed frontmatter")
	}
	raw := window[:close[0]]

	// Excise the existing kbm mapping, byte-for-byte, same rule as Canonical.
	// Top-level keys may sit at a consistent non-zero indent (e.g. " kbm:");
	// detect the mapping indent from the first key-looking line instead of
	// assuming column 0, or the old block is kept and a duplicate appended.
	lines := strings.SplitAfter(raw, "\n")
	topIndent := topLevelIndent(lines)
	inKBM := false
	kept := strings.Builder{}
	for _, line := range lines {
		trim := strings.TrimRight(line, "\r\n")
		text := strings.TrimSpace(trim)
		structural := isStructuralLine(text, indentOfLine(trim), topIndent)
		switch {
		case structural && strings.HasPrefix(text, ManagedKey+":"):
			inKBM = true
		case structural:
			inKBM = false
			kept.WriteString(line)
		case !inKBM:
			kept.WriteString(line)
		}
	}
	newFM := kept.String()
	if len(block) > 0 {
		if newFM != "" && !strings.HasSuffix(newFM, "\n") {
			newFM += "\n"
		}
		if topIndent > 0 {
			// The emitted block must sit at the mapping's own indent — a
			// column-0 key would dedent below the existing mapping.
			pad := strings.Repeat(" ", topIndent)
			indented := strings.Builder{}
			for _, bl := range strings.SplitAfter(string(block), "\n") {
				if strings.TrimSpace(bl) != "" {
					indented.WriteString(pad)
				}
				indented.WriteString(bl)
			}
			block = []byte(indented.String())
		}
		newFM += string(block)
	}
	var out string
	if strings.TrimSpace(newFM) == "" {
		// kbm was the only frontmatter content (or nothing remains):
		// drop the now-empty fences, matching Canonical's collapse.
		out = bom + trimmed[open[1]+close[1]:]
	} else {
		if !strings.HasSuffix(newFM, "\n") {
			newFM += "\n"
		}
		out = bom + trimmed[:open[1]] + newFM + trimmed[open[1]+close[0]:open[1]+close[1]] + trimmed[open[1]+close[1]:]
	}
	return out, nil
}

// indentOfLine counts leading whitespace (spaces and tabs) of a raw line.
func indentOfLine(line string) int {
	n := 0
	for n < len(line) && (line[n] == ' ' || line[n] == '\t') {
		n++
	}
	return n
}

// topLevelIndent detects the indentation of a mapping's top-level keys —
// usually 0, but some files indent the whole frontmatter mapping uniformly
// (" key:"). Falls back to 0 when no key-looking line exists.
func topLevelIndent(lines []string) int {
	for _, line := range lines {
		if ind := indentOfKeyLine(line); ind >= 0 {
			return ind
		}
	}
	return 0
}

// isStructuralLine reports whether a frontmatter line sits at (or dedents
// below) the mapping's top level — i.e. a top-level key or stranger that
// ends an open block. Blanks, comments and list items are part of the
// enclosing block: YAML allows sequences at the parent's indent.
func isStructuralLine(text string, indent, topIndent int) bool {
	return text != "" && text[0] != '#' && text[0] != '-' && indent <= topIndent
}

// indentOfKeyLine reports the indentation of a YAML key line, or -1 for
// blanks, comments and list items. Used to locate a mapping's top-level
// indent when keys are uniformly indented (e.g. " key:" frontmatter).
func indentOfKeyLine(line string) int {
	trim := strings.TrimRight(line, "\r\n")
	text := strings.TrimSpace(trim)
	if text == "" || text[0] == '#' || text[0] == '-' || !strings.Contains(text, ":") {
		return -1
	}
	return indentOfLine(trim)
}

// verifyManagedWrite is the splice's self-check: it assumes a column-0
// top-level mapping, and pathological frontmatter (indented mapping, complex
// keys) can leave the emitted block outside the mapping where the parser
// silently drops it — fail closed instead of losing the write or moving the
// hash.
func verifyManagedWrite(content, key string, value any, out string) error {
	verify, _, err := Parse(out)
	if err != nil {
		return fmt.Errorf("managed write produced unparseable frontmatter: %w", err)
	}
	written, _ := verify[ManagedKey].(map[string]any)
	if _, ok := written[key]; ok != (value != nil) {
		return fmt.Errorf("frontmatter shape cannot host a kbm block safely")
	}
	if Canonical(out) != Canonical(content) {
		return fmt.Errorf("managed write would change the canonical content hash")
	}
	return nil
}

// stringKeys are lifted verbatim by the MCP server when they hold strings.
var stringKeys = map[string]bool{
	"arxiv_id": true, "title": true, "authors": true, "published": true,
	"ingested_at": true, "judge_method": true, "metrics_used": true,
	"bias_handling": true, "status": true, "review_status": true,
	"promote_model": true, "tier": true, "last_verified_at": true,
}

// dateKeys must hold a parseable ISO date or datetime when present.
var dateKeys = map[string]bool{
	"published": true, "ingested_at": true, "last_verified_at": true,
}

var (
	// tier is the closed set `kb promote` writes.
	tierValues = map[string]bool{"working": true, "validated": true, "wisdom": true}
	// deprecatedValues is the status family `kb superseded` flags as stale.
	deprecatedValues = map[string]bool{
		"archived": true, "deprecated": true, "dormant": true, "obsolete": true,
		"retired": true, "stale": true, "superseded": true,
	}
	// statusValues additionally allows the common "live" states.
	statusValues = map[string]bool{
		"draft": true, "active": true, "approved": true, "published": true,
	}
	resourceReadValues = map[string]bool{"allow": true, "local_only": true, "deny": true}
)

func init() {
	for v := range deprecatedValues {
		statusValues[v] = true
	}
}

// Validate returns human-readable issues with a parsed frontmatter mapping.
// Checks are deliberately local: key/type conformance with the MCP whitelist,
// closed enums, dates, numeric ranges and lexical link paths — no filesystem
// access (link *existence* is checked by the analyzer against the document set).
// `related` is an extra key this tool curates; the MCP server ignores it.
func Validate(fields map[string]any) []string {
	issues := []string{} // non-nil so JSON encodes [] rather than null
	if _, err := DocumentID(fields); err != nil {
		issues = append(issues, err.Error())
	}
	for key, value := range fields {
		switch {
		case key == "tags":
			issues = append(issues, checkStringList(key, value, true)...)
		case key == "related" || key == "contradicted_by":
			for _, item := range listItems(key, value, &issues) {
				if _, err := paths.CleanRelative(strings.TrimSpace(item)); err != nil {
					issues = append(issues, fmt.Sprintf("%s entry %q is not a valid repository-relative path", key, item))
				}
			}
		case key == "confidence" || key == "relevance_score":
			if _, ok := number(value); !ok {
				issues = append(issues, fmt.Sprintf("%s must be a number", key))
			}
		case key == "manual_edits":
			if _, ok := boolean(value); !ok {
				issues = append(issues, "manual_edits must be true or false")
			}
		case key == "kb_policy":
			issues = append(issues, checkPolicy(value)...)
		case key == ManagedKey:
			// Tool-owned operational metadata (link-check results and such);
			// it must be a mapping, contents are KBM-flow's responsibility.
			if _, ok := value.(map[string]any); !ok {
				issues = append(issues, ManagedKey+" must be a mapping")
			}
		case stringKeys[key]:
			s, ok := value.(string)
			if !ok {
				// yaml.v3 resolves ISO dates to time.Time — a typed date is
				// valid for the date keys even though the MCP server sees a string.
				if _, isTime := value.(time.Time); !isTime || !dateKeys[key] {
					issues = append(issues, fmt.Sprintf("%s must be a string", key))
				}
				continue
			}
			issues = append(issues, checkValue(key, strings.TrimSpace(s))...)
		default:
			if _, ok := value.(string); !ok {
				issues = append(issues, fmt.Sprintf("unknown key %q with a non-string value is dropped by the knowledge-base-mcp-server", key))
			}
		}
	}
	return issues
}

// checkValue applies the closed enums and date checks for whitelisted string keys.
func checkValue(key, s string) []string {
	switch key {
	case "tier":
		if !tierValues[s] {
			return []string{fmt.Sprintf("tier %q is not one of working|validated|wisdom", s)}
		}
	case "review_status":
		if s != "approved" && s != "needs-review" && !deprecatedValues[strings.ToLower(s)] {
			return []string{fmt.Sprintf("review_status %q is not one of approved|needs-review", s)}
		}
	case "status":
		if !statusValues[strings.ToLower(s)] {
			return []string{fmt.Sprintf("status %q is not a recognised lifecycle value", s)}
		}
	}
	if dateKeys[key] && !isDate(s) {
		return []string{fmt.Sprintf("%s %q is not an ISO date or datetime", key, s)}
	}
	return nil
}

// isDate accepts the shapes the MCP tooling parses: YYYY-MM-DD and RFC 3339.
func isDate(s string) bool {
	if _, err := time.Parse("2006-01-02", s); err == nil {
		return true
	}
	_, err := time.Parse(time.RFC3339, s)
	return err == nil
}

// checkPolicy validates the kb_policy map: no_llm_context (bool),
// resource_read (allow|local_only|deny), sensitivity (string).
func checkPolicy(value any) []string {
	m, ok := value.(map[string]any)
	if !ok {
		return []string{"kb_policy must be a mapping"}
	}
	var issues []string
	for k, v := range m {
		switch k {
		case "no_llm_context":
			if _, ok := boolean(v); !ok {
				issues = append(issues, "kb_policy.no_llm_context must be true or false")
			}
		case "resource_read":
			s, ok := v.(string)
			if !ok || !resourceReadValues[strings.ToLower(strings.ReplaceAll(s, "-", "_"))] {
				issues = append(issues, "kb_policy.resource_read must be allow|local_only|deny")
			}
		case "sensitivity":
			if _, ok := v.(string); !ok {
				issues = append(issues, "kb_policy.sensitivity must be a string")
			}
		default:
			issues = append(issues, fmt.Sprintf("kb_policy.%s is not a recognised policy key", k))
		}
	}
	return issues
}

// NoLLMContext reports whether the local policy excludes a document from LLM
// prompts. An invalid kb_policy fails closed: callers must not send document
// content when the policy cannot be interpreted safely.
func NoLLMContext(fields map[string]any) bool {
	raw, exists := fields["kb_policy"]
	if !exists {
		return false
	}
	policy, ok := raw.(map[string]any)
	if !ok || len(checkPolicy(policy)) != 0 {
		return true
	}
	value, exists := policy["no_llm_context"]
	if !exists {
		return false
	}
	enabled, ok := boolean(value)
	return !ok || enabled
}

// StubTag is the reserved frontmatter tag marking a placeholder document:
// scanned, embedded and usable as a link target, but excluded from every
// generation stage. Like no_llm_context it is human-owned — stub documents
// are never sent to the model, so the model can neither add nor remove it.
const StubTag = "stub"

// IsStub reports whether a document is a placeholder stub — either tagged
// `stub` explicitly, or carrying at most maxBodyLines non-empty body lines
// (a negative limit disables size detection; the tag always applies). Stubs
// stay LLM-visible (other notes may link to them and cite them as
// candidates); only their own enrichment and analysis are skipped.
func IsStub(fields map[string]any, body string, maxBodyLines int) bool {
	for _, tag := range StringList(fields, "tags") {
		if strings.EqualFold(strings.TrimSpace(tag), StubTag) {
			return true
		}
	}
	if maxBodyLines < 0 {
		return false
	}
	lines := 0
	for line := range strings.Lines(body) {
		if strings.TrimSpace(line) != "" {
			lines++
			if lines > maxBodyLines {
				return false
			}
		}
	}
	return true
}

// listItems normalises a scalar-or-list YAML value to a string slice,
// appending an issue when the shape is wrong entirely.
func listItems(key string, value any, issues *[]string) []string {
	switch v := value.(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			} else {
				*issues = append(*issues, fmt.Sprintf("%s entries must be strings", key))
			}
		}
		return out
	default:
		*issues = append(*issues, fmt.Sprintf("%s must be a string or list of strings", key))
		return nil
	}
}

// checkStringList validates a tags-style field; lowercase warns since the
// MCP tags filter is exact-match and the convention is lowercase.
func checkStringList(key string, value any, lowercase bool) []string {
	var issues []string
	for _, item := range listItems(key, value, &issues) {
		if lowercase && item != strings.ToLower(item) {
			issues = append(issues, fmt.Sprintf("%s entry %q should be lowercase", key, item))
		}
	}
	return issues
}

// number accepts both YAML numbers and numeric strings (the MCP server parses
// frontmatter with a failsafe schema where scalars arrive as strings).
func number(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case string:
		var f float64
		if _, err := fmt.Sscan(strings.TrimSpace(v), &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

// boolean accepts YAML booleans and "true"/"false" strings.
func boolean(value any) (bool, bool) {
	switch v := value.(type) {
	case bool:
		return v, true
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

// StringList extracts a scalar-or-list frontmatter key (e.g. tags, related)
// as a clean string slice; returns nil when absent or malformed.
func StringList(fields map[string]any, key string) []string {
	var issues []string
	return listItems(key, fields[key], &issues)
}

// Render serializes fields back into a frontmatter block followed by body —
// the inverse of Parse, used to build proposed_content for frontmatter-fix
// suggestions. The body passes through verbatim. Empty fields return the body
// alone (a document without frontmatter). Top-level keys use the stable KBM
// order; custom keys sort between standard metadata and the final kbm block.
func Render(fields map[string]any, body string) (string, error) {
	if len(fields) == 0 {
		return body, nil
	}
	b, err := marshalOrdered(fields)
	if err != nil {
		return "", fmt.Errorf("marshal frontmatter: %w", err)
	}
	return "---\n" + string(b) + "---\n" + body, nil
}

// Rewrite parses content and re-emits it with the frontmatter block replaced
// by fields, keeping the body byte-for-byte. A document without frontmatter
// gains one; passing nil fields strips the block entirely.
func Rewrite(content string, fields map[string]any) (string, error) {
	_, body, err := Parse(content)
	if err != nil {
		return "", err
	}
	return Render(fields, body)
}
