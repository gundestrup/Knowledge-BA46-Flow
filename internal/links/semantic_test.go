package links

import (
	"strings"
	"testing"
)

func TestSemanticBodyIgnoresInternalDestinationsOnly(t *testing.T) {
	a := "Read [the guide](../docs/a.md?view=short#start), [site](https://example.com), and ` [code](local.md) `.\n"
	b := "Read [the guide](../new/a.md#other), [site](https://example.com), and ` [code](local.md) `.\n"
	if SemanticBody(a) != SemanticBody(b) {
		t.Fatalf("internal destination changed semantic body:\n%q\n%q", SemanticBody(a), SemanticBody(b))
	}
	if got := SemanticBody("Read the guide, [site](https://example.com).\n"); got == SemanticBody(a) {
		t.Fatal("external link or visible body text was ignored")
	}
}

func TestSemanticBodyPreservesCodeAndVisibleLinkText(t *testing.T) {
	body := "See [AEM](../cms/aem.md).\n\n```md\n[example](../example.md)\n```\n\n`[inline](../inline.md)`\n"
	got := SemanticBody(body)
	for _, want := range []string{
		"See AEM.\n",
		"```md\n[example](../example.md)\n```\n",
		"`[inline](../inline.md)`",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("semantic body %q missing %q", got, want)
		}
	}
	if got := SemanticBody("See [logo](../img/logo.svg).\n"); got != "See logo.\n" {
		t.Fatalf("image/link label projection=%q", got)
	}
	if got := SemanticBody("[parenthesized](../docs/guide_(legacy).md)\n"); got != "parenthesized\n" {
		t.Fatalf("parenthesized internal target was not normalized safely: %q", got)
	}
}

func TestSemanticBodyKeepsExternalAnchorsAndAbsoluteTargets(t *testing.T) {
	body := "[web](https://example.com) [mail](mailto:a@example.com) [root](/docs/a.md)\n"
	if got := SemanticBody(body); got != body {
		t.Fatalf("non-relative links changed: %q", got)
	}
}
