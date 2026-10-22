package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanRelative(t *testing.T) {
	bad := []string{
		"", ".", "..", "../escape.md", "a/../../b.md", "a//../b.md", "a/../b.md",
		"/abs/path.md", filepath.Join(string(filepath.Separator), "tmp", "x.md"),
	}
	for _, raw := range bad {
		if _, err := CleanRelative(raw); err == nil {
			t.Errorf("CleanRelative(%q) should fail", raw)
		}
	}
	for _, raw := range []string{"a.md", "sub/dir/doc.md", "deep/nested/dir/file.md"} {
		got, err := CleanRelative(raw)
		if err != nil {
			t.Errorf("CleanRelative(%q)=%v", raw, err)
			continue
		}
		if got != filepath.Clean(raw) {
			t.Errorf("CleanRelative(%q)=%q", raw, got)
		}
	}
}

func TestParseExcludePatternRejectsUnsafeOrMalformed(t *testing.T) {
	bad := []string{
		"", "   ", "/", "/abs/dir", "../escape", "a/../b", "a/./b", "a//b",
		"a**b", "x/**y", "[", "a/[/b",
	}
	for _, raw := range bad {
		if _, err := ParseExcludePattern(raw); err == nil {
			t.Errorf("ParseExcludePattern(%q) should fail", raw)
		}
	}
	for _, raw := range []string{
		"docs/tmp", "drafts/**", "**/*.draft.md", "a/*/b.md", "**/tmp",
		"note.draft.md", "docs/", "a/b?/c.md", "[ab]/c.md", "**",
	} {
		if _, err := ParseExcludePattern(raw); err != nil {
			t.Errorf("ParseExcludePattern(%q)=%v", raw, err)
		}
	}
}

func TestAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	target := filepath.Join("sub", "doc.md")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, target), []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := AtomicWrite(root, target, []byte("new content\n")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, target))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new content\n" {
		t.Fatalf("content=%q", got)
	}
	// The rename preserves the existing file's mode.
	info, err := os.Stat(filepath.Join(dir, target))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode=%o, want 640", info.Mode().Perm())
	}
	// No temp siblings left behind.
	entries, err := os.ReadDir(filepath.Join(dir, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".kbm-") {
			t.Fatalf("leftover temp file %s", entry.Name())
		}
	}
}

func TestAtomicWriteRejectsUnsafeTargets(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	// Missing target cannot be replaced atomically.
	if err := AtomicWrite(root, "absent.md", []byte("x")); err == nil {
		t.Error("missing target should fail")
	}
	// A symlink must not be followed — the guard is the write barrier
	// against redirecting a managed update outside the root.
	if err := os.WriteFile(filepath.Join(dir, "real.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.md", filepath.Join(dir, "link.md")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := AtomicWrite(root, "link.md", []byte("y")); err == nil {
		t.Error("symlink target should fail")
	}
	// Directories are not regular files.
	if err := AtomicWrite(root, ".", []byte("x")); err == nil {
		t.Error("directory target should fail")
	}
}

func TestExcludePatternsMatch(t *testing.T) {
	ps, err := ParseExcludePatterns([]string{
		"docs/tmp", "drafts/**", "**/*.draft.md", "a/*/c.md", "**/private",
	})
	if err != nil {
		t.Fatal(err)
	}
	// A pattern matches the path itself; the walk covers descendants because a
	// matching directory is pruned before descent (e.g. docs/tmp/x.md is never
	// visited once docs/tmp matches).
	matches := []string{
		"docs/tmp",
		"drafts", "drafts/a.md", "drafts/deep/b.md",
		"note.draft.md", "sub/note.draft.md",
		"a/b/c.md",
		"private", "a/b/private",
	}
	for _, rel := range matches {
		if !ps.Match(rel) {
			t.Errorf("expected %q to be excluded", rel)
		}
	}
	keeps := []string{
		"", "docs", "docs/keep.md", "docs/tmp/x.md", "other/tmp",
		"other/tmp/y.md", "drafts.md", "a/b/d.md", "a/b/c/x.md",
		"x.draft.md.bak", "private/inside.md",
	}
	for _, rel := range keeps {
		if ps.Match(rel) {
			t.Errorf("did not expect %q to be excluded", rel)
		}
	}
}
