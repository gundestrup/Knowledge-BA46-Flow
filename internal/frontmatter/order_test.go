package frontmatter

import (
	"reflect"
	"strings"
	"testing"
)

func TestRenderUsesStableMCPCompatibleFrontmatterOrder(t *testing.T) {
	fields := map[string]any{
		"kbm":          map[string]any{"freshness": map[string]any{"verdict": "current"}},
		"custom_field": "kept",
		"tags":         []any{"cms", "java"},
		"related":      []any{"cms/README.md"},
		"title":        "Adobe Experience Manager",
		"document_id":  "857ca793-76e9-427c-92c6-443247888962",
		"kb_policy":    map[string]any{"no_llm_context": true},
	}
	got, err := Render(fields, "# Adobe Experience Manager\n")
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{
		"document_id:",
		"title:",
		"related:",
		"tags:",
		"kb_policy:",
		"custom_field:",
		"kbm:",
	}
	last := -1
	for _, key := range wantOrder {
		at := strings.Index(got, key)
		if at <= last {
			t.Fatalf("%q not in stable order in:\n%s", key, got)
		}
		last = at
	}
	if !strings.Contains(got, `document_id: "857ca793-76e9-427c-92c6-443247888962"`) {
		t.Fatalf("document_id should be a quoted UUID:\n%s", got)
	}
	fieldsAfter, body, err := Parse(got)
	if err != nil || fieldsAfter["title"] != fields["title"] ||
		StringList(fieldsAfter, "tags")[0] != "cms" || body != "# Adobe Experience Manager\n" {
		t.Fatalf("serialization changed fields/body: fields=%v body=%q err=%v", fieldsAfter, body, err)
	}
}

func TestNormalizeOrderDoesNotChangeFieldValues(t *testing.T) {
	raw := "---\n# Retrieval tags\ntags: [cms, java]\n# Display title\ntitle: Adobe Experience Manager\n---\n# Adobe\n"
	normalized, err := NormalizeOrder(raw)
	if err != nil {
		t.Fatal(err)
	}
	titleAt, tagsAt := strings.Index(normalized, "title: Adobe Experience Manager"), strings.Index(normalized, "tags:")
	if titleAt < 0 || tagsAt < 0 || titleAt >= tagsAt {
		t.Fatalf("title not moved before tags in stable order:\n%s", normalized)
	}
	if !strings.Contains(normalized, "# Display title") || !strings.Contains(normalized, "# Retrieval tags") {
		t.Fatalf("frontmatter comments were lost during ordering:\n%s", normalized)
	}
	before, beforeBody, _ := Parse(raw)
	after, afterBody, _ := Parse(normalized)
	if beforeBody != afterBody || before["title"] != after["title"] ||
		!reflect.DeepEqual(before["tags"], after["tags"]) {
		t.Fatalf("normalization changed values: %v -> %v", before, after)
	}
}

func TestSemanticPartsIgnoreGraphAndInternalDestinationChanges(t *testing.T) {
	a := "---\ntitle: AEM\ntags: [cms]\nrelated: [cms/old.md]\n---\nRead [guide](../old.md#top).\n"
	b := "---\ntitle: AEM\ntags: [cms]\nrelated: [cms/new.md]\n---\nRead [guide](../new.md#section).\n"
	if SemanticHash(a) != SemanticHash(Canonical(a)) {
		t.Fatal("managed/identity canonicalization changed semantic hash")
	}
	if SemanticHash(a) != SemanticHash(b) {
		aFM, aBody := SemanticParts(a)
		bFM, bBody := SemanticParts(b)
		t.Fatalf("graph-only changes altered semantic hash:\n%q+%q\n%q+%q", aFM, aBody, bFM, bBody)
	}
	titleChanged := strings.Replace(b, "title: AEM", "title: Adobe Experience Manager", 1)
	if SemanticHash(titleChanged) == SemanticHash(a) {
		t.Fatal("semantic title change was ignored")
	}
	labelChanged := strings.Replace(b, "[guide]", "[manual]", 1)
	if SemanticHash(labelChanged) == SemanticHash(a) {
		t.Fatal("visible link-label change was ignored")
	}
}
