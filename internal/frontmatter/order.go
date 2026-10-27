package frontmatter

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"knowledge-ba46-flow/internal/links"
)

// keyOrder is the stable authoring order for emitted KBM-flow frontmatter.
// document_id/title identify the note, graph/retrieval fields follow, and
// operational kbm metadata is always last.
var keyOrder = []string{
	"document_id",
	"title",
	"summary",
	"related",
	"tags",
	"contradicted_by",
	"arxiv_id",
	"authors",
	"published",
	"source_url",
	"source",
	"ingested_at",
	"status",
	"review_status",
	"tier",
	"confidence",
	"last_verified_at",
	"manual_edits",
	"promote_model",
	"kb_policy",
}

// SemanticParts returns frontmatter and body projections used for LLM-work
// freshness. Graph-only metadata and relative Markdown link destinations are
// omitted, while titles, tags, privacy/lifecycle metadata, labels and prose
// remain. Invalid YAML falls back to the conservative raw canonical form.
func SemanticParts(content string) (fm, body string) {
	fields, parsedBody, err := Parse(content)
	if err != nil {
		return CanonicalParts(content)
	}
	if fields != nil {
		fm = SemanticFrontmatter(fields)
	}
	return fm, links.SemanticBody(parsedBody)
}

// SemanticFrontmatter serializes content metadata while omitting graph edges
// and tool-owned fields that do not change the note's knowledge.
func SemanticFrontmatter(fields map[string]any) string {
	semantic := maps.Clone(fields)
	delete(semantic, ManagedKey)
	delete(semantic, "document_id")
	delete(semantic, "related")
	delete(semantic, "contradicted_by")
	if len(semantic) == 0 {
		return ""
	}
	encoded, err := json.Marshal(semantic)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// SemanticFrontmatterHashJSON fingerprints a stored frontmatter object for
// the one-time migration from byte hashes to semantic task hashes.
func SemanticFrontmatterHashJSON(raw string) (string, bool) {
	var fields map[string]any
	if err := json.Unmarshal([]byte(raw), &fields); err != nil || fields == nil {
		return "", false
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(SemanticFrontmatter(fields)))), true
}

// SemanticHash fingerprints meaningful note content, not graph edges or
// internal link destinations. Keep Canonical for proposal/source freshness.
func SemanticHash(content string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(SemanticContent(content))))
}

// SemanticContent is the stable text projection used to invalidate LLM and
// embedding work. The source file remains unchanged; this is not a write format.
func SemanticContent(content string) string {
	fm, body := SemanticParts(content)
	return fm + "\x00" + body
}

// SemanticBodyHash fingerprints the Markdown body without internal link syntax.
func SemanticBodyHash(content string) string {
	_, body := SemanticParts(content)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
}

// NormalizeOrder re-emits valid frontmatter in the stable authoring order.
// Files are normalized only when producing a reviewed proposal; scanning
// preserves existing bytes (apart from the separate hash-neutral ID insertion).
func NormalizeOrder(content string) (string, error) {
	fields, body, err := Parse(content)
	if err != nil || fields == nil || len(fields) == 0 {
		return content, err
	}
	trimmed := strings.TrimPrefix(content, "\uFEFF")
	bom := content[:len(content)-len(trimmed)]
	open := openFence.FindStringIndex(trimmed)
	if open == nil {
		return content, nil
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
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(window[:close[0]]), &document); err != nil {
		return "", fmt.Errorf("parse frontmatter for ordering: %w", err)
	}
	if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("frontmatter must be a mapping")
	}
	mapping := document.Content[0]
	type pair struct {
		key       string
		keyNode   *yaml.Node
		valueNode *yaml.Node
	}
	pairs := make([]pair, 0, len(mapping.Content)/2)
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		keyNode, valueNode := mapping.Content[i], mapping.Content[i+1]
		pairs = append(pairs, pair{key: keyNode.Value, keyNode: keyNode, valueNode: valueNode})
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		return compareKeys(pairs[i].key, pairs[j].key) < 0
	})
	mapping.Content = mapping.Content[:0]
	for _, item := range pairs {
		if item.key == "document_id" && item.valueNode.Kind == yaml.ScalarNode {
			item.valueNode.Style = yaml.DoubleQuotedStyle
		}
		mapping.Content = append(mapping.Content, item.keyNode, item.valueNode)
	}
	encoded, err := yaml.Marshal(&document)
	if err != nil {
		return "", fmt.Errorf("marshal ordered frontmatter: %w", err)
	}
	return bom + "---\n" + string(encoded) + "---\n" + body, nil
}

func marshalOrdered(fields map[string]any) ([]byte, error) {
	if len(fields) == 0 {
		return nil, nil
	}
	order := orderedKeys(fields)
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, key := range order {
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
		var valueNode yaml.Node
		if err := valueNode.Encode(fields[key]); err != nil {
			return nil, fmt.Errorf("encode frontmatter field %q: %w", key, err)
		}
		if key == "document_id" {
			valueNode.Style = yaml.DoubleQuotedStyle
		}
		node.Content = append(node.Content, keyNode, &valueNode)
	}
	return yaml.Marshal(node)
}

func orderedKeys(fields map[string]any) []string {
	known := make(map[string]int, len(keyOrder))
	for i, key := range keyOrder {
		known[key] = i
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		if key != ManagedKey {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(a, b string) int { return compareKeysWithOrder(a, b, known) })
	if _, ok := fields[ManagedKey]; ok {
		keys = append(keys, ManagedKey)
	}
	return keys
}

func compareKeys(a, b string) int {
	known := make(map[string]int, len(keyOrder))
	for i, key := range keyOrder {
		known[key] = i
	}
	return compareKeysWithOrder(a, b, known)
}

func compareKeysWithOrder(a, b string, known map[string]int) int {
	if a == ManagedKey {
		if b == ManagedKey {
			return 0
		}
		return 1
	}
	if b == ManagedKey {
		return -1
	}
	ai, aKnown := known[a]
	bi, bKnown := known[b]
	switch {
	case aKnown && bKnown:
		return ai - bi
	case aKnown:
		return -1
	case bKnown:
		return 1
	default:
		return strings.Compare(a, b)
	}
}
