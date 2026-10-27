package links

import (
	"slices"
	"strings"
	"testing"
)

func TestInternalTargets(t *testing.T) {
	body := `See [the guide](../guides/x.md) and [sibling](sib.md).
External [site](https://example.com) and [mail](mailto:a@b.c) are out.
Anchors like [jump](#section) and absolute [root](/abs.md) are skipped.
` + "```\n[not a link](fake.md)\n```\n" + "`[inline](code.md)` is inline code.\n[dup](sib.md) twice."
	got := InternalTargets(body)
	want := []string{"../guides/x.md", "sib.md"}
	if !slices.Equal(got, want) {
		t.Fatalf("InternalTargets=%v, want %v", got, want)
	}
}

func TestExternalURLs(t *testing.T) {
	body := `Docs: [site](https://example.com/path).
Bare https://other.example/a?b=1, and https://dup.example same as [dup](https://dup.example).
` + "```\nhttps://in-fence.example\n```\n`https://inline.example` skipped."
	got := ExternalURLs(body)
	want := []string{"https://example.com/path", "https://dup.example", "https://other.example/a?b=1"}
	if !slices.Equal(got, want) {
		t.Fatalf("ExternalURLs=%v, want %v", got, want)
	}
}

func TestInternalCandidates(t *testing.T) {
	got := InternalCandidates("tools/x.md", "../guides/g.md#s")
	if !slices.Equal(got, []string{"guides/g.md", "../guides/g.md"}) {
		t.Fatalf("candidates=%v", got)
	}
	got = InternalCandidates("tools/x.md", "sib.md?y#z")
	if !slices.Equal(got, []string{"tools/sib.md", "sib.md"}) {
		t.Fatalf("candidates=%v", got)
	}
	if InternalCandidates("a.md", "#only") != nil || InternalCandidates("a.md", "") != nil {
		t.Fatal("fragment-only/empty targets should have no candidates")
	}
}

func TestIsDocumentTarget(t *testing.T) {
	for target, ok := range map[string]bool{
		"x.md": true, "x.markdown": true, "x.md#sec": true,
		"x.png": false, "x": false, "x.txt": false,
	} {
		if IsDocumentTarget(target) != ok {
			t.Fatalf("IsDocumentTarget(%q) = %v, want %v", target, !ok, ok)
		}
	}
}

// FuzzExternalURLs: extraction must never panic, and every reported URL is a
// verbatim http(s) substring of the input — no invented targets.
func FuzzExternalURLs(f *testing.F) {
	for _, seed := range []string{
		"See [docs](https://example.com/a) and https://b.example/x?y=1.",
		"`https://in-code.example` and https://real.example",
		"```\nhttps://fenced.example\n```\nhttps://outside.example",
		"no urls",
		"https://",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, u := range ExternalURLs(s) {
			if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
				t.Fatalf("non-http URL reported: %q from %q", u, s)
			}
			if !strings.Contains(s, u) {
				t.Fatalf("URL %q is not a substring of the source %q", u, s)
			}
		}
	})
}

// FuzzInternalTargets: same invariants for ](target) link targets — never
// panic, and every target is a literal substring of the body.
func FuzzInternalTargets(f *testing.F) {
	for _, seed := range []string{
		"[go](languages/go.md) and [x](../up.md)",
		"![img](pic.png) `](code.md)`",
		"```\n[fenced](fenced.md)\n```\n[out](o.md)",
		"empty ]() and [missing close](",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, target := range InternalTargets(s) {
			if !strings.Contains(s, target) {
				t.Fatalf("target %q is not a substring of the source %q", target, s)
			}
		}
	})
}

// TestRetarget rewrites destinations to a moved document's new path while
// leaving live links, code, external URLs and anchors untouched.
func TestRetarget(t *testing.T) {
	renames := map[string]string{"sub/old.md": "archive/old.md", "a.md": "b.md"}
	retarget := func(old string) (string, bool) {
		n, ok := renames[old]
		return n, ok
	}
	body := `# Doc

See [old note](old.md) and [root link](a.md#section) and [live](keep.md).
Link in inline code stays: ` + "`[x](old.md)`" + `.

` + "```" + `
[code link](old.md)
` + "```" + `

[external](https://example.com/old.md) [anchor](#here) [dup](old.md)
`
	exists := func(path string) bool {
		return slices.Contains([]string{"archive/old.md", "b.md", "sub/keep.md"}, path)
	}
	got := Retarget("sub/doc.md", body, retarget, exists)
	want := `# Doc

See [old note](../archive/old.md) and [root link](../b.md#section) and [live](keep.md).
Link in inline code stays: ` + "`[x](old.md)`" + `.

` + "```" + `
[code link](old.md)
` + "```" + `

[external](https://example.com/old.md) [anchor](#here) [dup](../archive/old.md)
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestRetargetPreservesLiveAndIdentityLinks(t *testing.T) {
	body := "[catalog](../frameworks/css/README.md) [local](README.md)\n"
	for _, live := range []bool{true, false} {
		got := Retarget("languages/scss.md", body, func(path string) (string, bool) {
			return path, true
		}, func(string) bool { return live })
		if got != body {
			t.Fatalf("live=%v: unchanged paths must not be rewritten: %q", live, got)
		}
	}
	// A live relative link takes precedence over a historical root fallback.
	got := Retarget("sub/doc.md", "[live](old.md)", func(string) (string, bool) {
		return "archive/new.md", true
	}, func(path string) bool { return path == "sub/old.md" || path == "archive/new.md" })
	if got != "[live](old.md)" {
		t.Fatalf("live relative link changed: %s", got)
	}
}

func TestRetargetPreservesSuffixAndRejectsUnsafeEndpoint(t *testing.T) {
	for next, want := range map[string]string{
		"archive/new.md":      "[note](../archive/new.md?view=1#part)",
		"../outside.md":       "[note](old.md?view=1#part)",
		"archive/new note.md": "[note](../archive/new%20note.md?view=1#part)",
	} {
		got := Retarget("sub/doc.md", "[note](old.md?view=1#part)", func(string) (string, bool) {
			return next, true
		}, func(path string) bool { return path == next })
		if got != want {
			t.Fatalf("next=%q got=%q want=%q", next, got, want)
		}
	}
	if got := InternalCandidates("sub/doc.md", "../archive/new%20note.md#part"); got[0] != "archive/new note.md" {
		t.Fatalf("escaped destination no longer resolves: %v", got)
	}
}
