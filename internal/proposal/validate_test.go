package proposal

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidateLinkRepair(t *testing.T) {
	original := "---\ntags: [go]\nkb_policy: {no_llm_context: true}\n---\n# Note\n\nSee [old](old.md#part).\n\n`[code](old.md)`\n"
	fixed := strings.Replace(original, "](old.md#part)", "](../archive/new.md#part)", 1)
	retarget := func(path string) (string, bool) { return "archive/new.md", path == "sub/old.md" }
	exists := func(path string) bool { return path == "archive/new.md" }
	for _, tt := range []struct {
		name, proposed string
		valid          bool
	}{
		{"unchanged body", original, true},
		{"verified repair", fixed, true},
		{"invented path", strings.Replace(fixed, "../archive/new.md", "../other.md", 1), false},
		{"deleted heading", strings.Replace(fixed, "# Note\n", "", 1), false},
		{"changed code", strings.Replace(fixed, "`[code](old.md)`", "`[code](new.md)`", 1), false},
		{"changed privacy", strings.Replace(fixed, "no_llm_context: true", "no_llm_context: false", 1), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateLinkRepair("sub/note.md", original, tt.proposed, retarget, exists)
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v err=%v", tt.valid, err)
			}
		})
	}
	if err := ValidateRewrite(original, fixed); err == nil {
		t.Fatal("model-authored body rewrites must remain disallowed")
	}
}

func TestValidateRewrite(t *testing.T) {
	for _, tt := range []struct {
		name     string
		original string
		proposed string
		wantErr  string
	}{
		{
			name: "additive expansion", original: "# Title\n\nKeep this.\n",
			proposed: "# Title\n\nAn introduction.\n\nKeep this.\n\nMore context.\n",
		},
		{
			name: "whitespace reflow", original: "# Title\n\nKeep this\nparagraph.\n",
			proposed: "# Title\n\nKeep this paragraph.\n",
		},
		{
			name: "metadata patch", original: "# Title\n\nKeep this.\n",
			proposed: "---\ntitle: Title\ntags: [go]\nrelated: [other.md]\n---\n# Title\n\nKeep this.\n",
		},
		{
			name: "preserve human metadata", original: "---\npublished: 2026-10-01\ncustom: original\n---\nBody\n",
			proposed: "---\npublished: 2026-10-01\ncustom: original\ntags: [go]\n---\nBody\n",
		},
		{
			name: "invented provenance", original: "Body",
			proposed: "---\npublished: 2024-04-05\n---\nBody", wantErr: `protected frontmatter field "published"`,
		},
		{
			name: "invented verification", original: "Body",
			proposed: "---\nreview_status: approved\n---\nBody", wantErr: `protected frontmatter field "review_status"`,
		},
		{
			name: "changed privacy", original: "---\nkb_policy:\n  no_llm_context: true\n---\nBody",
			proposed: "---\nkb_policy:\n  no_llm_context: false\n---\nBody", wantErr: `protected frontmatter field "kb_policy"`,
		},
		{
			name: "removed unknown field", original: "---\ncustom: keep\n---\nBody",
			proposed: "Body", wantErr: `protected frontmatter field "custom"`,
		},
		{
			name: "removed heading", original: "# Title\n\nBody",
			proposed: "Body", wantErr: "existing body text",
		},
		{
			name: "removed caveat padded with new prose", original: "# Title\n\nBody\n\nImportant caveat.",
			proposed: "# Title\n\nBody\n\n" + strings.Repeat("Extra words. ", 100), wantErr: "existing body text",
		},
		{
			name: "corrupted regex escape", original: "# Regex\n\nUse `value.sub(%r{/+\\z}, '')`.\n",
			proposed: "# Regex\n\nUse `value.sub(%r{/+oops}, '')`.\n", wantErr: "existing body text",
		},
		{
			name: "changed code indentation", original: "# Code\n\n```python\nif ready:\n    run()\n```\n",
			proposed: "# Code\n\n```python\nif ready:\nrun()\n```\n", wantErr: "code span or block",
		},
		{
			name: "changed inline code whitespace", original: "Use `a + b` now.",
			proposed: "Use `a  + b` now.", wantErr: "code span or block",
		},
		{
			name: "preserved tilde code", original: "~~~sh\nprintf ok\n~~~\n",
			proposed: "Example:\n~~~sh\nprintf ok\n~~~\n\nExplanation.\n",
		},
		{
			name: "CRLF normalization", original: "```sh\r\nprintf ok\r\n```\r\n",
			proposed: "```sh\nprintf ok\n```\n\nExplanation.\n",
		},
		{
			name: "invalid frontmatter value", original: "Body",
			proposed: "---\ntags: [GO]\n---\nBody", wantErr: "invalid proposed frontmatter",
		},
		{
			name: "malformed frontmatter", original: "Body",
			proposed: "---\ntitle: [\n---\nBody", wantErr: "proposed frontmatter",
		},
		{
			name: "empty proposal", original: "Body",
			proposed: " ", wantErr: "missing proposed_content",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRewrite(tt.original, tt.proposed)
			if tt.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("error=%v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateMove(t *testing.T) {
	for _, tt := range []struct {
		source string
		target string
		valid  bool
	}{
		{"frameworks/shell/README.md", "patterns/shell.md", true},
		{"frameworks/shell/README.md", "frameworks/workflows/ci.yml", false},
		{"frameworks/shell/README.md", "gundestrup/tools/Shell/test-helpers.sh", false},
		{"a.md", "a.md", false},
		{"a.md", "", false},
		{"a.md", "../outside.md", false},
	} {
		t.Run(tt.target, func(t *testing.T) {
			if err := ValidateMove(tt.source, tt.target); (err == nil) != tt.valid {
				t.Fatalf("ValidateMove(%q, %q)=%v", tt.source, tt.target, err)
			}
		})
	}
}

func TestValidateLinks(t *testing.T) {
	lookup := func(path string) error {
		if path == "known.md" {
			return nil
		}
		return fmt.Errorf("unknown document")
	}
	for _, tt := range []struct {
		link  string
		valid bool
	}{
		{"known.md", true}, {"missing.md", false},
		{"../known.md", false}, {"dir/../known.md", false}, {"/known.md", false},
	} {
		t.Run(tt.link, func(t *testing.T) {
			content := "---\nrelated: [\"" + tt.link + "\"]\n---\nBody\n"
			if err := ValidateLinks(content, lookup); (err == nil) != tt.valid {
				t.Fatalf("ValidateLinks(%q)=%v", tt.link, err)
			}
		})
	}
}

func TestCodeSpansMatchFenceLengthsAndInlineRuns(t *testing.T) {
	body := "````md\n```sh\ncommand\n```\n````\nUse ``a ` b`` and `x`.\n"
	got := codeSpans(body)
	if len(got) != 3 || got[1] != "``a ` b``" || got[2] != "`x`" {
		t.Fatalf("spans=%q", got)
	}
}
