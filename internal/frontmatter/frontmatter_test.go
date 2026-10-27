package frontmatter

import (
	"reflect"
	"strings"
	"testing"
)

func TestPreserveManaged(t *testing.T) {
	for _, tt := range []struct {
		name, original, proposed string
	}{
		{"new verdict", "---\ntags: [go]\nkbm:\n  freshness: {verdict: current}\n---\nbody\n", "---\ntags: [go, tool]\n---\nbody\n"},
		{"old verdict", "---\ntags: [go]\nkbm:\n  freshness: {verdict: current}\n---\nbody\n", "---\ntags: [go, tool]\nkbm:\n  freshness: {verdict: stale}\n---\nbody\n"},
		{"no live block", "---\ntags: [go]\n---\nbody\n", "---\ntags: [go, tool]\nkbm: {injected: true}\n---\nbody\n"},
		{"empty live block", "---\nkbm: {}\n---\nbody\n", "body\n"},
		{"only managed metadata", "---\nkbm: {freshness: {verdict: current}}\n---\nbody\n", "body\n"},
		{"unchanged", "---\nkbm: {link_check: {dead: []}}\n---\nbody\n", "---\nkbm: {link_check: {dead: []}}\n---\nbody\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PreserveManaged(tt.original, tt.proposed)
			if err != nil {
				t.Fatal(err)
			}
			before, _, _ := Parse(tt.original)
			after, _, err := Parse(got)
			if err != nil || !reflect.DeepEqual(before[ManagedKey], after[ManagedKey]) {
				t.Fatalf("managed block changed: %v -> %v, err=%v", before, after, err)
			}
			if Canonical(got) != Canonical(tt.proposed) {
				t.Fatalf("proposal's canonical bytes changed: %q -> %q", tt.proposed, got)
			}
		})
	}
}

func TestParseNoFrontmatter(t *testing.T) {
	for _, in := range []string{"", "# Title\n\nbody", "  ---\nnot: fence", "---no-space\n---\n"} {
		fields, body, err := Parse(in)
		if err != nil || fields != nil || body != in {
			t.Fatalf("Parse(%q) = fields=%v body=%q err=%v", in, fields, body, err)
		}
	}
}

func TestParseValidBlock(t *testing.T) {
	content := "---\ntitle: Hello\ntags: [go, cli]\n---\n# Body\n"
	fields, body, err := Parse(content)
	if err != nil {
		t.Fatal(err)
	}
	if fields["title"] != "Hello" {
		t.Fatalf("fields=%v", fields)
	}
	if body != "# Body\n" {
		t.Fatalf("body=%q", body)
	}
}

func TestParseEmptyBlockAndCRLFAndBOM(t *testing.T) {
	fields, body, err := Parse("---\r\n---\r\nbody")
	if err != nil || len(fields) != 0 || fields == nil {
		t.Fatalf("empty block: fields=%v err=%v", fields, err)
	}
	if body != "body" {
		t.Fatalf("body=%q", body)
	}
	fields, _, err = Parse("\uFEFF---\ntitle: T\n---\nbody")
	if err != nil || fields["title"] != "T" {
		t.Fatalf("BOM: fields=%v err=%v", fields, err)
	}
}

func TestParseMalformed(t *testing.T) {
	for _, in := range []string{
		"---\ntitle: never closed\n" + strings.Repeat("x", 9000),
		"---\n: bad: yaml: [\n---\nbody",
		"---\n- a\n- b\n---\nbody", // sequence, not a mapping
	} {
		if _, _, err := Parse(in); err == nil {
			t.Fatalf("expected error for %q", in[:40])
		}
	}
}

func TestValidateCleanDocument(t *testing.T) {
	fields, _, err := Parse(`---
title: Theme controller
tags: [bulma, javascript]
status: active
review_status: approved
tier: validated
confidence: 0.9
last_verified_at: 2026-05-08
contradicted_by: [old/theme.md]
related: [frameworks/bulma/README.md]
manual_edits: true
relevance_score: 42
arxiv_id: "2604.17948"
kb_policy:
  no_llm_context: true
  resource_read: local_only
  sensitivity: internal
custom_note: still fine
---
body
`)
	if err != nil {
		t.Fatal(err)
	}
	if issues := Validate(fields); len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
}

func TestValidateReportsBadValues(t *testing.T) {
	fields, _, err := Parse(`---
tier: golden
status: whatever
review_status: maybe
confidence: high
last_verified_at: "not a date"
manual_edits: perhaps
tags: [GO, 123]
related: ["../outside.md", "/abs/path.md"]
contradicted_by: 12
kb_policy:
  resource_read: everything
  no_llm_context: [maybe]
  unknown_sub: x
metrics_used: [nDCG]
unknown_map:
  nested: value
---
body
`)
	if err != nil {
		t.Fatal(err)
	}
	issues := Validate(fields)
	joined := strings.Join(issues, "\n")
	for _, want := range []string{
		`tier "golden"`, `status "whatever"`, `review_status "maybe"`,
		"confidence must be a number", "not an ISO date", "manual_edits",
		`tags entry "GO"`, "tags entries must be strings",
		"not a valid repository-relative path", "contradicted_by must be a string or list",
		"resource_read must be allow|local_only|deny", "no_llm_context",
		"not a recognised policy key", "metrics_used must be a string", `unknown key "unknown_map"`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("issues missing %q:\n%s", want, joined)
		}
	}
}

func TestValidateNumericStringsAndBooleans(t *testing.T) {
	// The MCP server parses with a failsafe schema, so scalars arrive as
	// strings; validators must accept that shape too.
	fields := map[string]any{
		"confidence":   "0.75",
		"manual_edits": "true",
		"status":       "Deprecated", // case-insensitive lifecycle check
	}
	if issues := Validate(fields); len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
}

func TestStringList(t *testing.T) {
	fields := map[string]any{
		"tags":    []any{"a", "b"},
		"related": "single.md",
		"bad":     42,
	}
	if got := StringList(fields, "tags"); len(got) != 2 || got[0] != "a" {
		t.Fatalf("tags=%v", got)
	}
	if got := StringList(fields, "related"); len(got) != 1 || got[0] != "single.md" {
		t.Fatalf("related=%v", got)
	}
	if got := StringList(fields, "bad"); got != nil {
		t.Fatalf("bad=%v", got)
	}
	if got := StringList(fields, "missing"); got != nil {
		t.Fatalf("missing=%v", got)
	}
}

func TestNoLLMContextFailsClosedOnInvalidPolicy(t *testing.T) {
	for _, tt := range []struct {
		name   string
		fields map[string]any
		want   bool
	}{
		{name: "no policy", fields: map[string]any{}, want: false},
		{name: "policy without flag", fields: map[string]any{"kb_policy": map[string]any{"resource_read": "allow"}}, want: false},
		{name: "boolean true", fields: map[string]any{"kb_policy": map[string]any{"no_llm_context": true}}, want: true},
		{name: "failsafe string true", fields: map[string]any{"kb_policy": map[string]any{"no_llm_context": "true"}}, want: true},
		{name: "boolean false", fields: map[string]any{"kb_policy": map[string]any{"no_llm_context": false}}, want: false},
		{name: "failsafe string false", fields: map[string]any{"kb_policy": map[string]any{"no_llm_context": "false"}}, want: false},
		{name: "invalid flag", fields: map[string]any{"kb_policy": map[string]any{"no_llm_context": "yes"}}, want: true},
		{name: "invalid policy shape", fields: map[string]any{"kb_policy": "allow"}, want: true},
		{name: "invalid policy member", fields: map[string]any{"kb_policy": map[string]any{"unknown": true}}, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := NoLLMContext(tt.fields); got != tt.want {
				t.Fatalf("NoLLMContext(%v)=%v, want %v", tt.fields, got, tt.want)
			}
		})
	}
}

func TestIsStub(t *testing.T) {
	for _, tt := range []struct {
		name   string
		fields map[string]any
		body   string
		limit  int
		want   bool
	}{
		{name: "no tags, large body", fields: map[string]any{}, body: "a\nb\nc\nd\ne\nf\n", limit: 5, want: false},
		{name: "other tags, large body", fields: map[string]any{"tags": []any{"tools", "go"}}, body: "a\nb\nc\nd\ne\nf\n", limit: 5, want: false},
		{name: "stub in list", fields: map[string]any{"tags": []any{"tools", "stub"}}, body: "long\nbody\nover\nthe\nline\nlimit\n", limit: 5, want: true},
		{name: "scalar stub", fields: map[string]any{"tags": "stub"}, body: "", limit: 5, want: true},
		{name: "case-insensitive", fields: map[string]any{"tags": []any{"Stub"}}, body: "", limit: 5, want: true},
		{name: "malformed tags, large body", fields: map[string]any{"tags": 42}, body: "a\nb\nc\nd\ne\nf\n", limit: 5, want: false},
		{name: "tag wins over disabled size check", fields: map[string]any{"tags": []any{"stub"}}, body: "a\nb\nc\nd\ne\nf\n", limit: -1, want: true},
		{name: "body at limit", fields: map[string]any{}, body: "a\nb\nc\nd\ne\n", limit: 5, want: true},
		{name: "body over limit", fields: map[string]any{}, body: "a\nb\nc\nd\ne\nf\n", limit: 5, want: false},
		{name: "blank lines ignored", fields: map[string]any{}, body: "a\n\n\n \nb\n\n", limit: 2, want: true},
		{name: "empty body", fields: map[string]any{}, body: "", limit: 5, want: true},
		{name: "negative limit disables size check", fields: map[string]any{}, body: "x\n", limit: -1, want: false},
		{name: "zero limit stubs only empty bodies", fields: map[string]any{}, body: "x\n", limit: 0, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsStub(tt.fields, tt.body, tt.limit); got != tt.want {
				t.Fatalf("IsStub(%v, %q, %d)=%v, want %v", tt.fields, tt.body, tt.limit, got, tt.want)
			}
		})
	}
}

func TestCanonicalExcludesManagedBlock(t *testing.T) {
	base := "---\ntitle: T\ntags: [a]\n---\n# Body\n"
	if got := Canonical(base); got != base {
		t.Fatalf("no kbm block should return input verbatim, got %q", got)
	}
	managed := "---\ntitle: T\nkbm:\n  link_check:\n    at: \"2026-10-20\"\ntags: [a]\n---\n# Body\n"
	got := Canonical(managed)
	if !strings.Contains(got, "title: T") || !strings.Contains(got, "tags: [a]") || !strings.Contains(got, "# Body") {
		t.Fatalf("canonical lost real content: %q", got)
	}
	if strings.Contains(got, "kbm") || strings.Contains(got, "link_check") || strings.Contains(got, "2026") {
		t.Fatalf("canonical kept managed data: %q", got)
	}
	// A second check run only changes the managed block — the hash input stays identical.
	updated := "---\ntitle: T\nkbm:\n  link_check:\n    at: \"2026-10-21\"\ntags: [a]\n---\n# Body\n"
	if Canonical(updated) != got {
		t.Fatalf("kbm update changed canonical form:\n%q\nvs\n%q", Canonical(updated), got)
	}
}

func TestCanonicalEdgeCases(t *testing.T) {
	for _, in := range []string{
		"", "# No frontmatter", "---\ntitle: T\n---\nbody",
		"---\nunclosed: [x\nbody", // unclosed fence: verbatim
	} {
		if got := Canonical(in); got != in {
			t.Fatalf("Canonical(%q) = %q, want input verbatim", in, got)
		}
	}
	// kbm as the only key leaves an empty block — still canonical.
	if got := Canonical("---\nkbm: {}\n---\nbody\n"); strings.Contains(got, "kbm") {
		t.Fatalf("kbm-only block not excised: %q", got)
	}
	// A column-0 comment inside the block goes with it; next key survives.
	in := "---\nkbm:\n  a: 1\n# tool note\ntitle: T\n---\nbody\n"
	got := Canonical(in)
	if strings.Contains(got, "a: 1") || !strings.Contains(got, "title: T") {
		t.Fatalf("block boundary wrong: %q", got)
	}
}

func TestSetManaged(t *testing.T) {
	out, err := SetManaged("# Body\nno frontmatter", "link_check", map[string]any{"at": "t1", "dead": []string{"u"}})
	if err != nil {
		t.Fatal(err)
	}
	fields, body, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if body != "# Body\nno frontmatter" {
		t.Fatalf("body changed: %q", body)
	}
	kbm, ok := fields[ManagedKey].(map[string]any)
	if !ok {
		t.Fatalf("kbm not a mapping: %v", fields[ManagedKey])
	}
	if lc, _ := kbm["link_check"].(map[string]any); lc["at"] != "t1" {
		t.Fatalf("link_check=%v", kbm["link_check"])
	}
	// A second managed key keeps the first; nil removes one key.
	out, err = SetManaged(out, "freshness", map[string]any{"at": "t2"})
	if err != nil {
		t.Fatal(err)
	}
	out, err = SetManaged(out, "link_check", nil)
	if err != nil {
		t.Fatal(err)
	}
	fields, _, _ = Parse(out)
	kbm = fields[ManagedKey].(map[string]any)
	if _, ok := kbm["link_check"]; ok {
		t.Fatalf("link_check should be removed: %v", kbm)
	}
	if _, ok := kbm["freshness"]; !ok {
		t.Fatalf("sibling key lost: %v", kbm)
	}
	// Canonical content is unchanged by any of this.
	if Canonical(out) != Canonical("# Body\nno frontmatter") {
		t.Fatalf("managed writes changed canonical form")
	}
}

// Regression: frontmatter whose top-level keys are uniformly indented
// (" kbm:") parses fine, and a managed write must replace the existing
// block rather than append a duplicate column-0 key — observed production
// corruption where an indented file ended up with two kbm mappings.
func TestSetManagedIndentedFrontmatter(t *testing.T) {
	input := "---\n tags:\n    - database\n title: MongoDB\n kbm:\n    freshness:\n        at: \"2026-10-25T10:41:22Z\"\n        verdict: review\n    link_check:\n        dead: []\n---\n# MongoDB\n"
	out, err := SetManaged(input, "freshness", map[string]any{"at": "2026-10-27T06:30:17Z", "verdict": "current"})
	if err != nil {
		t.Fatalf("managed write on indented frontmatter must succeed: %v", err)
	}
	fields, body, err := Parse(out)
	if err != nil {
		t.Fatalf("output must stay parseable: %v", err)
	}
	if body != "# MongoDB\n" {
		t.Fatalf("body changed: %q", body)
	}
	kbm, ok := fields[ManagedKey].(map[string]any)
	if !ok {
		t.Fatalf("kbm not a mapping: %v", fields[ManagedKey])
	}
	if fr, _ := kbm["freshness"].(map[string]any); fr["at"] != "2026-10-27T06:30:17Z" {
		t.Fatalf("freshness not updated: %v", kbm["freshness"])
	}
	if _, ok := kbm["link_check"]; !ok {
		t.Fatalf("link_check sibling lost: %v", kbm)
	}
	if Canonical(out) != Canonical(input) {
		t.Fatalf("managed write moved the canonical hash")
	}
}

func TestSetManagedNeverWritesDuplicateKBM(t *testing.T) {
	inputs := map[string]string{
		"indented keys":     "---\n tags:\n    - a\n kbm:\n    freshness:\n        at: t0\n---\nbody\n",
		"deep indent":       "---\n    tags: [a]\n    kbm:\n        link_check:\n            dead: []\n---\nbody\n",
		"kbm list children": "---\ntags: [a]\nkbm:\n  link_check:\n    dead:\n    - http://x\n---\nbody\n",
	}
	for name, input := range inputs {
		out, err := SetManaged(input, "freshness", map[string]any{"at": "t1"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		fields, _, err := Parse(out)
		if err != nil {
			t.Fatalf("%s: output unparseable: %v", name, err)
		}
		kbm, _ := fields[ManagedKey].(map[string]any)
		if fr, _ := kbm["freshness"].(map[string]any); fr["at"] != "t1" {
			t.Fatalf("%s: freshness not updated: %v", name, kbm)
		}
	}
}

func TestValidateManagedKey(t *testing.T) {
	if issues := Validate(map[string]any{ManagedKey: map[string]any{"link_check": map[string]any{"at": "t"}}}); len(issues) != 0 {
		t.Fatalf("valid kbm flagged: %v", issues)
	}
	if issues := Validate(map[string]any{ManagedKey: "oops"}); len(issues) != 1 {
		t.Fatalf("non-map kbm should flag: %v", issues)
	}
}

func TestParseFailsClosedOnMalformedFrontmatter(t *testing.T) {
	for name, input := range map[string]string{
		"invalid utf-8 in frontmatter": "---\ntitle: \xff\xfe\n---\nbody",
		"NUL byte in frontmatter":      "---\ntitle: a\x00b\n---\nbody",
		"duplicate mapping keys":       "---\ntitle: a\ntitle: b\n---\nbody",
		"unclosed flow sequence":       "---\ntags: [x\n---\nbody",
	} {
		if _, _, err := Parse(input); err == nil {
			t.Errorf("%s: Parse should fail, not silently degrade", name)
		}
		// Managed writes must fail closed too — a corrupt file is never
		// silently overwritten with a kbm block.
		if _, err := SetManaged(input, "k", map[string]any{"v": 1}); err == nil {
			t.Errorf("%s: SetManaged should refuse malformed frontmatter", name)
		}
	}
}

func TestManagedKeyIsNamespacedRegardlessOfShape(t *testing.T) {
	// A hand-written `kbm` scalar or list occupies the tool-owned namespace:
	// it is excised from the canonical hash exactly like a proper block, and
	// SetManaged replaces it with a valid map rather than nesting into it.
	for _, input := range []string{
		"---\ntags: [x]\nkbm: yes\n---\n# T\n",
		"---\nkbm: [1, 2]\n---\nbody\n",
	} {
		fields, _, err := Parse(input)
		if err != nil {
			t.Fatal(err)
		}
		if Canonical(input) == input {
			t.Error("non-map kbm must still be excised from the canonical form")
		}
		out, err := SetManaged(input, "link_check", map[string]any{"at": "now"})
		if err != nil {
			t.Fatal(err)
		}
		updated, _, err := Parse(out)
		if err != nil {
			t.Fatal(err)
		}
		kbm, ok := updated[ManagedKey].(map[string]any)
		if !ok || kbm["link_check"] == nil {
			t.Fatalf("SetManaged should install a proper map over any kbm shape: %v", updated)
		}
		_ = fields
	}
}

func TestParsePreservesNonUTF8Body(t *testing.T) {
	input := "---\ntags: [x]\n---\n# T\n\n\xff\xfe binary body\n"
	fields, body, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) == 0 || body != "# T\n\n\xff\xfe binary body\n" {
		t.Fatalf("body bytes must pass through untouched: %q", body)
	}
}

// FuzzParse asserts the parser never panics on arbitrary file bytes and is
// deterministic; valid parses must survive Validate without panicking.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"---\ntags: [x]\n---\n# Doc\n\nBody.\n",
		"no frontmatter at all",
		"---\nkbm: {link_check: {dead: [u]}}\n---\nb",
		"---\nunclosed: [x\nbody",
		"---\r\ntitle: x\r\n---\r\nbody",
		"",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		fm1, body1, err1 := Parse(s)
		fm2, body2, err2 := Parse(s)
		if (err1 == nil) != (err2 == nil) {
			t.Fatalf("Parse not deterministic for %q", s)
		}
		if err1 == nil {
			if body1 != body2 || len(fm1) != len(fm2) {
				t.Fatalf("Parse output not deterministic for %q", s)
			}
			_ = Validate(fm1) // must not panic on parsed maps
		}
	})
}

// FuzzCanonicalParts pins the split-hash contract: the canonical form is
// exactly frontmatter-part + body-part, byte for byte, on any input.
func FuzzCanonicalParts(f *testing.F) {
	for _, seed := range []string{
		"---\ntags: [x]\n---\n# Doc\n\nBody.\n",
		"---\nkbm:\n  link_check: {dead: [u]}\ntags: [x]\n---\nb",
		"no frontmatter",
		"---\n---\n",
		"---",
		"\xff\xfe binary",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		fmPart, bodyPart := CanonicalParts(s)
		if got := fmPart + bodyPart; got != Canonical(s) {
			t.Fatalf("canonical split inconsistent for %q", s)
		}
	})
}

// FuzzSetManaged pins the kbm hash-immunity invariant: when a managed write
// succeeds, the canonical hash of the file must not change — otherwise every
// operational write would requeue analysis. It must also emit parseable YAML.
func FuzzSetManaged(f *testing.F) {
	for _, seed := range []string{
		"---\ntags: [x]\n---\n# Doc\n\nBody.\n",
		"---\nkbm:\n  freshness: {verdict: current}\ntags: [x]\n---\nbody\n",
		"no frontmatter at all",
		"---\nkbm: yes\n---\nbody\n",
		"---\n---\nbody\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out, err := SetManaged(s, "link_check", map[string]any{"at": "t", "dead": []string{"u"}})
		if err != nil {
			return // malformed input refuses — that path is covered by the table
		}
		if Canonical(out) != Canonical(s) {
			t.Fatalf("managed write changed the canonical hash for %q", s)
		}
		fields, _, err := Parse(out)
		if err != nil {
			t.Fatalf("SetManaged produced unparseable output for %q: %v", s, err)
		}
		kbm, ok := fields[ManagedKey].(map[string]any)
		if !ok || kbm["link_check"] == nil {
			t.Fatalf("managed key missing after write for %q", s)
		}
	})
}

func BenchmarkCanonicalParts(b *testing.B) {
	doc := "---\ntitle: T\ntags: [go, tool]\nkbm:\n  link_check:\n    at: t\n    dead: [u]\n---\n# T\n\n" +
		strings.Repeat("Body paragraph with [links](other.md) and `code`.\n\n", 80)
	for b.Loop() {
		CanonicalParts(doc)
	}
}
